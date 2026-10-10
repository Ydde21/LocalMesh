package store

import (
	"context"
	"time"
)

// NodeEvent is one connect/disconnect row from node_events.
type NodeEvent struct {
	ID           int64     `json:"id"`
	NodeID       string    `json:"node_id"`
	Event        string    `json:"event"`
	Reason       string    `json:"reason"`
	AgentVersion string    `json:"agent_version"`
	CreatedAt    time.Time `json:"created_at"`
}

// AddNodeEvent records a connection lifecycle event.
func (d *DB) AddNodeEvent(ctx context.Context, nodeID, event, reason, agentVersion string) error {
	_, err := d.Pool.Exec(ctx,
		`INSERT INTO node_events (node_id, event, reason, agent_version)
		 VALUES ($1,$2,$3,$4)`, nodeID, event, reason, agentVersion)
	return err
}

// ControlPlaneInfo is the ownership row — which instance holds the
// lease, its fencing epoch, and how fresh its heartbeat is.
type ControlPlaneInfo struct {
	OwnerID     string    `json:"owner_id"`
	Epoch       int64     `json:"epoch"`
	StartedAt   time.Time `json:"started_at"`
	HeartbeatAt time.Time `json:"heartbeat_at"`
}

// ReadControlPlane returns the current ownership row, or nil if the
// migration predates it (pre-v0.6 databases can't reach this path —
// AcquireOwnership runs before serving — but tolerate a missing row).
func (d *DB) ReadControlPlane(ctx context.Context) (*ControlPlaneInfo, error) {
	var info ControlPlaneInfo
	err := d.Pool.QueryRow(ctx,
		`SELECT owner_id::text, epoch, started_at, heartbeat_at
		 FROM control_plane_lock WHERE id = 1`).
		Scan(&info.OwnerID, &info.Epoch, &info.StartedAt, &info.HeartbeatAt)
	if err != nil {
		return nil, err
	}
	return &info, nil
}

// ListNodeEvents returns a node's most recent events, newest first.
func (d *DB) ListNodeEvents(ctx context.Context, nodeID string, limit int) ([]NodeEvent, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := d.Pool.Query(ctx,
		`SELECT id,node_id,event,reason,agent_version,created_at
		 FROM node_events WHERE node_id=$1
		 ORDER BY id DESC LIMIT $2`, nodeID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NodeEvent
	for rows.Next() {
		var e NodeEvent
		if err := rows.Scan(&e.ID, &e.NodeID, &e.Event, &e.Reason,
			&e.AgentVersion, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
