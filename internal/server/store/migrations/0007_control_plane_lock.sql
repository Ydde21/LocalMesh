-- Single-active control-plane ownership (v0.6).
--
-- One row, fixed id=1. The owning process holds a session-scoped
-- pg_advisory_lock on a dedicated connection (auto-released on conn
-- death) AND bumps `epoch` on acquisition. `epoch` is the fencing
-- token: authoritative writes (model-op claims) carry the holder's
-- epoch and match zero rows once a successor bumps it, so a stale
-- process that lost the advisory lock cannot still dispatch work.
--
-- heartbeat_at is observational (how fresh the owner is) — the
-- advisory lock itself is the liveness authority.
CREATE TABLE control_plane_lock (
    id SMALLINT PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    owner_id UUID NOT NULL,
    epoch BIGINT NOT NULL,
    started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    heartbeat_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
INSERT INTO control_plane_lock (id, owner_id, epoch) VALUES (1, gen_random_uuid(), 0);
