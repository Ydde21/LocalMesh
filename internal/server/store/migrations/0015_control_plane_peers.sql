-- Control-plane peer presence + node disk telemetry (v0.10 observability).
--
-- control_plane_peers gives every control-plane PROCESS a presence row —
-- most importantly a waiting standby, which otherwise publishes nothing:
-- it holds no ownership lock (the lock is the authority, not this table),
-- serves a 503-only mux, and appears in no table. Each instance upserts
-- one row keyed by an instance uuid and heartbeats it on a short cadence
-- (store.PeerReporter). Liveness is judged by heartbeat freshness, never
-- by the row's existence: rows are deleted on clean shutdown/promotion,
-- and a crashed peer's leftover row simply goes stale (readers apply
-- store.PeerStaleAfter). started_at records process start for uptime
-- display; hostname identifies which machine the process is on.
CREATE TABLE control_plane_peers (
    instance_id  UUID PRIMARY KEY,
    role         TEXT NOT NULL CHECK (role IN ('active', 'standby')),
    hostname     TEXT NOT NULL DEFAULT '',
    started_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    heartbeat_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- The hot lookup is "freshest row of role R": /control-plane's standby
-- presence block and the peers gauge both read it on every request.
CREATE INDEX control_plane_peers_role_fresh_idx
    ON control_plane_peers (role, heartbeat_at DESC);

-- Node disk telemetry, pushed inside the agent heartbeat's metrics
-- object (wire extension on contracts.Metrics, ~10s cadence). NULL =
-- never measured — pre-v0.10 agents and agent-side probe failures must
-- read as "unknown", never as 0 bytes free. disk_measured_at is stamped
-- server-side on receipt, so consumers get measurement freshness
-- without trusting agent clocks.
ALTER TABLE nodes
    ADD COLUMN disk_free_bytes  BIGINT,
    ADD COLUMN disk_total_bytes BIGINT,
    ADD COLUMN disk_measured_at TIMESTAMPTZ;
