package store

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/localmesh/localmesh/internal/contracts"
)

// ---------------------------------------------------------------------------
// Workloads — metadata only. Prompts and responses are never stored.
// ---------------------------------------------------------------------------

type Workload struct {
	ID               string              `json:"id"`
	Model            string              `json:"model"`
	NodeID           *string             `json:"node_id,omitempty"`
	NodeName         string              `json:"node_name,omitempty"`
	APIKeyID         *string             `json:"api_key_id,omitempty"`
	Status           string              `json:"status"`
	Streaming        bool                `json:"streaming"`
	PromptChars      int                 `json:"prompt_chars"`
	Decision         *contracts.Decision `json:"decision,omitempty"`
	Error            string              `json:"error,omitempty"`
	PromptTokens     *int                `json:"prompt_tokens,omitempty"`
	CompletionTokens *int                `json:"completion_tokens,omitempty"`
	TTFTms           *int                `json:"ttft_ms,omitempty"`
	TotalMS          *int                `json:"total_ms,omitempty"`
	QueuedAt         time.Time           `json:"queued_at"`
	StartedAt        *time.Time          `json:"started_at,omitempty"`
	FinishedAt       *time.Time          `json:"finished_at,omitempty"`
}

func (d *DB) CreateWorkload(ctx context.Context, w *Workload) error {
	var dec []byte
	if w.Decision != nil {
		dec, _ = json.Marshal(w.Decision)
	}
	return d.Pool.QueryRow(ctx,
		`INSERT INTO workloads (id, model, api_key_id, streaming, prompt_chars, decision)
		 VALUES ($1,$2,$3,$4,$5,$6) RETURNING queued_at`,
		w.ID, w.Model, w.APIKeyID, w.Streaming, w.PromptChars, dec).Scan(&w.QueuedAt)
}

func (d *DB) SetWorkloadRunning(ctx context.Context, id, nodeID string, dec *contracts.Decision) error {
	decJ, _ := json.Marshal(dec)
	_, err := d.Pool.Exec(ctx,
		`UPDATE workloads SET status='running', node_id=$2, started_at=now(), decision=$3
		 WHERE id=$1`, id, nodeID, decJ)
	return err
}

func (d *DB) FinishWorkload(ctx context.Context, id, status, errStr string, promptTok, compTok, ttft, total *int) error {
	_, err := d.Pool.Exec(ctx,
		`UPDATE workloads SET status=$2, error=$3, prompt_tokens=$4,
			completion_tokens=$5, ttft_ms=$6, total_ms=$7, finished_at=now()
		 WHERE id=$1`, id, status, errStr, promptTok, compTok, ttft, total)
	return err
}

// FailOrphanedWorkloads marks every queued/running workload failed.
// Called at startup (a restart abandons all in-flight work — connections,
// reservations and goroutines are gone) and during graceful shutdown.
func (d *DB) FailOrphanedWorkloads(ctx context.Context, reason string) (int64, error) {
	tag, err := d.Pool.Exec(ctx,
		`UPDATE workloads SET status='failed', error=$1, finished_at=now()
		 WHERE status IN ('queued','running')`, reason)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// FailOrphanedModelOps does the same for model pull/delete operations.
// Restart-interrupted ops are marked 'interrupted' — their true outcome
// is unknowable (a pull may have partially completed on the node).
func (d *DB) FailOrphanedModelOps(ctx context.Context, reason string) (int64, error) {
	tag, err := d.Pool.Exec(ctx,
		`UPDATE model_operations SET status='interrupted', error=$1, updated_at=now()
		 WHERE status IN ('dispatched','running')`, reason)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// FailWorkloadIfRunning transitions one workload to failed only while it
// is still running — safe against racing a just-completed row. Used by
// the reconciler for workloads stranded on nodes that went offline.
func (d *DB) FailWorkloadIfRunning(ctx context.Context, id, reason string) error {
	_, err := d.Pool.Exec(ctx,
		`UPDATE workloads SET status='failed', error=$2, finished_at=now()
		 WHERE id=$1 AND status='running'`, id, reason)
	return err
}

// RescheduleWorkload moves a queued/running workload back to queued for a
// retry, recording the new decision.
func (d *DB) RequeueWorkload(ctx context.Context, id string, dec *contracts.Decision) error {
	decJ, _ := json.Marshal(dec)
	_, err := d.Pool.Exec(ctx,
		`UPDATE workloads SET status='queued', node_id=NULL, started_at=NULL, decision=$2
		 WHERE id=$1 AND status IN ('queued','running')`, id, decJ)
	return err
}

func (d *DB) GetWorkload(ctx context.Context, id string) (*Workload, error) {
	return scanWorkload(d.Pool.QueryRow(ctx,
		`SELECT w.id,w.model,w.node_id,COALESCE(n.name,''),w.api_key_id,w.status,
			w.streaming,w.prompt_chars,w.decision,w.error,w.prompt_tokens,
			w.completion_tokens,w.ttft_ms,w.total_ms,w.queued_at,w.started_at,w.finished_at
		 FROM workloads w LEFT JOIN nodes n ON n.id=w.node_id
		 WHERE w.id=$1`, id))
}

func scanWorkload(row pgx.Row) (*Workload, error) {
	var w Workload
	var dec []byte
	err := row.Scan(&w.ID, &w.Model, &w.NodeID, &w.NodeName, &w.APIKeyID, &w.Status,
		&w.Streaming, &w.PromptChars, &dec, &w.Error, &w.PromptTokens,
		&w.CompletionTokens, &w.TTFTms, &w.TotalMS, &w.QueuedAt, &w.StartedAt, &w.FinishedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if dec != nil {
		var dd contracts.Decision
		if json.Unmarshal(dec, &dd) == nil {
			w.Decision = &dd
		}
	}
	return &w, nil
}

type WorkloadFilter struct {
	Status string
	NodeID string
	Limit  int
}

func (d *DB) ListWorkloads(ctx context.Context, f WorkloadFilter) ([]*Workload, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 100
	}
	q := `SELECT w.id,w.model,w.node_id,COALESCE(n.name,''),w.api_key_id,w.status,
		w.streaming,w.prompt_chars,w.decision,w.error,w.prompt_tokens,
		w.completion_tokens,w.ttft_ms,w.total_ms,w.queued_at,w.started_at,w.finished_at
		FROM workloads w LEFT JOIN nodes n ON n.id=w.node_id`
	args := []any{}
	where := ""
	if f.Status != "" {
		where += ` AND w.status=$` + itoa(len(args)+1)
		args = append(args, f.Status)
	}
	if f.NodeID != "" {
		where += ` AND w.node_id=$` + itoa(len(args)+1)
		args = append(args, f.NodeID)
	}
	if where != "" {
		q += ` WHERE ` + where[5:]
	}
	q += ` ORDER BY w.queued_at DESC LIMIT $` + itoa(len(args)+1)
	args = append(args, f.Limit)

	rows, err := d.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Workload
	for rows.Next() {
		w, err := scanWorkload(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func itoa(i int) string { return strconv.Itoa(i) }

// ActivePerNode returns in-flight (queued->running assigned) counts per node.
func (d *DB) ActivePerNode(ctx context.Context) (map[string]int, error) {
	rows, err := d.Pool.Query(ctx,
		`SELECT node_id, count(*) FROM workloads
		 WHERE status='running' AND node_id IS NOT NULL GROUP BY node_id`)
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

func (d *DB) CountActiveWorkloads(ctx context.Context) (int, error) {
	var c int
	err := d.Pool.QueryRow(ctx,
		`SELECT count(*) FROM workloads WHERE status IN ('queued','running')`).Scan(&c)
	return c, err
}

// ---------------------------------------------------------------------------
// Model operations
// ---------------------------------------------------------------------------

type ModelOperation struct {
	ID        string    `json:"id"`
	NodeID    string    `json:"node_id"`
	NodeName  string    `json:"node_name,omitempty"`
	Model     string    `json:"model"`
	Op        string    `json:"op"`
	Status    string    `json:"status"`
	Detail    string    `json:"detail"`
	Completed int64     `json:"completed"`
	Total     int64     `json:"total"`
	Error     string    `json:"error,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (d *DB) CreateModelOperation(ctx context.Context, nodeID, model, op string) (*ModelOperation, error) {
	var o ModelOperation
	err := d.Pool.QueryRow(ctx,
		`INSERT INTO model_operations (node_id,model,op) VALUES ($1,$2,$3)
		 RETURNING id,node_id,model,op,status,detail,completed,total,error,created_at,updated_at`,
		nodeID, model, op).Scan(&o.ID, &o.NodeID, &o.Model, &o.Op, &o.Status,
		&o.Detail, &o.Completed, &o.Total, &o.Error, &o.CreatedAt, &o.UpdatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, ErrActiveModelOp
		}
	}
	return &o, err
}

func (d *DB) UpdateModelOperation(ctx context.Context, id, status, detail string, completed, total int64) error {
	tag, err := d.Pool.Exec(ctx,
		`UPDATE model_operations SET status=$2, detail=$3, completed=$4, total=$5, updated_at=now()
		 WHERE id=$1`, id, status, detail, completed, total)
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

func (d *DB) FinishModelOperation(ctx context.Context, id string, success bool, errStr string) error {
	return d.FinishModelOperationStatus(ctx, id, statusOf(success), errStr)
}

func statusOf(success bool) string {
	if success {
		return "succeeded"
	}
	return "failed"
}

// FinishModelOperationStatus finalizes an op with an explicit terminal
// status ('succeeded'|'failed'|'cancelled'|'interrupted'). Only applies
// while the op is still active so a late OpDone can't resurrect a row
// already cancelled or interrupted.
func (d *DB) FinishModelOperationStatus(ctx context.Context, id, status, errStr string) error {
	tag, err := d.Pool.Exec(ctx,
		`UPDATE model_operations SET status=$2, error=$3, updated_at=now()
		 WHERE id=$1 AND status IN ('dispatched','running')`, id, status, errStr)
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

// InterruptNodeModelOps marks every active op on a node interrupted —
// called when the node's connection drops mid-operation.
func (d *DB) InterruptNodeModelOps(ctx context.Context, nodeID, reason string) error {
	_, err := d.Pool.Exec(ctx,
		`UPDATE model_operations SET status='interrupted', error=$2, updated_at=now()
		 WHERE node_id=$1 AND status IN ('dispatched','running')`, nodeID, reason)
	return err
}

// GetModelOperation fetches one op by id.
func (d *DB) GetModelOperation(ctx context.Context, id string) (*ModelOperation, error) {
	var o ModelOperation
	err := d.Pool.QueryRow(ctx,
		`SELECT o.id,o.node_id,COALESCE(n.name,''),o.model,o.op,o.status,o.detail,
			o.completed,o.total,o.error,o.created_at,o.updated_at
		 FROM model_operations o LEFT JOIN nodes n ON n.id=o.node_id
		 WHERE o.id=$1`, id).Scan(&o.ID, &o.NodeID, &o.NodeName, &o.Model, &o.Op,
		&o.Status, &o.Detail, &o.Completed, &o.Total, &o.Error, &o.CreatedAt, &o.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &o, nil
}

// ErrActiveModelOp signals a conflicting in-flight op on the same
// node+model — produced by the model_operations_active_uniq index.
var ErrActiveModelOp = errors.New("an operation is already active for this model on this node")

func (d *DB) ListModelOperations(ctx context.Context, limit int) ([]*ModelOperation, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := d.Pool.Query(ctx,
		`SELECT o.id,o.node_id,COALESCE(n.name,''),o.model,o.op,o.status,o.detail,
			o.completed,o.total,o.error,o.created_at,o.updated_at
		 FROM model_operations o LEFT JOIN nodes n ON n.id=o.node_id
		 ORDER BY o.created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ModelOperation
	for rows.Next() {
		var o ModelOperation
		if err := rows.Scan(&o.ID, &o.NodeID, &o.NodeName, &o.Model, &o.Op, &o.Status,
			&o.Detail, &o.Completed, &o.Total, &o.Error, &o.CreatedAt, &o.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, &o)
	}
	return out, rows.Err()
}
