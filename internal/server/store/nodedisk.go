package store

// Node disk telemetry (migration 0015).
//
// Disk pressure is the classic silent killer for model-serving nodes —
// pulls fail with opaque engine errors only once the volume is already
// full. The agent reports free/total bytes inside its heartbeat metrics
// (wire extension: contracts.Metrics disk_free_bytes/disk_total_bytes,
// ~10s cadence); the hub persists them here on receipt, stamping
// disk_measured_at itself so consumers see measurement freshness
// without trusting agent clocks.
//
// All three fields are nullable end-to-end: pre-v0.10 agents and
// agent-side probe failures leave the columns NULL, which must surface
// as "unmeasured" — never as 0 bytes free.

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// NodeDisk is the nullable disk-telemetry triple stored on a node row.
// Nil fields mean "never measured". Emitted on API node rows as
// disk_free_bytes / disk_total_bytes / disk_measured_at.
type NodeDisk struct {
	FreeBytes  *int64     `json:"disk_free_bytes"`
	TotalBytes *int64     `json:"disk_total_bytes"`
	MeasuredAt *time.Time `json:"disk_measured_at"`
}

// UpdateNodeDisk records one measured disk report for a node, stamping
// disk_measured_at = now() (server clock). Called by the hub only when
// the heartbeat actually carries both values — a metrics report without
// disk fields must not touch the columns, so the last real measurement
// (and its timestamp) survives partial reports. Unmeasured stays NULL.
// Negative inputs are clamped: a bogus agent report must never write a
// negative "free bytes" for dashboards to render.
func (d *DB) UpdateNodeDisk(ctx context.Context, id string, freeBytes, totalBytes int64) error {
	if freeBytes < 0 {
		freeBytes = 0
	}
	if totalBytes < 0 {
		totalBytes = 0
	}
	tag, err := d.Pool.Exec(ctx,
		`UPDATE nodes SET disk_free_bytes=$2, disk_total_bytes=$3,
			disk_measured_at=now(), updated_at=now()
		 WHERE id=$1`, id, freeBytes, totalBytes)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// GetNodeDisk reads the disk triple for one node. Returns ErrNotFound
// when the node is gone; otherwise always returns a non-nil NodeDisk
// whose fields are nil when unmeasured.
func (d *DB) GetNodeDisk(ctx context.Context, id string) (*NodeDisk, error) {
	var nd NodeDisk
	err := d.Pool.QueryRow(ctx,
		`SELECT disk_free_bytes, disk_total_bytes, disk_measured_at
		 FROM nodes WHERE id=$1`, id).
		Scan(&nd.FreeBytes, &nd.TotalBytes, &nd.MeasuredAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &nd, nil
}
