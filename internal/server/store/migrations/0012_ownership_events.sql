-- Control-plane ownership history: acquisition, heartbeat loss,
-- promotion, clean release. The audit trail for standby/failover
-- operations and diagnostics ("who was authoritative when").

CREATE TABLE ownership_events (
    id         bigserial PRIMARY KEY,
    owner_id   uuid,
    epoch      bigint,
    event      text NOT NULL,   -- acquired|released|lost|promoted|refused
    detail     text NOT NULL DEFAULT '',
    at         timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ownership_events_at_idx ON ownership_events (at DESC);
