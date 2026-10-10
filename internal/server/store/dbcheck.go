package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// ---------------------------------------------------------------------------
// Ownership / connection health checks (v0.9 Postgres failure-recovery)
//
// The advisory-lock session (internal/server/ownership.go) is the liveness
// authority; these helpers let the REST of the control plane verify — through
// the shared pool, on an independent connection — what the lock row says.
// They exist for three consumers, all wired by the integrator:
//
//   - /readyz: answer 200 only while the pool reaches Postgres AND the lock
//     row still names this owner+epoch, so a fenced-but-draining instance
//     stops reporting ready the moment a successor stamps its takeover.
//   - Fenced-exit orphan sweeps: a demoted owner must not mass-fail rows a
//     promoted successor has already started dispatching — the *IfOwner
//     variants no-op once control_plane_lock.owner_id points elsewhere.
//   - WatchOwnershipRow (internal/server/dbwatch.go): a second loss detector
//     that polls this row instead of the lock session.
// ---------------------------------------------------------------------------

// OwnershipHeld reports whether the control_plane_lock row still names
// exactly this owner and fencing epoch — i.e. no successor has claimed
// ownership since. A single indexed read; it doubles as a liveness probe
// (any error means the pool cannot reach Postgres).
//
// ownerID is the claiming instance's Ownership.InstanceID().String(); epoch
// is its Ownership.Epoch(). A missing lock row reports (false, nil) — on a
// migrated database the row always exists, so "absent" is treated as
// "not held", not as an error.
func (d *DB) OwnershipHeld(ctx context.Context, ownerID string, epoch int64) (bool, error) {
	var held bool
	err := d.Pool.QueryRow(ctx,
		`SELECT (owner_id = $1::uuid AND epoch = $2)
		 FROM control_plane_lock WHERE id = 1`,
		ownerID, epoch).Scan(&held)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return held, nil
}

// FailOrphanedWorkloadsIfOwner is FailOrphanedWorkloads guarded by ownership:
// queued/running workloads are failed only while control_plane_lock still
// names ownerID. Use it on the fenced-exit path — once a successor has
// claimed ownership and stamped its own owner_id, a demoted instance's
// shutdown sweep must not touch rows the new owner is dispatching, so this
// returns 0 instead of an unfenced mass-fail.
func (d *DB) FailOrphanedWorkloadsIfOwner(ctx context.Context, reason, ownerID string) (int64, error) {
	tag, err := d.Pool.Exec(ctx,
		`UPDATE workloads w SET status='failed', error=$1, finished_at=now()
		 WHERE w.status IN ('queued','running')
		   AND (SELECT c.owner_id FROM control_plane_lock c WHERE c.id = 1) = $2::uuid`,
		reason, ownerID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// FailOrphanedModelOpsIfOwner is FailOrphanedModelOps guarded the same way:
// dispatched/running ops are marked 'interrupted' only while the lock row
// still names ownerID. Boot-time startup sweeps keep using the unguarded
// variant — at boot the caller IS the freshly-stamped owner, so the guard
// would pass anyway; this variant exists for the fenced-exit path.
func (d *DB) FailOrphanedModelOpsIfOwner(ctx context.Context, reason, ownerID string) (int64, error) {
	tag, err := d.Pool.Exec(ctx,
		`UPDATE model_operations o SET status='interrupted', error=$1, updated_at=now()
		 WHERE o.status IN ('dispatched','running')
		   AND (SELECT c.owner_id FROM control_plane_lock c WHERE c.id = 1) = $2::uuid`,
		reason, ownerID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
