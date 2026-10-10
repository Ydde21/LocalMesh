-- Model-operation lifecycle: add cancelled/interrupted terminal states
-- and a partial unique index preventing concurrent conflicting ops on
-- the same node+model (pull-pull, pull-delete, delete-delete).
ALTER TABLE model_operations DROP CONSTRAINT model_operations_status_check;
ALTER TABLE model_operations ADD CONSTRAINT model_operations_status_check
    CHECK (status IN ('dispatched','running','succeeded','failed','cancelled','interrupted'));

CREATE UNIQUE INDEX IF NOT EXISTS model_operations_active_uniq
    ON model_operations (node_id, model)
    WHERE status IN ('dispatched','running');
