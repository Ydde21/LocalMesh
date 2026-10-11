// Package store implements the Postgres persistence layer for the
// LocalMesh control plane. It deliberately stores metadata only —
// prompts and inference responses never reach this layer.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/localmesh/localmesh/internal/contracts"
)

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// ErrAlreadyDecided is returned when an enrollment request has already
// been approved or denied.
var ErrAlreadyDecided = errors.New("enrollment request already decided")

// ErrDuplicateCredential is returned when a node credential hash is
// already bound to a different node.
var ErrDuplicateCredential = errors.New("node credential already in use")

type DB struct {
	Pool *pgxpool.Pool
}

func Connect(ctx context.Context, dsn string) (*DB, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = 16
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &DB{Pool: pool}, nil
}

func (d *DB) Close() { d.Pool.Close() }

// ---------------------------------------------------------------------------
// Nodes
// ---------------------------------------------------------------------------

// Node is the persisted record; live connection state lives in nodehub.
type Node struct {
	ID            string                 `json:"id"`
	Name          string                 `json:"name"`
	Status        contracts.NodeStatus   `json:"status"`
	Hardware      contracts.HardwareInfo `json:"hardware"`
	Engines       []contracts.EngineInfo `json:"engines"`
	Models        []contracts.Model      `json:"models"`
	Metrics       *contracts.Metrics     `json:"metrics,omitempty"`
	Labels        map[string]string      `json:"labels"`
	MaxConcurrent int                    `json:"max_concurrent"`
	// Maintenance: '' in service · 'draining' · 'maintenance' ·
	// 'updating'. Any non-empty value excludes the node from new work.
	Maintenance       string     `json:"maintenance,omitempty"`
	MaintenanceReason string     `json:"maintenance_reason,omitempty"`
	MaintenanceSince  *time.Time `json:"maintenance_since,omitempty"`
	// Nullable disk telemetry (migration 0015) — nil means never
	// measured (pre-v0.10 agent or probe failure); never read as 0.
	DiskFreeBytes  *int64     `json:"disk_free_bytes,omitempty"`
	DiskTotalBytes *int64     `json:"disk_total_bytes,omitempty"`
	DiskMeasuredAt *time.Time `json:"disk_measured_at,omitempty"`
	LastSeenAt     *time.Time `json:"last_seen_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

func scanNode(row pgx.Row) (*Node, error) {
	var n Node
	var hw, eng, models, labels, metrics []byte
	var lastSeen *time.Time
	err := row.Scan(&n.ID, &n.Name, &n.Status, &hw, &eng, &models, &metrics,
		&labels, &n.MaxConcurrent, &n.Maintenance, &n.MaintenanceReason,
		&n.MaintenanceSince, &n.DiskFreeBytes, &n.DiskTotalBytes,
		&n.DiskMeasuredAt, &lastSeen, &n.CreatedAt, &n.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	json.Unmarshal(hw, &n.Hardware)
	json.Unmarshal(eng, &n.Engines)
	json.Unmarshal(models, &n.Models)
	json.Unmarshal(labels, &n.Labels)
	if metrics != nil {
		var m contracts.Metrics
		if json.Unmarshal(metrics, &m) == nil {
			n.Metrics = &m
		}
	}
	n.LastSeenAt = lastSeen
	if n.Labels == nil {
		n.Labels = map[string]string{}
	}
	return &n, nil
}

const nodeCols = `id, name, status, hardware, engines, models, metrics,
	labels, max_concurrent, maintenance, maintenance_reason,
	maintenance_since, disk_free_bytes, disk_total_bytes,
	disk_measured_at, last_seen_at, created_at, updated_at`

func (d *DB) CreateNode(ctx context.Context, name, tokenHash string, hw contracts.HardwareInfo, maxConcurrent int) (*Node, error) {
	hwJSON, _ := json.Marshal(hw)
	return scanNode(d.Pool.QueryRow(ctx,
		`INSERT INTO nodes (name, token_hash, hardware, max_concurrent)
		 VALUES ($1,$2,$3,$4) RETURNING `+nodeCols,
		name, tokenHash, hwJSON, maxConcurrent))
}

func (d *DB) GetNode(ctx context.Context, id string) (*Node, error) {
	return scanNode(d.Pool.QueryRow(ctx,
		`SELECT `+nodeCols+` FROM nodes WHERE id=$1`, id))
}

// GetNodeByTokenHash resolves a node credential to its record.
func (d *DB) GetNodeByTokenHash(ctx context.Context, hash string) (*Node, error) {
	return scanNode(d.Pool.QueryRow(ctx,
		`SELECT `+nodeCols+` FROM nodes WHERE token_hash=$1`, hash))
}

func (d *DB) ListNodes(ctx context.Context) ([]*Node, error) {
	rows, err := d.Pool.Query(ctx,
		`SELECT `+nodeCols+` FROM nodes ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// UpdateNodeReport stores the capability report pushed by the agent.
func (d *DB) UpdateNodeReport(ctx context.Context, id string, hw contracts.HardwareInfo, engines []contracts.EngineInfo, models []contracts.Model) error {
	hwJ, _ := json.Marshal(hw)
	engJ, _ := json.Marshal(engines)
	modJ, _ := json.Marshal(models)
	tag, err := d.Pool.Exec(ctx,
		`UPDATE nodes SET hardware=$2, engines=$3, models=$4, updated_at=now()
		 WHERE id=$1`, id, hwJ, engJ, modJ)
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

func (d *DB) UpdateNodeMetrics(ctx context.Context, id string, m contracts.Metrics) error {
	mJ, _ := json.Marshal(m)
	tag, err := d.Pool.Exec(ctx,
		`UPDATE nodes SET metrics=$2, last_seen_at=now(), updated_at=now()
		 WHERE id=$1`, id, mJ)
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

func (d *DB) TouchNode(ctx context.Context, id string) {
	d.Pool.Exec(ctx, `UPDATE nodes SET last_seen_at=now() WHERE id=$1`, id)
}

func (d *DB) SetNodeStatus(ctx context.Context, id string, status contracts.NodeStatus) error {
	tag, err := d.Pool.Exec(ctx,
		`UPDATE nodes SET status=$2, updated_at=now() WHERE id=$1`, id, status)
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

func (d *DB) UpdateNodeConfig(ctx context.Context, id string, name *string, maxConcurrent *int, labels map[string]string) error {
	if name != nil {
		if _, err := d.Pool.Exec(ctx,
			`UPDATE nodes SET name=$2, updated_at=now() WHERE id=$1`, id, *name); err != nil {
			return err
		}
	}
	if maxConcurrent != nil {
		if _, err := d.Pool.Exec(ctx,
			`UPDATE nodes SET max_concurrent=$2, updated_at=now() WHERE id=$1`, id, *maxConcurrent); err != nil {
			return err
		}
	}
	if labels != nil {
		lJ, _ := json.Marshal(labels)
		if _, err := d.Pool.Exec(ctx,
			`UPDATE nodes SET labels=$2, updated_at=now() WHERE id=$1`, id, lJ); err != nil {
			return err
		}
	}
	return nil
}

func (d *DB) DeleteNode(ctx context.Context, id string) error {
	tag, err := d.Pool.Exec(ctx, `DELETE FROM nodes WHERE id=$1`, id)
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

// RotateNodeToken replaces the credential hash (used on revoke->reapprove
// or admin-initiated rotation).
func (d *DB) SetNodeTokenHash(ctx context.Context, id, hash string) error {
	_, err := d.Pool.Exec(ctx,
		`UPDATE nodes SET token_hash=$2, updated_at=now() WHERE id=$1`, id, hash)
	return err
}

// ---------------------------------------------------------------------------
// Settings
// ---------------------------------------------------------------------------

func (d *DB) GetSetting(ctx context.Context, key string, out any) error {
	var raw []byte
	err := d.Pool.QueryRow(ctx,
		`SELECT value FROM settings WHERE key=$1`, key).Scan(&raw)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	return json.Unmarshal(raw, out)
}

func (d *DB) SetSetting(ctx context.Context, key string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = d.Pool.Exec(ctx,
		`INSERT INTO settings (key,value) VALUES ($1,$2)
		 ON CONFLICT (key) DO UPDATE SET value=$2, updated_at=now()`, key, raw)
	return err
}

func (d *DB) AllSettings(ctx context.Context) (map[string]json.RawMessage, error) {
	rows, err := d.Pool.Query(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]json.RawMessage{}
	for rows.Next() {
		var k string
		var v json.RawMessage
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Audit
// ---------------------------------------------------------------------------

type AuditEntry struct {
	ID        int64             `json:"id"`
	Actor     string            `json:"actor"`
	Action    string            `json:"action"`
	Target    string            `json:"target"`
	Meta      map[string]string `json:"meta"`
	CreatedAt time.Time         `json:"created_at"`
}

func (d *DB) Audit(ctx context.Context, actor, action, target string, meta map[string]string) {
	if meta == nil {
		meta = map[string]string{}
	}
	mJ, _ := json.Marshal(meta)
	d.Pool.Exec(ctx,
		`INSERT INTO audit_log (actor,action,target,meta) VALUES ($1,$2,$3,$4)`,
		actor, action, target, mJ)
}

func (d *DB) ListAudit(ctx context.Context, limit int) ([]AuditEntry, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := d.Pool.Query(ctx,
		`SELECT id,actor,action,target,meta,created_at FROM audit_log
		 ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var meta []byte
		if err := rows.Scan(&e.ID, &e.Actor, &e.Action, &e.Target, &meta, &e.CreatedAt); err != nil {
			return nil, err
		}
		json.Unmarshal(meta, &e.Meta)
		out = append(out, e)
	}
	return out, rows.Err()
}
