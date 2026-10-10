-- Release registry: admin-approved signed releases + trusted signing
-- keys. Registration validates the detached signature against a
-- registered key BEFORE the row lands — an unsigned release can never
-- enter the registry, so a dispatch can never reference one.

CREATE TABLE release_keys (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    key_id      text NOT NULL UNIQUE,      -- signer identity: minisign key id or pubkey fingerprint
    kind        text NOT NULL,             -- 'minisign' | 'openssl'
    public_key  text NOT NULL,             -- base64 (minisign) or PEM (openssl)
    comment     text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now(),
    revoked_at  timestamptz
);

CREATE TABLE releases (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    version           text NOT NULL,       -- semver string from the manifest
    channel           text NOT NULL DEFAULT 'stable',
    base_url          text NOT NULL,       -- where artifacts live (https, or loopback http for mirrors)
    manifest_json     jsonb NOT NULL,      -- verbatim release-manifest.json
    manifest_sha256   text NOT NULL,       -- sha256 of manifest_json bytes
    sums_sha256       text NOT NULL,       -- sha256 of SHA256SUMS bytes
    signatures        jsonb NOT NULL,      -- {"SHA256SUMS.minisig": "…", "release-manifest.json.minisig": "…"}
    signer_key_id     text NOT NULL REFERENCES release_keys(key_id),
    protocol_version  int  NOT NULL,
    min_agent_protocol int NOT NULL,
    platforms         text[] NOT NULL,     -- 'agent/darwin/arm64', 'server/linux/amd64', …
    notes             text NOT NULL DEFAULT '',
    status            text NOT NULL DEFAULT 'approved',  -- approved | revoked
    created_by        text NOT NULL DEFAULT '',
    created_at        timestamptz NOT NULL DEFAULT now(),
    UNIQUE (version, channel)
);
CREATE INDEX releases_status_idx ON releases (status);
