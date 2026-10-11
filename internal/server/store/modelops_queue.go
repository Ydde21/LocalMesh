package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ---------------------------------------------------------------------------
// Model-operation scheduling queue (migration 0005; dedupe hardened 0014)
//
// These queries back the modelops dispatcher: ops are persisted 'queued'
// first and claimed atomically (FOR UPDATE SKIP LOCKED) so a future second
// poller cannot double-dispatch. OpState extends ModelOperation with the
// scheduling columns; the embedded struct keeps JSON shape compatible with
// the existing admin API.
//
// Dedupe: model_operations_active_uniq covers the WHOLE active lifecycle
// — ('queued','dispatched','running','interrupted'). 'interrupted' is in
// the set since 0014: an op parked between interruption and the resume
// sweep must still block resubmission, or a second live op lands on the
// same (node, model) and both end up dispatched.
// ---------------------------------------------------------------------------

// OpState is a model_operations row including the queue columns.
type OpState struct {
	ModelOperation
	Attempt       int        `json:"attempt"`
	MaxAttempts   int        `json:"max_attempts"`
	NextAttemptAt *time.Time `json:"next_attempt_at,omitempty"`
	LastError     string     `json:"last_error,omitempty"`
	Phase         string     `json:"phase,omitempty"`
	RateBPS       int64      `json:"rate_bps"`
	Indeterminate bool       `json:"indeterminate"`
	Priority      int        `json:"priority"`
	FinishedAt    *time.Time `json:"finished_at,omitempty"`
}

// Column order consumed by scanOpState — node_name position 3.
const modelOpCols = `o.id,o.node_id,COALESCE(n.name,''),o.model,o.op,o.status,o.detail,
	o.completed,o.total,o.error,o.created_at,o.updated_at,
	o.attempt,o.max_attempts,o.next_attempt_at,COALESCE(o.last_error,''),o.phase,
	o.rate_bps,o.indeterminate,o.priority,o.finished_at`

func scanOpState(row pgx.Row) (*OpState, error) {
	var s OpState
	err := row.Scan(&s.ID, &s.NodeID, &s.NodeName, &s.Model, &s.Op, &s.Status,
		&s.Detail, &s.Completed, &s.Total, &s.Error, &s.CreatedAt, &s.UpdatedAt,
		&s.Attempt, &s.MaxAttempts, &s.NextAttemptAt, &s.LastError, &s.Phase,
		&s.RateBPS, &s.Indeterminate, &s.Priority, &s.FinishedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &s, nil
}

// EnqueueModelOp persists a new op in 'queued' status, due immediately.
// The partial unique index maps a conflicting non-terminal op on the
// same node+model — queued, dispatched, running, OR interrupted — to
// ErrActiveModelOp.
func (d *DB) EnqueueModelOp(ctx context.Context, nodeID, model, op string, priority, maxAttempts int) (*ModelOperation, error) {
	var o ModelOperation
	err := d.Pool.QueryRow(ctx,
		`INSERT INTO model_operations (node_id,model,op,status,priority,max_attempts,next_attempt_at)
		 VALUES ($1,$2,$3,'queued',$4,$5,now())
		 RETURNING id,node_id,model,op,status,detail,completed,total,error,created_at,updated_at`,
		nodeID, model, op, priority, maxAttempts).Scan(&o.ID, &o.NodeID, &o.Model, &o.Op,
		&o.Status, &o.Detail, &o.Completed, &o.Total, &o.Error, &o.CreatedAt, &o.UpdatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, ErrActiveModelOp
		}
		return nil, err
	}
	return &o, nil
}

// ActiveModelOpFor returns the op currently covering a node+model —
// queued/dispatched/running OR interrupted (the whole
// model_operations_active_uniq set) — used to describe a dedupe
// conflict. Post-0014 at most one such row exists; the ORDER BY only
// matters for data written before the index closed the 'interrupted'
// gap, and prefers surfacing the interrupted row since it is the one an
// operator must resolve.
func (d *DB) ActiveModelOpFor(ctx context.Context, nodeID, model string) (*ModelOperation, error) {
	var o ModelOperation
	err := d.Pool.QueryRow(ctx,
		`SELECT id,node_id,model,op,status,detail,completed,total,error,created_at,updated_at
		 FROM model_operations
		 WHERE node_id=$1 AND model=$2 AND status IN ('queued','dispatched','running','interrupted')
		 ORDER BY (status='interrupted') DESC, created_at DESC, id DESC
		 LIMIT 1`,
		nodeID, model).Scan(&o.ID, &o.NodeID, &o.Model, &o.Op, &o.Status,
		&o.Detail, &o.Completed, &o.Total, &o.Error, &o.CreatedAt, &o.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &o, nil
}

// GetModelOpState fetches one op with its scheduling columns.
func (d *DB) GetModelOpState(ctx context.Context, id string) (*OpState, error) {
	return scanOpState(d.Pool.QueryRow(ctx,
		`SELECT `+modelOpCols+` FROM model_operations o
		 LEFT JOIN nodes n ON n.id=o.node_id WHERE o.id=$1`, id))
}

// QueuedModelOpCount is the current queue backlog (QueueMax enforcement).
func (d *DB) QueuedModelOpCount(ctx context.Context) (int, error) {
	var c int
	err := d.Pool.QueryRow(ctx,
		`SELECT count(*) FROM model_operations WHERE status='queued'`).Scan(&c)
	return c, err
}

// ModelOpsActiveByNode counts in-flight (dispatched/running) ops per node —
// the per-node concurrency gate.
func (d *DB) ModelOpsActiveByNode(ctx context.Context) (map[string]int, error) {
	rows, err := d.Pool.Query(ctx,
		`SELECT node_id, count(*) FROM model_operations
		 WHERE status IN ('dispatched','running') GROUP BY node_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var id string
		var c int
		if err := rows.Scan(&id, &c); err != nil {
			return nil, err
		}
		out[id] = c
	}
	return out, rows.Err()
}

// ModelOpWorkNodes lists nodes holding any non-terminal op — the set whose
// connectivity the dispatch loop must track.
func (d *DB) ModelOpWorkNodes(ctx context.Context) ([]string, error) {
	rows, err := d.Pool.Query(ctx,
		`SELECT DISTINCT node_id FROM model_operations
		 WHERE status IN ('queued','dispatched','running')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ClaimDueModelOps atomically transitions up to `limit` due queued ops on
// the given nodes to 'dispatched', consuming one attempt each. The due set
// is ranked per node (priority, then age) and interleaved by that rank so
// one node's backlog cannot starve others out of a small `limit`. Each op
// claims at most perNode of its node's queue. FOR UPDATE SKIP LOCKED makes
// the claim safe under concurrent pollers: a second claimant skips locked
// rows instead of double-dispatching.
//
// epoch >= 0 adds a fencing check: the claim matches zero rows unless
// control_plane_lock.epoch equals it — a process that lost ownership
// cannot claim work even before it notices. epoch < 0 disables the
// check (standalone/test use with no ownership session).
//
// Nodes in any maintenance state (draining/maintenance/updating) are
// never due — their queued ops wait until the node resumes service.
func (d *DB) ClaimDueModelOps(ctx context.Context, epoch int64, nodeIDs []string, perNode, limit int) ([]*OpState, error) {
	if len(nodeIDs) == 0 || limit <= 0 || perNode <= 0 {
		return nil, nil
	}
	rows, err := d.Pool.Query(ctx,
		`WITH due AS (
			SELECT o.id, o.node_id,
				ROW_NUMBER() OVER (PARTITION BY o.node_id ORDER BY o.priority DESC, o.created_at) AS rn
			FROM model_operations o
			JOIN nodes n ON n.id = o.node_id AND n.maintenance = ''
			WHERE o.status='queued'
			  AND (o.next_attempt_at IS NULL OR o.next_attempt_at <= now())
			  AND o.node_id = ANY($1::uuid[])
			  AND ($4::bigint < 0 OR
			       (SELECT epoch FROM control_plane_lock WHERE id = 1) = $4)
		),
		locked AS (
			SELECT o.id FROM model_operations o JOIN due ON due.id = o.id
			WHERE due.rn <= $3
			ORDER BY due.rn, o.created_at
			LIMIT $2
			FOR UPDATE OF o SKIP LOCKED
		)
		UPDATE model_operations o
		SET status='dispatched', attempt=o.attempt+1, updated_at=now()
		FROM locked WHERE o.id = locked.id
		RETURNING o.id,o.node_id,'',o.model,o.op,o.status,o.detail,
			o.completed,o.total,o.error,o.created_at,o.updated_at,
			o.attempt,o.max_attempts,o.next_attempt_at,COALESCE(o.last_error,''),o.phase,
			o.rate_bps,o.indeterminate,o.priority,o.finished_at`,
		nodeIDs, limit, perNode, epoch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*OpState
	for rows.Next() {
		s, err := scanOpState(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ReleaseModelOpClaim returns a claimed-but-never-sent op to 'queued' so it
// is reconsidered on the next pass (e.g. its node saturated or dropped
// between claim and send). Does not consume an attempt — nothing ran.
func (d *DB) ReleaseModelOpClaim(ctx context.Context, id string) error {
	_, err := d.Pool.Exec(ctx,
		`UPDATE model_operations SET status='queued', updated_at=now()
		 WHERE id=$1 AND status='dispatched'`, id)
	return err
}

// RequeueModelOp returns a dispatched/running op to 'queued' for a retry.
// The attempt was already consumed at claim time; this is allowed only
// while consumed attempts remain under max_attempts — returns false when
// the op is no longer active or is exhausted, and the caller then fails
// or drops it.
func (d *DB) RequeueModelOp(ctx context.Context, id string, nextAt time.Time, lastErr string) (bool, error) {
	tag, err := d.Pool.Exec(ctx,
		`UPDATE model_operations SET status='queued',
			next_attempt_at=$2, last_error=$3, updated_at=now()
		 WHERE id=$1 AND status IN ('dispatched','running') AND attempt < max_attempts`,
		id, nextAt, lastErr)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// FailModelOp terminally fails an op, recording the error in both the
// user-facing error column and the retry bookkeeping column.
func (d *DB) FailModelOp(ctx context.Context, id, errStr string) error {
	_, err := d.Pool.Exec(ctx,
		`UPDATE model_operations SET status='failed', error=$2, last_error=$2,
			finished_at=now(), updated_at=now()
		 WHERE id=$1 AND status IN ('queued','dispatched','running','interrupted')`,
		id, errStr)
	return err
}

// MarkModelOpInterrupted moves an active op to 'interrupted' — used when a
// delete's dispatch outcome is unknowable (the frame may have reached the
// agent) and auto-retry must not fire a second delete.
func (d *DB) MarkModelOpInterrupted(ctx context.Context, id, reason string) error {
	_, err := d.Pool.Exec(ctx,
		`UPDATE model_operations SET status='interrupted', error=$2, last_error=$2, updated_at=now()
		 WHERE id=$1 AND status IN ('dispatched','running')`, id, reason)
	return err
}

// CancelQueuedModelOp cancels an op still waiting in the queue — it never
// reaches an agent. Returns false when the op already left 'queued'.
// CancelQueuedModelOp finalizes an op that no agent may be executing:
// 'queued' (never dispatched) or 'interrupted' (node lost, awaiting the
// resume sweep — without this a cancel in the interrupt window would
// 409 yet still retry later). Returns false when the op already raced
// to dispatched/running/terminal.
func (d *DB) CancelQueuedModelOp(ctx context.Context, id string) (bool, error) {
	tag, err := d.Pool.Exec(ctx,
		`UPDATE model_operations SET status='cancelled', error='cancelled by administrator',
			finished_at=now(), updated_at=now()
		 WHERE id=$1 AND status IN ('queued','interrupted')`, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// UpdateModelOpProgress records an agent progress frame: verbatim engine
// phase, byte counters, measured rate, and the indeterminate flag. Guarded
// to active statuses so a late frame on a finished op is dropped.
func (d *DB) UpdateModelOpProgress(ctx context.Context, id, phase string, completed, total, rateBPS int64, indeterminate bool) error {
	_, err := d.Pool.Exec(ctx,
		`UPDATE model_operations SET status='running', detail=$2, phase=$2,
			completed=$3, total=$4, rate_bps=$5, indeterminate=$6, updated_at=now()
		 WHERE id=$1 AND status IN ('dispatched','running')`,
		id, phase, completed, total, rateBPS, indeterminate)
	return err
}

// FinishModelOp finalizes an active op with terminal status plus the final
// progress measurement carried by OpDone. Guarded to active statuses —
// late completions on cancelled/interrupted rows are dropped.
func (d *DB) FinishModelOp(ctx context.Context, id, status, errStr string,
	completed, total, rateBPS int64, phase string, indeterminate bool) error {
	_, err := d.Pool.Exec(ctx,
		`UPDATE model_operations SET status=$2, error=$3, completed=$4, total=$5,
			rate_bps=$6, phase=$7, indeterminate=$8, finished_at=now(), updated_at=now()
		 WHERE id=$1 AND status IN ('dispatched','running')`,
		id, status, errStr, completed, total, rateBPS, phase, indeterminate)
	return err
}

// InterruptActiveModelOps marks every in-flight op interrupted — boot-time
// reconcile for ops orphaned by a process restart.
func (d *DB) InterruptActiveModelOps(ctx context.Context, reason string) (int64, error) {
	tag, err := d.Pool.Exec(ctx,
		`UPDATE model_operations SET status='interrupted', error=$1, updated_at=now()
		 WHERE status IN ('dispatched','running')`, reason)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// ResumeInterruptedPulls requeues interrupted pulls on a node (empty
// nodeID = all nodes) with attempts remaining, scheduling each retry at
// now + base*2^attempt with ±20% jitter (attempt already counts the
// interrupted dispatch — interrupt→resume preserves the consumed
// attempt). Deletes are NEVER resumed — a delete's outcome after
// interruption is unknown, so it stays parked for a human.
//
// The NOT EXISTS guard is defensive conflict-safety: a row only resumes
// when no OTHER live row covers the same node+model. Migration 0014
// makes such pairs impossible (and cleans legacy ones), so the guard
// can never trigger on a migrated database — but if it somehow did, the
// row parks 'interrupted' for an operator rather than crashing the
// whole sweep on a 23505. Returns the requeued op ids.
func (d *DB) ResumeInterruptedPulls(ctx context.Context, nodeID string, baseSecs float64) ([]string, error) {
	rows, err := d.Pool.Query(ctx,
		`UPDATE model_operations o SET status='queued',
			last_error=o.error, error='',
			next_attempt_at=now() + make_interval(secs => $1 * power(2, LEAST(o.attempt,8)) * (0.8 + 0.4*random())),
			updated_at=now()
		 WHERE o.op='pull' AND o.status='interrupted' AND o.attempt < o.max_attempts
		   AND (CAST(NULLIF($2,'') AS uuid) IS NULL OR o.node_id = CAST(NULLIF($2,'') AS uuid))
		   AND NOT EXISTS (SELECT 1 FROM model_operations x
		                   WHERE x.node_id = o.node_id AND x.model = o.model
		                     AND x.id <> o.id
		                     AND x.status IN ('queued','dispatched','running','interrupted'))
		 RETURNING o.id`,
		baseSecs, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// RetryModelOp manually requeues an 'interrupted' op — the operator
// resolution path for work parked on an uncertain outcome (deletes are
// never auto-resumed; a pull interrupted past its sweep may also park).
// The op keeps its identity and consumed attempts: this is a retry of
// the SAME operation, not a new submission, so next_attempt_at=now()
// makes it due on the next dispatch pass while attempt/max_attempts
// bookkeeping stays honest. Does NOT gate on attempt — an operator may
// deliberately re-drive an exhausted op; the next claim consumes an
// attempt as usual and normal bookkeeping applies from there.
//
// epoch >= 0 fences the mutation like ClaimDueModelOps: a superseded
// control plane cannot re-arm work a successor owns. Returns false when
// the op is not currently 'interrupted' or the epoch no longer matches —
// callers re-read the row to report the honest state. A 23505 from the
// unique index (a second live row appeared for the same node+model)
// maps to ErrActiveModelOp: unreachable post-0014, but the mapping
// keeps the failure deterministic instead of leaking a pg error.
func (d *DB) RetryModelOp(ctx context.Context, id string, epoch int64) (bool, error) {
	tag, err := d.Pool.Exec(ctx,
		`UPDATE model_operations SET status='queued',
			last_error=error, error='',
			next_attempt_at=now(), updated_at=now()
		 WHERE id=$1 AND status='interrupted'
		   AND ($2::bigint < 0 OR
		        (SELECT epoch FROM control_plane_lock WHERE id = 1) = $2)`,
		id, epoch)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return false, ErrActiveModelOp
		}
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// FailExhaustedInterruptedPulls terminally fails interrupted pulls that
// consumed all attempts.
func (d *DB) FailExhaustedInterruptedPulls(ctx context.Context, nodeID string) (int64, error) {
	tag, err := d.Pool.Exec(ctx,
		`UPDATE model_operations SET status='failed', last_error=error,
			finished_at=now(), updated_at=now()
		 WHERE op='pull' AND status='interrupted' AND attempt >= max_attempts
		   AND (CAST(NULLIF($1,'') AS uuid) IS NULL OR node_id = CAST(NULLIF($1,'') AS uuid))`,
		nodeID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// DueNowNodeModelOps expedites a node's queued ops — called when a node
// reconnects so resumed work doesn't sit out its full backoff.
func (d *DB) DueNowNodeModelOps(ctx context.Context, nodeID string) error {
	_, err := d.Pool.Exec(ctx,
		`UPDATE model_operations SET next_attempt_at=now(), updated_at=now()
		 WHERE node_id=$1 AND status='queued' AND next_attempt_at > now()`, nodeID)
	return err
}

// ListQueuedModelOps returns the current queue backlog, next-due first.
func (d *DB) ListQueuedModelOps(ctx context.Context, limit int) ([]*OpState, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := d.Pool.Query(ctx,
		`SELECT `+modelOpCols+` FROM model_operations o
		 LEFT JOIN nodes n ON n.id=o.node_id
		 WHERE o.status='queued'
		 ORDER BY o.priority DESC, o.created_at LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*OpState
	for rows.Next() {
		s, err := scanOpState(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ListModelOpStates returns the recent full op view (queue + progress +
// retry fields) — the shape the admin API and dashboard consume.
func (d *DB) ListModelOpStates(ctx context.Context, limit int) ([]*OpState, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := d.Pool.Query(ctx,
		`SELECT `+modelOpCols+` FROM model_operations o
		 LEFT JOIN nodes n ON n.id=o.node_id
		 ORDER BY o.created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*OpState
	for rows.Next() {
		s, err := scanOpState(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
