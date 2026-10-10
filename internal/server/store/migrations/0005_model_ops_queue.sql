-- Model-operation scheduling queue: persistent 'queued' state, retry
-- bookkeeping, honest progress fields, and priority ordering.

ALTER TABLE model_operations DROP CONSTRAINT model_operations_status_check;
ALTER TABLE model_operations ADD CONSTRAINT model_operations_status_check
    CHECK (status IN ('queued','dispatched','running','succeeded','failed','cancelled','interrupted'));

ALTER TABLE model_operations
    ADD COLUMN attempt          INT NOT NULL DEFAULT 0,
    ADD COLUMN max_attempts     INT NOT NULL DEFAULT 3,
    ADD COLUMN next_attempt_at  TIMESTAMPTZ,
    ADD COLUMN last_error       TEXT,
    ADD COLUMN phase            TEXT NOT NULL DEFAULT '',
    ADD COLUMN rate_bps         BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN indeterminate    BOOL NOT NULL DEFAULT false,
    ADD COLUMN priority         INT NOT NULL DEFAULT 0,
    ADD COLUMN finished_at      TIMESTAMPTZ;

-- Dedupe is per (node, model) regardless of op kind: a pull in flight
-- blocks a delete and vice versa. 'queued' joins the active set so a
-- waiting op also dedupes new submissions.
DROP INDEX model_operations_active_uniq;
CREATE UNIQUE INDEX model_operations_active_uniq
    ON model_operations (node_id, model)
    WHERE status IN ('queued','dispatched','running');

CREATE INDEX model_operations_due_idx ON model_operations (next_attempt_at)
    WHERE status = 'queued';
