package store

import (
	"context"
	"time"
)

// ---------------------------------------------------------------------------
// Control-plane ownership history (migration 0012)
// ---------------------------------------------------------------------------

// OwnershipEvent is one row of control-plane history.
type OwnershipEvent struct {
	ID      int64     `json:"id"`
	OwnerID string    `json:"owner_id,omitempty"`
	Epoch   int64     `json:"epoch"`
	Event   string    `json:"event"` // acquired|released|lost|promoted|refused
	Detail  string    `json:"detail,omitempty"`
	At      time.Time `json:"at"`
}

// AddOwnershipEvent records a lifecycle transition. Best-effort like
// Audit — failures are caller-logged, never fatal.
func (d *DB) AddOwnershipEvent(ctx context.Context, ownerID string, epoch int64, event, detail string) {
	var owner any
	if ownerID != "" {
		owner = ownerID
	}
	d.Pool.Exec(ctx,
		`INSERT INTO ownership_events (owner_id,epoch,event,detail) VALUES ($1,$2,$3,$4)`,
		owner, epoch, event, detail)
}

// ListOwnershipEvents returns recent transitions, newest first.
func (d *DB) ListOwnershipEvents(ctx context.Context, limit int) ([]*OwnershipEvent, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := d.Pool.Query(ctx,
		`SELECT id,COALESCE(owner_id::text,''),epoch,event,detail,at
		 FROM ownership_events ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*OwnershipEvent{}
	for rows.Next() {
		var e OwnershipEvent
		if err := rows.Scan(&e.ID, &e.OwnerID, &e.Epoch, &e.Event, &e.Detail, &e.At); err != nil {
			return nil, err
		}
		out = append(out, &e)
	}
	return out, rows.Err()
}
