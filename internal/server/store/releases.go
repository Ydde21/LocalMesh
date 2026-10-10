package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ---------------------------------------------------------------------------
// Release registry (migration 0009)
//
// Trusted signing keys and admin-approved signed releases. Validation
// (signature verify, manifest parse, platform/protocol checks) lives in
// the update orchestrator — this layer is persistence only. A release
// row can exist only when signed: RegisterRelease takes the already-
// verified signature material and the signer key id must reference a
// live release_keys row (FK).
// ---------------------------------------------------------------------------

// ReleaseKey is one trusted release-signing public key.
type ReleaseKey struct {
	ID        string     `json:"id"`
	KeyID     string     `json:"key_id"`
	Kind      string     `json:"kind"` // minisign | openssl
	PublicKey string     `json:"public_key"`
	Comment   string     `json:"comment"`
	CreatedAt time.Time  `json:"created_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

// Release is one approved signed release in the registry.
type Release struct {
	ID               string            `json:"id"`
	Version          string            `json:"version"`
	Channel          string            `json:"channel"`
	BaseURL          string            `json:"base_url"`
	Manifest         json.RawMessage   `json:"manifest"`
	ManifestSHA256   string            `json:"manifest_sha256"`
	SumsSHA256       string            `json:"sums_sha256"`
	Signatures       map[string]string `json:"signatures"`
	SignerKeyID      string            `json:"signer_key_id"`
	ProtocolVersion  int               `json:"protocol_version"`
	MinAgentProtocol int               `json:"min_agent_protocol"`
	Platforms        []string          `json:"platforms"`
	Notes            string            `json:"notes"`
	Status           string            `json:"status"` // approved | revoked
	CreatedBy        string            `json:"created_by"`
	CreatedAt        time.Time         `json:"created_at"`
}

// ErrDuplicateRelease maps the (version, channel) unique violation.
var ErrDuplicateRelease = errors.New("release already registered for this version+channel")

// ErrDuplicateKey maps the release_keys.key_id unique violation.
var ErrDuplicateKey = errors.New("a signing key with this id is already registered")

func scanReleaseKey(row pgx.Row) (*ReleaseKey, error) {
	var k ReleaseKey
	err := row.Scan(&k.ID, &k.KeyID, &k.Kind, &k.PublicKey, &k.Comment,
		&k.CreatedAt, &k.RevokedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &k, nil
}

func scanRelease(row pgx.Row) (*Release, error) {
	var r Release
	var sigs []byte
	err := row.Scan(&r.ID, &r.Version, &r.Channel, &r.BaseURL, &r.Manifest,
		&r.ManifestSHA256, &r.SumsSHA256, &sigs, &r.SignerKeyID,
		&r.ProtocolVersion, &r.MinAgentProtocol, &r.Platforms, &r.Notes,
		&r.Status, &r.CreatedBy, &r.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	json.Unmarshal(sigs, &r.Signatures)
	if r.Signatures == nil {
		r.Signatures = map[string]string{}
	}
	if r.Platforms == nil {
		r.Platforms = []string{}
	}
	return &r, nil
}

const releaseCols = `id,version,channel,base_url,manifest_json,manifest_sha256,
	sums_sha256,signatures,signer_key_id,protocol_version,min_agent_protocol,
	platforms,notes,status,created_by,created_at`

// AddReleaseKey registers a trusted signing key. KeyID is derived by the
// caller from the key material (minisign keynum hex / PEM sha256 fp).
func (d *DB) AddReleaseKey(ctx context.Context, keyID, kind, pub, comment string) (*ReleaseKey, error) {
	k, err := scanReleaseKey(d.Pool.QueryRow(ctx,
		`INSERT INTO release_keys (key_id,kind,public_key,comment)
		 VALUES ($1,$2,$3,$4)
		 RETURNING id,key_id,kind,public_key,comment,created_at,revoked_at`,
		keyID, kind, pub, comment))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, ErrDuplicateKey
		}
		return nil, err
	}
	return k, nil
}

func (d *DB) GetReleaseKey(ctx context.Context, keyID string) (*ReleaseKey, error) {
	return scanReleaseKey(d.Pool.QueryRow(ctx,
		`SELECT id,key_id,kind,public_key,comment,created_at,revoked_at
		 FROM release_keys WHERE key_id=$1 AND revoked_at IS NULL`, keyID))
}

func (d *DB) ListReleaseKeys(ctx context.Context) ([]*ReleaseKey, error) {
	rows, err := d.Pool.Query(ctx,
		`SELECT id,key_id,kind,public_key,comment,created_at,revoked_at
		 FROM release_keys ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*ReleaseKey{}
	for rows.Next() {
		k, err := scanReleaseKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// RevokeReleaseKey retires a key. Existing signed releases stay
// registered (they were verified at registration), but nothing new can
// be signed by it. Key deletion is refused while referenced releases
// exist — revocation is the audit-preserving path.
func (d *DB) RevokeReleaseKey(ctx context.Context, keyID string) error {
	tag, err := d.Pool.Exec(ctx,
		`UPDATE release_keys SET revoked_at=now() WHERE key_id=$1 AND revoked_at IS NULL`, keyID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// RegisterRelease persists a validated release. Callers must have
// already verified the signatures against a registered key — the FK on
// signer_key_id is the persistence-level guard, not the proof.
func (d *DB) RegisterRelease(ctx context.Context, r *Release) (*Release, error) {
	sigs, _ := json.Marshal(r.Signatures)
	out, err := scanRelease(d.Pool.QueryRow(ctx,
		`INSERT INTO releases (version,channel,base_url,manifest_json,manifest_sha256,
			sums_sha256,signatures,signer_key_id,protocol_version,min_agent_protocol,
			platforms,notes,created_by)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		 RETURNING `+releaseCols,
		r.Version, r.Channel, r.BaseURL, r.Manifest, r.ManifestSHA256,
		r.SumsSHA256, sigs, r.SignerKeyID, r.ProtocolVersion,
		r.MinAgentProtocol, r.Platforms, r.Notes, r.CreatedBy))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, ErrDuplicateRelease
		}
		return nil, err
	}
	return out, nil
}

func (d *DB) GetRelease(ctx context.Context, id string) (*Release, error) {
	return scanRelease(d.Pool.QueryRow(ctx,
		`SELECT `+releaseCols+` FROM releases WHERE id=$1`, id))
}

func (d *DB) ListReleases(ctx context.Context) ([]*Release, error) {
	rows, err := d.Pool.Query(ctx,
		`SELECT `+releaseCols+` FROM releases ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Release{}
	for rows.Next() {
		r, err := scanRelease(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RevokeRelease marks a release unusable for new rollouts. In-flight
// updates already dispatched keep their verified payloads — revocation
// stops new authorization, it cannot un-send a frame.
func (d *DB) RevokeRelease(ctx context.Context, id string) error {
	tag, err := d.Pool.Exec(ctx,
		`UPDATE releases SET status='revoked' WHERE id=$1 AND status='approved'`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
