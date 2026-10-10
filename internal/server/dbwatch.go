package server

// DB-side ownership watchdog (v0.9 Postgres failure-recovery).
//
// The primary fence is the advisory-lock session's own heartbeat
// (ownership.go): it declares loss when its Exec fails or the lock row
// stops naming us, bounded by one heartbeat tick plus the exec timeout.
// WatchOwnershipRow is a SECOND, independent detector reading the same
// control_plane_lock row through the shared pool — it catches a
// *confirmed takeover* (a successor has already stamped owner_id+epoch)
// even while the lock-session heartbeat hasn't ticked or its Exec is
// still waiting out its timeout.
//
// It deliberately never fires on query errors: a Postgres that is merely
// down cannot have been claimed, and treating "can't read" as "lost"
// would fence a healthy owner over a pool blip. "Row read says someone
// else owns it" is the only signal here — the heartbeat stays the
// authority for connectivity loss.
//
// ---------------------------------------------------------------------------
// INTEGRATOR WIRING (this file owns no production wiring itself)
// ---------------------------------------------------------------------------
//
//	server.go, in Start after `own` is acquired (either branch), e.g.
//	near `qctx, qcancel := context.WithCancel(ctx)`:
//
//	  dbLost := WatchOwnershipRow(qctx, st, own, time.Second, s.log)
//
//	and in the lifecycle select, fold it into the loss case:
//
//	  case <-MergedLoss(own.Lost(), dbLost):
//	      // ... existing fenced-shutdown body, unchanged ...
//
//	Server.Start is core-owned; apply those two lines there, not here.
//
//	Related helpers this workstream provides in internal/server/store/
//	dbcheck.go for the same integrator pass:
//
//	  - /readyz (~server.go:331): replace `s.st.Pool.Ping(pctx)` with
//	    `s.st.OwnershipHeld(pctx, own.InstanceID().String(), own.Epoch())`
//	    and 503 unless (held && err == nil) — one roundtrip, subsumes the
//	    ping, and flips a fenced-but-draining instance to 503 the moment
//	    a successor stamps the lock row.
//	  - fenced-exit sweep (~server.go:414): use
//	    `FailOrphanedWorkloadsIfOwner(ctx, reason, own.InstanceID().String())`
//	    so a demoted owner cannot mass-fail workloads the promoted
//	    successor has begun dispatching during the drain window.
//	  - bookkeeping loops (~server.go:287-288): `reconcileLoop` and
//	    `drainLoop` run on `ctx`; running them on `qctx` stops their
//	    unfenced writes the same moment the dispatch queues stop.

import (
	"context"
	"log/slog"
	"time"

	"github.com/localmesh/localmesh/internal/server/store"
)

// WatchOwnershipRow polls the control_plane_lock row on the shared pool
// every interval and closes the returned channel the first time the row
// is read and does NOT name (own.instanceID, own.epoch) — i.e. a
// successor already holds it. It closes nothing on ctx cancel or query
// error; callers merge it with Ownership.Lost via MergedLoss.
//
// interval <= 0 defaults to 1s — comfortably faster than the 2s lock-
// session heartbeat so a stamped takeover is noticed first.
func WatchOwnershipRow(ctx context.Context, st *store.DB, own *Ownership, interval time.Duration, log *slog.Logger) <-chan struct{} {
	lost := make(chan struct{})
	if interval <= 0 {
		interval = time.Second
	}
	if log == nil {
		log = slog.Default()
	}
	ownerID := own.InstanceID().String()
	epoch := own.Epoch()
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			qctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			held, err := st.OwnershipHeld(qctx, ownerID, epoch)
			cancel()
			switch {
			case err != nil:
				// Can't read the row — can't confirm a takeover. The
				// lock-session heartbeat owns the connectivity signal.
				log.Debug("ownership row check failed", "err", err)
			case !held:
				log.Error("ownership row re-stamped by a successor — fencing this instance",
					"epoch", epoch)
				close(lost)
				return
			}
		}
	}()
	return lost
}

// MergedLoss returns a channel that closes when either loss signal
// fires, letting Server.Start keep ONE fenced-shutdown case for both the
// lock-session heartbeat and the pool-side row watcher:
//
//	case <-MergedLoss(own.Lost(), dbLost):
//
// A nil second argument selects the heartbeat alone — wiring the watcher
// in is optional and safe to omit.
func MergedLoss(a, b <-chan struct{}) <-chan struct{} {
	if b == nil {
		return a
	}
	m := make(chan struct{})
	go func() {
		select {
		case <-a:
		case <-b:
		}
		close(m)
	}()
	return m
}
