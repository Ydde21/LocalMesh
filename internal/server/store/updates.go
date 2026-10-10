package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ---------------------------------------------------------------------------
// Agent update orchestration (migration 0010)
//
// agent_updates rows are the durable state of one node's move to one
// release; rollouts group them into a campaign. Phase transitions are
// guarded UPDATEs — late agent frames on terminal rows and double-dispatch
// are dropped by the guards, mirroring modelops_queue.go semantics.
// The dispatch claim carries the ownership epoch as a fencing token.
// ---------------------------------------------------------------------------

// Update phases. Phases before succeeded/failed/rolled_back/cancelled/
// interrupted are "live" (finished_at IS NULL).
const (
	UpdPending      = "pending"    // created, awaiting rollout slot
	UpdDraining     = "draining"   // node draining workloads pre-update
	UpdDispatched   = "dispatched" // frame sent, awaiting agent report
	UpdVerifying    = "verifying"  // agent: signature/checksum/protocol
	UpdDownloading  = "downloading"
	UpdStaging      = "staging"
	UpdInstalling   = "installing"
	UpdRestarting   = "restarting"    // binary swapped, agent bouncing
	UpdVerifyHealth = "verify_health" // reconnected; version+health check
	UpdSucceeded    = "succeeded"
	UpdFailed       = "failed"
	UpdRolledBack   = "rolled_back"
	UpdCancelled    = "cancelled"
	UpdInterrupted  = "interrupted"
)

// Rollout states.
const (
	RolloutPending          = "pending"
	RolloutRunning          = "running"
	RolloutAwaitingApproval = "awaiting_approval" // canary done; next batch needs admin OK
	RolloutSucceeded        = "succeeded"
	RolloutFailed           = "failed"
	RolloutCancelled        = "cancelled"
)

// ErrActiveUpdate maps the partial unique index — a node already has an
// unfinished update row.
var ErrActiveUpdate = errors.New("an agent update is already in progress on this node")

// UpdTerminal reports whether a phase is final.
func UpdTerminal(phase string) bool {
	switch phase {
	case UpdSucceeded, UpdFailed, UpdRolledBack, UpdCancelled, UpdInterrupted:
		return true
	}
	return false
}

// AgentUpdate is one agent_updates row.
type AgentUpdate struct {
	ID           string     `json:"id"`
	RolloutID    string     `json:"rollout_id,omitempty"`
	ReleaseID    string     `json:"release_id"`
	NodeID       string     `json:"node_id"`
	NodeName     string     `json:"node_name,omitempty"`
	ReleaseVer   string     `json:"release_version,omitempty"`
	Phase        string     `json:"phase"`
	Attempt      int        `json:"attempt"`
	Error        string     `json:"error,omitempty"`
	Detail       string     `json:"detail,omitempty"`
	FromVersion  string     `json:"from_version,omitempty"`
	ToVersion    string     `json:"to_version,omitempty"`
	Epoch        int64      `json:"epoch"`
	CreatedAt    time.Time  `json:"created_at"`
	DispatchedAt *time.Time `json:"dispatched_at,omitempty"`
	UpdatedAt    time.Time  `json:"updated_at"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
}

// Rollout is one campaign row.
type Rollout struct {
	ID          string    `json:"id"`
	ReleaseID   string    `json:"release_id"`
	ReleaseVer  string    `json:"release_version,omitempty"`
	Strategy    string    `json:"strategy"`
	State       string    `json:"state"`
	CanaryCount int       `json:"canary_count"`
	BatchSize   int       `json:"batch_size"`
	Error       string    `json:"error,omitempty"`
	CreatedBy   string    `json:"created_by"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	// Counts populated by loadRolloutCounts.
	Total     int `json:"total"`
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
	Pending   int `json:"pending"`
}

const updateCols = `u.id,COALESCE(u.rollout_id::text,''),u.release_id,u.node_id,
	COALESCE(n.name,''),COALESCE(r.version,''),u.phase,u.attempt,u.error,u.detail,
	u.from_version,u.to_version,u.epoch,u.created_at,u.dispatched_at,u.updated_at,u.finished_at`

const updateJoin = ` FROM agent_updates u
	LEFT JOIN nodes n ON n.id=u.node_id
	LEFT JOIN releases r ON r.id=u.release_id`

func scanUpdate(row pgx.Row) (*AgentUpdate, error) {
	var u AgentUpdate
	err := row.Scan(&u.ID, &u.RolloutID, &u.ReleaseID, &u.NodeID, &u.NodeName,
		&u.ReleaseVer, &u.Phase, &u.Attempt, &u.Error, &u.Detail,
		&u.FromVersion, &u.ToVersion, &u.Epoch, &u.CreatedAt, &u.DispatchedAt,
		&u.UpdatedAt, &u.FinishedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &u, nil
}

// ---------------------------------------------------------------------------
// agent_updates
// ---------------------------------------------------------------------------

// EnqueueAgentUpdate creates a pending update row for one node and
// stamps to_version from the release row. The partial unique index on
// live (node_id) turns a second in-flight update into ErrActiveUpdate.
func (d *DB) EnqueueAgentUpdate(ctx context.Context, rolloutID, releaseID, nodeID, fromVersion string) (*AgentUpdate, error) {
	var rollout any
	if rolloutID != "" {
		rollout = rolloutID
	}
	var id string
	err := d.Pool.QueryRow(ctx,
		`INSERT INTO agent_updates (rollout_id,release_id,node_id,phase,from_version,to_version)
		 SELECT $1,$2,$3,'pending',$4,r.version FROM releases r WHERE r.id=$2
		 RETURNING id`, rollout, releaseID, nodeID, fromVersion).Scan(&id)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, ErrActiveUpdate
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound // unknown release id
		}
		return nil, err
	}
	return d.GetAgentUpdate(ctx, id)
}

func (d *DB) GetAgentUpdate(ctx context.Context, id string) (*AgentUpdate, error) {
	return scanUpdate(d.Pool.QueryRow(ctx,
		`SELECT `+updateCols+updateJoin+` WHERE u.id=$1`, id))
}

// ListAgentUpdates returns update rows, newest first; optional rollout
// and phase filters. Limit clamps to [1,500].
func (d *DB) ListAgentUpdates(ctx context.Context, rolloutID, phase string, limit int) ([]*AgentUpdate, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	q := `SELECT ` + updateCols + updateJoin + ` WHERE true`
	var args []any
	if rolloutID != "" {
		args = append(args, rolloutID)
		q += fmt.Sprintf(` AND u.rollout_id=$%d`, len(args))
	}
	if phase != "" {
		args = append(args, phase)
		q += fmt.Sprintf(` AND u.phase=$%d`, len(args))
	}
	args = append(args, limit)
	q += fmt.Sprintf(` ORDER BY u.created_at DESC LIMIT $%d`, len(args))
	rows, err := d.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*AgentUpdate{}
	for rows.Next() {
		u, err := scanUpdate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// LiveUpdateForNode returns the node's unfinished update row, or
// ErrNotFound.
func (d *DB) LiveUpdateForNode(ctx context.Context, nodeID string) (*AgentUpdate, error) {
	return scanUpdate(d.Pool.QueryRow(ctx,
		`SELECT `+updateCols+updateJoin+` WHERE u.node_id=$1 AND u.finished_at IS NULL`,
		nodeID))
}

// ClaimPendingUpdate transitions pending→draining for the update's node
// under the fencing epoch — the update orchestrator's dispatch step.
// Returns false when the row is not pending or was claimed by a stale
// epoch.
func (d *DB) ClaimPendingUpdate(ctx context.Context, id string, epoch int64) (bool, error) {
	tag, err := d.Pool.Exec(ctx,
		`UPDATE agent_updates u SET phase='draining', epoch=$2, attempt=u.attempt+1,
			updated_at=now()
		 WHERE u.id=$1 AND u.phase='pending' AND u.finished_at IS NULL
		   AND EXISTS (SELECT 1 FROM control_plane_lock c WHERE c.id=1 AND c.epoch=$2)`,
		id, epoch)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// SetUpdatePhaseFrom moves an update between the agent-driven phases.
// Allowed sources keep late/duplicate frames honest: a terminal row or a
// phase the agent never passed ignores the write. dispatched_at is
// stamped when the row first leaves pending/draining.
func (d *DB) SetUpdatePhase(ctx context.Context, id, phase, detail, errStr string) (bool, error) {
	tag, err := d.Pool.Exec(ctx,
		`UPDATE agent_updates SET phase=$2, detail=$3, error=$4,
			updated_at=now(),
			dispatched_at=COALESCE(dispatched_at, CASE WHEN $2 <> 'pending' THEN now() END)
		 WHERE id=$1 AND finished_at IS NULL`, id, phase, detail, errStr)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// FinishUpdate lands an update in a terminal phase (succeeded, failed,
// rolled_back, cancelled, interrupted) with finished_at set.
func (d *DB) FinishUpdate(ctx context.Context, id, phase, errStr, detail string) (bool, error) {
	if !UpdTerminal(phase) {
		return false, fmt.Errorf("phase %q is not terminal", phase)
	}
	tag, err := d.Pool.Exec(ctx,
		`UPDATE agent_updates SET phase=$2, error=$3, detail=$4,
			updated_at=now(), finished_at=now()
		 WHERE id=$1 AND finished_at IS NULL`, id, phase, errStr, detail)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// RevertUpdate puts an update back to pending (claim released because
// the drain couldn't proceed — e.g. node went offline). Clears the
// fencing epoch so the next claim re-fences.
func (d *DB) RevertUpdate(ctx context.Context, id string) error {
	_, err := d.Pool.Exec(ctx,
		`UPDATE agent_updates SET phase='pending', epoch=-1, updated_at=now()
		 WHERE id=$1 AND phase='draining' AND finished_at IS NULL`, id)
	return err
}

// LiveUpdatesByPhase lists unfinished updates in the given phases — the
// orchestrator's work set each pass.
func (d *DB) LiveUpdatesByPhase(ctx context.Context, phases ...string) ([]*AgentUpdate, error) {
	if len(phases) == 0 {
		return nil, nil
	}
	rows, err := d.Pool.Query(ctx,
		`SELECT `+updateCols+updateJoin+` WHERE u.finished_at IS NULL AND u.phase = ANY($1)
		 ORDER BY u.created_at`, phases)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*AgentUpdate{}
	for rows.Next() {
		u, err := scanUpdate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// rollouts
// ---------------------------------------------------------------------------

const rolloutCols = `id,release_id,strategy,state,canary_count,batch_size,
	error,created_by,created_at,updated_at`

func scanRollout(row pgx.Row) (*Rollout, error) {
	var r Rollout
	err := row.Scan(&r.ID, &r.ReleaseID, &r.Strategy, &r.State, &r.CanaryCount,
		&r.BatchSize, &r.Error, &r.CreatedBy, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &r, nil
}

// CreateRollout inserts the campaign row in 'pending'.
func (d *DB) CreateRollout(ctx context.Context, releaseID, strategy string, canary, batch int, createdBy string) (*Rollout, error) {
	return scanRollout(d.Pool.QueryRow(ctx,
		`INSERT INTO rollouts (release_id,strategy,canary_count,batch_size,created_by)
		 VALUES ($1,$2,$3,$4,$5) RETURNING `+rolloutCols,
		releaseID, strategy, canary, batch, createdBy))
}

// GetRollout returns one rollout with live update counts.
func (d *DB) GetRollout(ctx context.Context, id string) (*Rollout, error) {
	r, err := scanRollout(d.Pool.QueryRow(ctx,
		`SELECT `+rolloutCols+` FROM rollouts WHERE id=$1`, id))
	if err != nil {
		return nil, err
	}
	var rel string
	if err := d.Pool.QueryRow(ctx,
		`SELECT version FROM releases WHERE id=$1`, r.ReleaseID).Scan(&rel); err == nil {
		r.ReleaseVer = rel
	}
	if err := d.loadRolloutCounts(ctx, r); err != nil {
		return nil, err
	}
	return r, nil
}

// ListRollouts returns campaigns, newest first, with update counts.
func (d *DB) ListRollouts(ctx context.Context, limit int) ([]*Rollout, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := d.Pool.Query(ctx,
		`SELECT `+rolloutCols+` FROM rollouts ORDER BY created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Rollout{}
	for rows.Next() {
		r, err := scanRollout(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	for _, r := range out {
		var rel string
		if err := d.Pool.QueryRow(ctx,
			`SELECT version FROM releases WHERE id=$1`, r.ReleaseID).Scan(&rel); err == nil {
			r.ReleaseVer = rel
		}
		if err := d.loadRolloutCounts(ctx, r); err != nil {
			return nil, err
		}
	}
	return out, rows.Err()
}

func (d *DB) loadRolloutCounts(ctx context.Context, r *Rollout) error {
	return d.Pool.QueryRow(ctx,
		`SELECT count(*),
			count(*) FILTER (WHERE phase='succeeded'),
			count(*) FILTER (WHERE phase IN ('failed','rolled_back')),
			count(*) FILTER (WHERE finished_at IS NULL)
		 FROM agent_updates WHERE rollout_id=$1`, r.ID).
		Scan(&r.Total, &r.Succeeded, &r.Failed, &r.Pending)
}

// SetRolloutState transitions a campaign row; returns false when the
// rollout was already terminal.
func (d *DB) SetRolloutState(ctx context.Context, id, state, errStr string) (bool, error) {
	tag, err := d.Pool.Exec(ctx,
		`UPDATE rollouts SET state=$2, error=$3, updated_at=now()
		 WHERE id=$1 AND state NOT IN ('succeeded','failed','cancelled')`,
		id, state, errStr)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}
