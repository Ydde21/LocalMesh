package store

import (
	"context"
	"time"
)

// ---------------------------------------------------------------------------
// Node maintenance state (migration 0011)
//
// nodes.maintenance: '' in service · 'draining' no new work, in-flight
// finishing · 'maintenance' drained, admin may act · 'updating' set by
// the update orchestrator during agent upgrades. Scheduler treats any
// non-empty value as ineligible for NEW work.
// ---------------------------------------------------------------------------

// Maintenance state values.
const (
	MaintNone     = ""
	MaintDraining = "draining"
	MaintMode     = "maintenance"
	MaintUpdating = "updating"
)

// SetMaintenance marks a node draining (or directly into maintenance
// when nothing is in flight — callers decide). Returns the recorded
// state. Refused states are the caller's problem; the store guards only
// against touching nonexistent nodes.
func (d *DB) SetMaintenance(ctx context.Context, nodeID, state, reason string) error {
	var since any
	if state != MaintNone {
		since = time.Now()
	}
	tag, err := d.Pool.Exec(ctx,
		`UPDATE nodes SET maintenance=$2, maintenance_reason=$3,
			maintenance_since=$4, updated_at=now() WHERE id=$1`,
		nodeID, state, reason, since)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
