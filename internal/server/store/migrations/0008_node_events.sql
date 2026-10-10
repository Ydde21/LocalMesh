-- Node connection history (v0.6 observability).
-- One row per connect/disconnect with the negotiated detail available
-- at that point — close reason (replaced, revoked, shutdown, client
-- EOF) and the agent version reported in hello. Bounded by query, not
-- by retention: callers always LIMIT.
CREATE TABLE node_events (
    id            BIGSERIAL PRIMARY KEY,
    node_id       UUID NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    event         TEXT NOT NULL CHECK (event IN ('connected','disconnected')),
    reason        TEXT NOT NULL DEFAULT '',
    agent_version TEXT NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX node_events_node_idx ON node_events (node_id, created_at DESC);
