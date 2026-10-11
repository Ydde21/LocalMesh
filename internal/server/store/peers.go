package store

// Control-plane peer presence (migration 0015).
//
// A waiting standby is invisible everywhere else in the system: it
// holds no ownership lock, serves a 503-only mux, and writes no rows.
// control_plane_peers fixes that — every control-plane process upserts
// one row and heartbeats it while it lives. The advisory lock remains
// the sole authority on WHO owns the cluster; this table only answers
// "is a standby present and alive" for dashboards, /control-plane, and
// the peers metric.

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// Peer roles — the only values the migration's CHECK constraint admits.
const (
	PeerRoleActive  = "active"
	PeerRoleStandby = "standby"
)

// Presence cadence contract shared by writers (PeerReporter) and
// readers (StandbyPresence, /control-plane/peers, the peers gauge):
// a reporter heartbeats every PeerHeartbeatInterval; a peer whose
// heartbeat is older than PeerStaleAfter is treated as gone. ~7 missed
// beats of slack tolerates slow ticks and one-off DB hiccups without
// delaying crash detection past ~15s.
const (
	PeerHeartbeatInterval = 2 * time.Second
	PeerStaleAfter        = 15 * time.Second
)

// Peer is one control_plane_peers row — a control-plane process's
// presence record.
type Peer struct {
	InstanceID  string    `json:"instance_id"`
	Role        string    `json:"role"` // PeerRole*
	Hostname    string    `json:"hostname"`
	StartedAt   time.Time `json:"started_at"`
	HeartbeatAt time.Time `json:"heartbeat_at"`
}

// Fresh reports whether the row's heartbeat is within maxAge of the
// CALLER's clock. For API display only — freshness decisions that gate
// presence use the DB-clock predicates (ListFreshPeers et al.) so a
// skewed control-plane clock can't misdeclare a peer.
func (p *Peer) Fresh(maxAge time.Duration) bool {
	return p != nil && time.Since(p.HeartbeatAt) <= maxAge
}

// UpsertPeer registers this process as a peer (or refreshes an existing
// row's identity). Called once when a reporter starts; liveness after
// that is carried by PeerHeartbeat. role must be a PeerRole* constant —
// anything else is rejected before hitting the CHECK constraint.
func (d *DB) UpsertPeer(ctx context.Context, instanceID, role, hostname string) error {
	if role != PeerRoleActive && role != PeerRoleStandby {
		return fmt.Errorf("peers: invalid role %q", role)
	}
	_, err := d.Pool.Exec(ctx,
		`INSERT INTO control_plane_peers (instance_id, role, hostname)
		 VALUES ($1::uuid, $2, $3)
		 ON CONFLICT (instance_id) DO UPDATE
		   SET role = $2, hostname = $3, heartbeat_at = now()`,
		instanceID, role, hostname)
	return err
}

// PeerHeartbeat refreshes the row's heartbeat_at. A missing row is NOT
// re-created — a deleted row (stopped reporter, manual prune) staying
// absent is the signal readers want, and resurrection would be wrong.
func (d *DB) PeerHeartbeat(ctx context.Context, instanceID string) error {
	_, err := d.Pool.Exec(ctx,
		`UPDATE control_plane_peers SET heartbeat_at = now()
		 WHERE instance_id = $1::uuid`, instanceID)
	return err
}

// DeletePeer removes the row. Best-effort by contract: callers invoke it
// on shutdown/promotion paths where a failure is tolerable because the
// row goes stale on its own within PeerStaleAfter.
func (d *DB) DeletePeer(ctx context.Context, instanceID string) error {
	_, err := d.Pool.Exec(ctx,
		`DELETE FROM control_plane_peers WHERE instance_id = $1::uuid`, instanceID)
	return err
}

// ListPeers returns peer rows newest-heartbeat first. role filters when
// non-empty ("" = all roles); stale rows are included — pruning/staleness
// is a reader concern.
func (d *DB) ListPeers(ctx context.Context, role string) ([]*Peer, error) {
	q := `SELECT instance_id, role, hostname, started_at, heartbeat_at
	      FROM control_plane_peers`
	args := []any{}
	if role != "" {
		q += ` WHERE role = $1`
		args = append(args, role)
	}
	q += ` ORDER BY heartbeat_at DESC`
	rows, err := d.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Peer{}
	for rows.Next() {
		var p Peer
		var id uuid.UUID
		if err := rows.Scan(&id, &p.Role, &p.Hostname, &p.StartedAt, &p.HeartbeatAt); err != nil {
			return nil, err
		}
		p.InstanceID = id.String()
		out = append(out, &p)
	}
	return out, rows.Err()
}

// ListFreshPeers returns peers of a role whose heartbeat is fresh per
// the DATABASE clock — comparing in SQL so a skewed control-plane clock
// can't misdeclare presence. maxAge is the freshness window.
func (d *DB) ListFreshPeers(ctx context.Context, role string, maxAge time.Duration) ([]*Peer, error) {
	rows, err := d.Pool.Query(ctx,
		`SELECT instance_id, role, hostname, started_at, heartbeat_at
		 FROM control_plane_peers
		 WHERE role = $1 AND heartbeat_at > now() - make_interval(secs => $2)
		 ORDER BY heartbeat_at DESC`, role, maxAge.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Peer{}
	for rows.Next() {
		var p Peer
		var id uuid.UUID
		if err := rows.Scan(&id, &p.Role, &p.Hostname, &p.StartedAt, &p.HeartbeatAt); err != nil {
			return nil, err
		}
		p.InstanceID = id.String()
		out = append(out, &p)
	}
	return out, rows.Err()
}

// CountFreshPeers is the cheap presence count behind the peers gauge.
func (d *DB) CountFreshPeers(ctx context.Context, role string, maxAge time.Duration) (int, error) {
	var n int
	err := d.Pool.QueryRow(ctx,
		`SELECT count(*) FROM control_plane_peers
		 WHERE role = $1 AND heartbeat_at > now() - make_interval(secs => $2)`,
		role, maxAge.Seconds()).Scan(&n)
	return n, err
}

// StandbyPresence builds the "standby" object for the /control-plane
// payload — the single place the presence shape is defined so handler
// wiring stays a one-liner:
//
//	"standby": {"present": false, "count": 0}
//	"standby": {"present": true, "count": 1, "instance_id": "...",
//	            "hostname": "cp2", "heartbeat_age_seconds": 2}
//
// present is true while ≥1 standby-role peer has a fresh heartbeat;
// instance_id/hostname identify the freshest standby (the one that
// would promote). heartbeat_age_seconds uses the caller's clock — it's
// a display hint, not a liveness decision (ListFreshPeers already made
// that on the DB clock).
func (d *DB) StandbyPresence(ctx context.Context) (map[string]any, error) {
	fresh, err := d.ListFreshPeers(ctx, PeerRoleStandby, PeerStaleAfter)
	if err != nil {
		return nil, err
	}
	out := map[string]any{
		"present": len(fresh) > 0,
		"count":   len(fresh),
	}
	if len(fresh) > 0 {
		p := fresh[0]
		out["instance_id"] = p.InstanceID
		out["hostname"] = p.Hostname
		out["heartbeat_age_seconds"] = int64(time.Since(p.HeartbeatAt).Seconds())
	}
	return out, nil
}

// PrunePeers deletes rows whose heartbeat is older than maxAge (DB
// clock) — the periodic sweep for crashed peers that never got to
// delete their row. Returns the number of rows removed.
func (d *DB) PrunePeers(ctx context.Context, maxAge time.Duration) (int64, error) {
	tag, err := d.Pool.Exec(ctx,
		`DELETE FROM control_plane_peers
		 WHERE heartbeat_at < now() - make_interval(secs => $1)`, maxAge.Seconds())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// ---------------------------------------------------------------------------
// PeerReporter
// ---------------------------------------------------------------------------

// PeerReporter heartbeats one control_plane_peers row for the lifetime
// of a control-plane process (or one role phase of it). It exists so a
// waiting standby is visible to the rest of the cluster — nothing else
// publishes a standby's existence.
//
// Lifecycle the integrator wires:
//
//	standby: rep := store.NewPeerReporter(st, store.PeerRoleStandby, "", log)
//	         rep.Start(); ... WaitForOwnership ...; rep.Stop(ctx)
//	         — Stop deletes the row at promotion; the new "active"
//	         identity (the ownership instance id) takes over.
//	active:  rep := store.NewPeerReporterWithID(st, own.InstanceID().String(),
//	         store.PeerRoleActive, "", log)
//	         rep.Start(); defer rep.Stop(context.Background())
//
// Using the ownership instance id for the active row makes peers rows
// correlate with control_plane_lock.owner_id and ownership_events.
// Stop is idempotent and never panics; a skipped Stop leaves the row
// to fade stale within PeerStaleAfter — presence is never faked either
// direction.
type PeerReporter struct {
	st       *DB
	id       string
	role     string
	hostname string
	interval time.Duration
	log      *slog.Logger

	started atomic.Bool
	stopCh  chan struct{}
	done    chan struct{}
	stop    sync.Once
}

// NewPeerReporter builds a reporter with a fresh instance id — the
// right identity for a standby (it has no ownership id yet).
func NewPeerReporter(st *DB, role, hostname string, log *slog.Logger) *PeerReporter {
	return NewPeerReporterWithID(st, uuid.NewString(), role, hostname, log)
}

// NewPeerReporterWithID builds a reporter bound to a specific instance
// id — the right identity for the active owner (its ownership uuid).
func NewPeerReporterWithID(st *DB, instanceID, role, hostname string, log *slog.Logger) *PeerReporter {
	if _, err := uuid.Parse(instanceID); err != nil {
		instanceID = uuid.NewString()
	}
	if hostname == "" {
		hostname, _ = os.Hostname()
	}
	if log == nil {
		log = slog.Default()
	}
	return &PeerReporter{
		st: st, id: instanceID, role: role, hostname: hostname,
		interval: PeerHeartbeatInterval,
		log:      log.With("component", "peer-reporter"),
		stopCh:   make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// InstanceID is the identity this reporter heartbeats.
func (r *PeerReporter) InstanceID() string { return r.id }

// Start registers the row and launches the heartbeat loop. Idempotent —
// a second call is a no-op.
func (r *PeerReporter) Start() {
	if !r.started.CompareAndSwap(false, true) {
		return
	}
	go r.loop()
}

// Stop ends heartbeating and best-effort deletes the row. Waits for the
// loop to exit (bounded by ctx); the delete then gets its own short
// deadline so shutdown can't stall on a dead database. Idempotent.
func (r *PeerReporter) Stop(ctx context.Context) {
	if !r.started.Load() {
		return
	}
	r.stop.Do(func() { close(r.stopCh) })
	select {
	case <-r.done:
	case <-ctx.Done():
	}
	dctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := r.st.DeletePeer(dctx, r.id); err != nil {
		r.log.Debug("peer row delete on stop", "err", err)
	}
}

func (r *PeerReporter) loop() {
	defer close(r.done)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := r.st.UpsertPeer(ctx, r.id, r.role, r.hostname); err != nil {
		r.log.Warn("peer register failed — presence may lag", "err", err)
	}
	cancel()
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		select {
		case <-r.stopCh:
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := r.st.PeerHeartbeat(ctx, r.id); err != nil {
				// The row may be gone (pruned/stop) or the DB down —
				// either way presence correctly fades; keep trying so a
				// transient outage self-heals on the next tick.
				r.log.Debug("peer heartbeat failed", "err", err)
			}
			cancel()
		}
	}
}
