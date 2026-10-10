-- Agent update orchestration. agent_updates rows are the persistent
-- state of one node moving to one release; rollouts group them into an
-- administrable campaign (single / selected / sequential / canary /
-- parallel). Everything survives control-plane restarts; dispatch is
-- epoch-fenced like model ops.

CREATE TABLE rollouts (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    release_id    uuid NOT NULL REFERENCES releases(id),
    strategy      text NOT NULL,             -- single|selected|sequential|canary|parallel
    state         text NOT NULL DEFAULT 'pending',
                  -- pending|running|awaiting_approval|succeeded|failed|cancelled
    canary_count  int  NOT NULL DEFAULT 1,
    batch_size    int  NOT NULL DEFAULT 1,
    error         text NOT NULL DEFAULT '',
    created_by    text NOT NULL DEFAULT '',
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE agent_updates (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    rollout_id    uuid REFERENCES rollouts(id),
    release_id    uuid NOT NULL REFERENCES releases(id),
    node_id       uuid NOT NULL REFERENCES nodes(id),
    phase         text NOT NULL DEFAULT 'pending',
                  -- pending|draining|dispatched|verifying|downloading|
                  -- staging|installing|restarting|verify_health|
                  -- succeeded|failed|rolled_back|cancelled|interrupted
    attempt       int  NOT NULL DEFAULT 0,
    error         text NOT NULL DEFAULT '',
    detail        text NOT NULL DEFAULT '',
    from_version  text NOT NULL DEFAULT '',
    to_version    text NOT NULL DEFAULT '',
    epoch         bigint NOT NULL DEFAULT -1, -- fencing token of the dispatching owner
    created_at    timestamptz NOT NULL DEFAULT now(),
    dispatched_at timestamptz,
    updated_at    timestamptz NOT NULL DEFAULT now(),
    finished_at   timestamptz
);
-- One in-flight update per node, ever: an unfinished row blocks a
-- second rollout targeting the same node until it resolves.
CREATE UNIQUE INDEX agent_updates_one_live_per_node
    ON agent_updates (node_id) WHERE finished_at IS NULL;
CREATE INDEX agent_updates_live_idx ON agent_updates (phase) WHERE finished_at IS NULL;
CREATE INDEX agent_updates_rollout_idx ON agent_updates (rollout_id);
