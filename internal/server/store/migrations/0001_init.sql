-- LocalMesh initial schema. (schema_migrations is created by the migrator.)

-- Unique key material for connected nodes. Hash only; raw tokens are
-- shown once at approval/enroll time and never stored.
CREATE TABLE nodes (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name            TEXT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending','approved','revoked')),
    token_hash      TEXT NOT NULL,
    hardware        JSONB NOT NULL DEFAULT '{}',
    engines         JSONB NOT NULL DEFAULT '[]',
    models          JSONB NOT NULL DEFAULT '[]',
    metrics         JSONB,
    labels          JSONB NOT NULL DEFAULT '{}',
    max_concurrent  INT NOT NULL DEFAULT 2 CHECK (max_concurrent > 0),
    last_seen_at    TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX nodes_status_idx ON nodes (status);

-- Enrollment tokens are created by admins and consumed by `agent join`.
CREATE TABLE enrollment_tokens (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    label       TEXT NOT NULL DEFAULT '',
    token_hash  TEXT NOT NULL UNIQUE,
    prefix      TEXT NOT NULL,
    max_uses    INT NOT NULL DEFAULT 1,
    uses        INT NOT NULL DEFAULT 0,
    expires_at  TIMESTAMPTZ NOT NULL,
    revoked_at  TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One enrollment attempt. Requires admin approval before credentials
-- are issued (enrollment tokens are bearer secrets for requesting
-- enrollment, not for operating a node).
CREATE TABLE enrollment_requests (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    token_id     UUID NOT NULL REFERENCES enrollment_tokens(id),
    name         TEXT NOT NULL,
    hardware     JSONB NOT NULL DEFAULT '{}',
    status       TEXT NOT NULL DEFAULT 'pending'
                 CHECK (status IN ('pending','approved','denied')),
    node_id      UUID REFERENCES nodes(id),
    -- Secret the agent uses to poll for the decision. Never exposed to
    -- the dashboard.
    poll_secret_hash TEXT NOT NULL,
    -- Hash of the agent-generated node credential. Moved into the nodes
    -- table on approval; the plaintext never touches the server.
    node_token_hash TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    decided_at   TIMESTAMPTZ
);
CREATE INDEX enrollment_requests_status_idx ON enrollment_requests (status);

-- API keys for the OpenAI-compatible gateway and the admin API.
CREATE TABLE api_keys (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name         TEXT NOT NULL,
    prefix       TEXT NOT NULL,           -- visible key fragment, e.g. "lm_a1b2"
    key_hash     TEXT NOT NULL UNIQUE,
    role         TEXT NOT NULL DEFAULT 'inference'
                 CHECK (role IN ('admin','inference','readonly')),
    last_used_at TIMESTAMPTZ,
    revoked_at   TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Model pull/delete operations dispatched to nodes.
CREATE TABLE model_operations (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    node_id     UUID NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    model       TEXT NOT NULL,
    op          TEXT NOT NULL CHECK (op IN ('pull','delete')),
    status      TEXT NOT NULL DEFAULT 'dispatched'
                CHECK (status IN ('dispatched','running','succeeded','failed')),
    detail      TEXT NOT NULL DEFAULT '',
    completed   BIGINT NOT NULL DEFAULT 0,
    total       BIGINT NOT NULL DEFAULT 0,
    error       TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX model_operations_node_idx ON model_operations (node_id);

-- Inference workloads. Privacy: prompt text and responses are NEVER
-- persisted, only counts/timings.
CREATE TABLE workloads (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    model          TEXT NOT NULL,
    node_id        UUID REFERENCES nodes(id),
    api_key_id     UUID REFERENCES api_keys(id),
    status         TEXT NOT NULL DEFAULT 'queued'
                   CHECK (status IN ('queued','running','completed','failed','cancelled')),
    streaming      BOOLEAN NOT NULL DEFAULT false,
    prompt_chars   INT NOT NULL DEFAULT 0,
    decision       JSONB,
    error          TEXT NOT NULL DEFAULT '',
    prompt_tokens     INT,
    completion_tokens INT,
    ttft_ms        INT,
    total_ms       INT,
    queued_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at     TIMESTAMPTZ,
    finished_at    TIMESTAMPTZ
);
CREATE INDEX workloads_status_idx ON workloads (status);
CREATE INDEX workloads_queued_idx  ON workloads (queued_at DESC);
CREATE INDEX workloads_node_idx    ON workloads (node_id);

-- Audit log for security-relevant events.
CREATE TABLE audit_log (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    actor      TEXT NOT NULL,   -- api key name, "system", node name
    action     TEXT NOT NULL,   -- e.g. "node.approve", "key.create"
    target     TEXT NOT NULL DEFAULT '',
    meta       JSONB NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX audit_log_created_idx ON audit_log (created_at DESC);

-- Cluster-level configuration.
CREATE TABLE settings (
    key        TEXT PRIMARY KEY,
    value      JSONB NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO settings (key, value) VALUES
    ('scheduling', '{
        "queue_timeout_ms": 30000,
        "max_attempts": 2,
        "heartbeat_timeout_seconds": 45,
        "weights": {
            "model_loaded": 3.0,
            "memory_headroom": 2.0,
            "gpu_util": 1.5,
            "cpu_util": 1.0,
            "active_requests": 2.0,
            "latency": 0.5,
            "preferred": 2.0
        }
    }'),
    ('privacy', '{"allow_remote_nodes": true, "required_labels": {}}'),
    ('cluster',   '{"name": "localmesh", "default_max_concurrent": 2}');
