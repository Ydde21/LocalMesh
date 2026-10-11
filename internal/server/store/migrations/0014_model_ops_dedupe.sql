-- Model-op dedupe hardening: close the 'interrupted' hole in
-- model_operations_active_uniq (v0.10 core-correctness fix).
--
-- DEFECT: the partial unique index covered only
-- ('queued','dispatched','running'). An op parked 'interrupted' —
-- between a node drop / control-plane restart / uncertain delete
-- dispatch and the resume sweep — did NOT block EnqueueModelOp, so a
-- resubmit could insert a second live op for the same (node_id, model).
-- ResumeInterruptedPulls then requeued the old row alongside the new
-- one: two live ops, duplicate dispatch (two pulls racing, or a pull +
-- delete conflict). For deletes the window was permanent — deletes are
-- never auto-resumed — so a resubmit silently duplicated them.
--
-- SAFETY OF THIS MIGRATION: the same gap let existing databases already
-- accumulate duplicate covered rows per (node_id, model). The old index
-- guaranteed at most one row in ('queued','dispatched','running'), so
-- every duplicate set is exactly <=1 live row + N interrupted rows. The
-- UPDATE below resolves each set BEFORE the index is rebuilt: keep the
-- single live row when present (it may be executing on an agent right
-- now), else the newest interrupted row (freshest expression of
-- intent); cancel the rest with an honest terminal error so audit
-- history shows exactly what happened. Both statements run inside the
-- migrator's single transaction, so cleanup and constraint land
-- atomically. Worst case: a still-running old binary inserts a
-- duplicate between the UPDATE and the CREATE INDEX — then CREATE
-- INDEX fails, the transaction rolls back, and the next boot re-runs
-- the cleanup. Honest failure, never silent corruption.

WITH ranked AS (
    SELECT id,
           ROW_NUMBER() OVER (
               PARTITION BY node_id, model
               ORDER BY (status IN ('queued','dispatched','running')) DESC,
                        created_at DESC, id DESC
           ) AS rn
    FROM model_operations
    WHERE status IN ('queued','dispatched','running','interrupted')
)
UPDATE model_operations o
SET status      = 'cancelled',
    error       = 'cancelled by migration 0014: duplicate model operation for this node+model',
    last_error  = 'cancelled by migration 0014: duplicate model operation for this node+model',
    finished_at = now(),
    updated_at  = now()
FROM ranked
WHERE o.id = ranked.id AND ranked.rn > 1;

DROP INDEX model_operations_active_uniq;
CREATE UNIQUE INDEX model_operations_active_uniq
    ON model_operations (node_id, model)
    WHERE status IN ('queued','dispatched','running','interrupted');
