-- Persistent dashboard-session revocation: the web BFF revokes a
-- session nonce on logout; checkSession verifies against this table so
-- revocation survives web-process restarts. Rows self-expire with the
-- cookie's TTL and are swept periodically.
CREATE TABLE IF NOT EXISTS session_revocations (
    nonce       TEXT PRIMARY KEY,
    expires_at  TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS session_revocations_expiry
    ON session_revocations (expires_at);
