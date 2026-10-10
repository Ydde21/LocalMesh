-- Durable canary-rollout approval. v0.7 kept the approve decision in
-- process memory — a control-plane restart or standby promotion
-- re-parked an already-approved canary and forced a second approval.
-- approved_at IS NOT NULL is the authoritative "canary approved" bit;
-- it is written in the same UPDATE that flips state back to 'running',
-- so recovery can never see approved-but-parked or parked-but-approved.

ALTER TABLE rollouts
    ADD COLUMN approved_by text        NOT NULL DEFAULT '',
    ADD COLUMN approved_at timestamptz;
