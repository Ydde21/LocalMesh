package server

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Single-active control-plane ownership.
//
// Design: ONE dedicated pgx connection holds a session-scoped
// pg_advisory_lock — process death (or a dead DB connection) releases
// it automatically, so crash recovery needs no lease-expiry heuristics.
// The advisory lock alone is not treated as sufficient: on acquisition
// the owner bumps control_plane_lock.epoch, and authoritative writes
// (model-op claims) carry that epoch as a fencing token — a stale
// process whose connection silently died claims zero rows the moment a
// successor takes over. A watchdog closes node connections on loss so a
// fenced process also stops dispatching inference.
const ownershipLockKey int64 = 0x4c4d4350 // "LMCP"

// ownershipAcquireBudget bounds how long startup waits for a
// predecessor's lock to clear (graceful restarts release instantly;
// crashed ones release when the DB notices the dead conn — fast).
// Var so tests can shrink the contention-wait.
var ownershipAcquireBudget = 8 * time.Second

const ownershipHeartbeat = 2 * time.Second

// Ownership is the active control-plane lease. Lost() fires exactly
// once, when the advisory-lock session fails — callers must fence
// writes and close node connections.
type Ownership struct {
	conn       *pgx.Conn
	instanceID uuid.UUID
	epoch      int64
	backendPID uint32 // pg backend pid of the lock session
	log        *slog.Logger
	lost       chan struct{}
	stop       chan struct{}
	hbDone     chan struct{}
	relOnce    sync.Once
}

// OwnerInfo is the row visible to a blocked second instance.
type OwnerInfo struct {
	OwnerID     uuid.UUID `json:"owner_id"`
	Epoch       int64     `json:"epoch"`
	StartedAt   time.Time `json:"started_at"`
	HeartbeatAt time.Time `json:"heartbeat_at"`
}

// AcquireOwnership takes the cluster's single-active lease. On success
// the returned Ownership must be Released at shutdown. On contention it
// reads the current owner row and returns a descriptive error — a
// second control plane must fail clearly, never silently co-own the
// cluster (duplicate model-op dispatch is the corruption this prevents).
func AcquireOwnership(ctx context.Context, dsn string, log *slog.Logger) (*Ownership, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("ownership connect: %w", err)
	}
	deadline := time.Now().Add(ownershipAcquireBudget)
	var held bool
	for {
		if err = conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", ownershipLockKey).Scan(&held); err != nil {
			conn.Close(ctx)
			return nil, fmt.Errorf("ownership lock: %w", err)
		}
		if held {
			break
		}
		if time.Now().After(deadline) {
			info := readOwnerInfo(ctx, conn)
			conn.Close(ctx)
			if info != nil {
				return nil, fmt.Errorf("control-plane ownership held by another instance "+
					"(owner %s, epoch %d, started %s, heartbeat %s ago) — refusing to start "+
					"to prevent split-brain scheduling",
					info.OwnerID, info.Epoch, info.StartedAt.Format(time.RFC3339),
					time.Since(info.HeartbeatAt).Round(time.Second))
			}
			return nil, fmt.Errorf("control-plane ownership held by another instance — refusing to start")
		}
		select {
		case <-ctx.Done():
			conn.Close(ctx)
			return nil, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}

	instanceID := uuid.New()
	var pid uint32
	if err := conn.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		conn.Close(ctx)
		return nil, fmt.Errorf("ownership backend pid: %w", err)
	}
	var epoch int64
	err = conn.QueryRow(ctx,
		`INSERT INTO control_plane_lock (id, owner_id, epoch)
		 VALUES (1, $1, 1)
		 ON CONFLICT (id) DO UPDATE
		   SET owner_id = $1, epoch = control_plane_lock.epoch + 1,
		       started_at = now(), heartbeat_at = now()
		 RETURNING epoch`, instanceID).Scan(&epoch)
	if err != nil {
		conn.Close(ctx)
		return nil, fmt.Errorf("ownership fence: %w", err)
	}
	if log == nil {
		log = slog.Default()
	}
	o := &Ownership{
		conn:       conn,
		instanceID: instanceID,
		epoch:      epoch,
		backendPID: pid,
		log:        log,
		lost:       make(chan struct{}),
		stop:       make(chan struct{}),
		hbDone:     make(chan struct{}),
	}
	go o.heartbeat()
	log.Info("control-plane ownership acquired", "instance", instanceID, "epoch", epoch)
	return o, nil
}

// Epoch is the fencing token authoritative writes must carry.
func (o *Ownership) Epoch() int64 { return o.epoch }

// BackendPID is the Postgres backend pid of the lock session — lets a
// peer connection kill it precisely (tests, admin tooling).
func (o *Ownership) BackendPID() uint32 { return o.backendPID }

// Lost closes when the advisory-lock session fails. Exactly once.
func (o *Ownership) Lost() <-chan struct{} { return o.lost }

// Release drops the advisory lock (conn close) and stops heartbeats.
// The owner row stays for observability — the next acquirer overwrites
// it and bumps the epoch.
func (o *Ownership) Release(ctx context.Context) {
	if o == nil || o.conn == nil {
		return
	}
	o.relOnce.Do(func() {
		// Stop heartbeating, then close — a heartbeat failure after
		// close would otherwise trip Lost() during a CLEAN shutdown.
		close(o.stop)
		<-o.hbDone
		o.conn.Close(ctx)
	})
}

func (o *Ownership) heartbeat() {
	t := time.NewTicker(ownershipHeartbeat)
	defer func() {
		t.Stop()
		close(o.hbDone)
	}()
	for {
		select {
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			tag, err := o.conn.Exec(ctx,
				`UPDATE control_plane_lock SET heartbeat_at = now()
				 WHERE id = 1 AND owner_id = $1`, o.instanceID)
			cancel()
			if err != nil {
				o.log.Error("ownership heartbeat failed — advisory lock session dead",
					"err", err)
				close(o.lost)
				return
			}
			if tag.RowsAffected() == 0 {
				// Row no longer names us: the only path here is a
				// successor that already holds the lock — we're fenced.
				o.log.Error("ownership row lost — another control plane has taken over")
				close(o.lost)
				return
			}
		case <-o.stop:
			return
		}
	}
}

func readOwnerInfo(ctx context.Context, conn *pgx.Conn) *OwnerInfo {
	var info OwnerInfo
	err := conn.QueryRow(ctx,
		`SELECT owner_id, epoch, started_at, heartbeat_at
		 FROM control_plane_lock WHERE id = 1`).
		Scan(&info.OwnerID, &info.Epoch, &info.StartedAt, &info.HeartbeatAt)
	if err != nil {
		return nil
	}
	return &info
}
