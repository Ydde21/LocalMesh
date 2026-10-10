package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/localmesh/localmesh/internal/contracts"
)

// ---------------------------------------------------------------------------
// API keys
// ---------------------------------------------------------------------------

type APIKey struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	Role       string     `json:"role"` // admin | inference | readonly
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

func scanKey(row pgx.Row) (*APIKey, error) {
	var k APIKey
	err := row.Scan(&k.ID, &k.Name, &k.Prefix, &k.Role, &k.LastUsedAt, &k.RevokedAt, &k.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &k, nil
}

func (d *DB) CreateAPIKey(ctx context.Context, name, prefix, hash, role string) (*APIKey, error) {
	return scanKey(d.Pool.QueryRow(ctx,
		`INSERT INTO api_keys (name,prefix,key_hash,role) VALUES ($1,$2,$3,$4)
		 RETURNING id,name,prefix,role,last_used_at,revoked_at,created_at`,
		name, prefix, hash, role))
}

func (d *DB) GetAPIKeyByHash(ctx context.Context, hash string) (*APIKey, error) {
	return scanKey(d.Pool.QueryRow(ctx,
		`SELECT id,name,prefix,role,last_used_at,revoked_at,created_at
		 FROM api_keys WHERE key_hash=$1`, hash))
}

func (d *DB) ListAPIKeys(ctx context.Context) ([]*APIKey, error) {
	rows, err := d.Pool.Query(ctx,
		`SELECT id,name,prefix,role,last_used_at,revoked_at,created_at
		 FROM api_keys ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*APIKey
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (d *DB) RevokeAPIKey(ctx context.Context, id string) error {
	tag, err := d.Pool.Exec(ctx,
		`UPDATE api_keys SET revoked_at=now() WHERE id=$1 AND revoked_at IS NULL`, id)
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

func (d *DB) TouchAPIKey(ctx context.Context, id string) {
	d.Pool.Exec(ctx, `UPDATE api_keys SET last_used_at=now() WHERE id=$1`, id)
}

// ---------------------------------------------------------------------------
// Enrollment tokens
// ---------------------------------------------------------------------------

type EnrollmentToken struct {
	ID        string     `json:"id"`
	Label     string     `json:"label"`
	Prefix    string     `json:"prefix"`
	MaxUses   int        `json:"max_uses"`
	Uses      int        `json:"uses"`
	ExpiresAt time.Time  `json:"expires_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

func scanEnrollToken(row pgx.Row) (*EnrollmentToken, error) {
	var t EnrollmentToken
	err := row.Scan(&t.ID, &t.Label, &t.Prefix, &t.MaxUses, &t.Uses,
		&t.ExpiresAt, &t.RevokedAt, &t.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &t, nil
}

func (d *DB) CreateEnrollmentToken(ctx context.Context, label, prefix, hash string, maxUses int, ttl time.Duration) (*EnrollmentToken, error) {
	return scanEnrollToken(d.Pool.QueryRow(ctx,
		`INSERT INTO enrollment_tokens (label,prefix,token_hash,max_uses,expires_at)
		 VALUES ($1,$2,$3,$4, now() + $5::interval)
		 RETURNING id,label,prefix,max_uses,uses,expires_at,revoked_at,created_at`,
		label, prefix, hash, maxUses, ttl.String()))
}

func (d *DB) ListEnrollmentTokens(ctx context.Context) ([]*EnrollmentToken, error) {
	rows, err := d.Pool.Query(ctx,
		`SELECT id,label,prefix,max_uses,uses,expires_at,revoked_at,created_at
		 FROM enrollment_tokens ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*EnrollmentToken
	for rows.Next() {
		t, err := scanEnrollToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (d *DB) RevokeEnrollmentToken(ctx context.Context, id string) error {
	tag, err := d.Pool.Exec(ctx,
		`UPDATE enrollment_tokens SET revoked_at=now() WHERE id=$1 AND revoked_at IS NULL`, id)
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

// ConsumeEnrollmentToken atomically checks validity and increments use
// count. Returns the token record if consumption succeeded.
func (d *DB) ConsumeEnrollmentToken(ctx context.Context, hash string) (*EnrollmentToken, error) {
	return scanEnrollToken(d.Pool.QueryRow(ctx,
		`UPDATE enrollment_tokens SET uses = uses + 1
		 WHERE token_hash=$1 AND revoked_at IS NULL AND expires_at > now()
		   AND uses < max_uses
		 RETURNING id,label,prefix,max_uses,uses,expires_at,revoked_at,created_at`, hash))
}

// ---------------------------------------------------------------------------
// Enrollment requests
// ---------------------------------------------------------------------------

type EnrollmentRequest struct {
	ID        string                 `json:"id"`
	TokenID   string                 `json:"token_id"`
	Name      string                 `json:"name"`
	Hardware  contracts.HardwareInfo `json:"hardware"`
	Status    string                 `json:"status"` // pending | approved | denied
	NodeID    *string                `json:"node_id,omitempty"`
	CreatedAt time.Time              `json:"created_at"`
	DecidedAt *time.Time             `json:"decided_at,omitempty"`
}

func (d *DB) CreateEnrollmentRequest(ctx context.Context, tokenID, name string, hw contracts.HardwareInfo, pollHash, nodeTokenHash string) (*EnrollmentRequest, error) {
	hwJ, _ := json.Marshal(hw)
	var r EnrollmentRequest
	var nodeID *string
	err := d.Pool.QueryRow(ctx,
		`INSERT INTO enrollment_requests (token_id,name,hardware,poll_secret_hash,node_token_hash)
		 VALUES ($1,$2,$3,$4,$5)
		 RETURNING id,token_id,name,status,node_id,created_at,decided_at`,
		tokenID, name, hwJ, pollHash, nodeTokenHash).Scan(&r.ID, &r.TokenID, &r.Name, &r.Status, &nodeID, &r.CreatedAt, &r.DecidedAt)
	if err != nil {
		return nil, err
	}
	r.NodeID = nodeID
	r.Hardware = hw
	return &r, nil
}

// GetEnrollmentRequestForPoll resolves an agent poll: request id + secret.
func (d *DB) GetEnrollmentRequestForPoll(ctx context.Context, id, pollHash string) (*EnrollmentRequest, error) {
	var r EnrollmentRequest
	var hw []byte
	err := d.Pool.QueryRow(ctx,
		`SELECT id,token_id,name,hardware,status,node_id,created_at,decided_at
		 FROM enrollment_requests WHERE id=$1 AND poll_secret_hash=$2`,
		id, pollHash).Scan(&r.ID, &r.TokenID, &r.Name, &hw, &r.Status, &r.NodeID, &r.CreatedAt, &r.DecidedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	json.Unmarshal(hw, &r.Hardware)
	return &r, nil
}

// GetEnrollmentRequest fetches a single request by id. Used by approve/
// deny so a request buried past the list cap (200) stays reachable.
func (d *DB) GetEnrollmentRequest(ctx context.Context, id string) (*EnrollmentRequest, error) {
	var r EnrollmentRequest
	var hw []byte
	err := d.Pool.QueryRow(ctx,
		`SELECT id,token_id,name,hardware,status,node_id,created_at,decided_at
		 FROM enrollment_requests WHERE id=$1`, id).
		Scan(&r.ID, &r.TokenID, &r.Name, &hw, &r.Status, &r.NodeID, &r.CreatedAt, &r.DecidedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	json.Unmarshal(hw, &r.Hardware)
	return &r, nil
}

func (d *DB) ListEnrollmentRequests(ctx context.Context, status string) ([]*EnrollmentRequest, error) {
	q := `SELECT id,token_id,name,hardware,status,node_id,created_at,decided_at
	      FROM enrollment_requests`
	args := []any{}
	if status != "" {
		q += ` WHERE status=$1`
		args = append(args, status)
	}
	q += ` ORDER BY created_at DESC LIMIT 200`
	rows, err := d.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*EnrollmentRequest
	for rows.Next() {
		var r EnrollmentRequest
		var hw []byte
		if err := rows.Scan(&r.ID, &r.TokenID, &r.Name, &hw, &r.Status, &r.NodeID, &r.CreatedAt, &r.DecidedAt); err != nil {
			return nil, err
		}
		json.Unmarshal(hw, &r.Hardware)
		out = append(out, &r)
	}
	return out, rows.Err()
}

// EnrollmentRequestNodeTokenHash returns the agent-supplied credential
// hash that should be installed on the approved node.
func (d *DB) EnrollmentRequestNodeTokenHash(ctx context.Context, reqID string) (string, error) {
	var h string
	err := d.Pool.QueryRow(ctx,
		`SELECT node_token_hash FROM enrollment_requests WHERE id=$1`, reqID).Scan(&h)
	return h, err
}

// ApproveEnrollmentRequest creates the node record and links it. The
// plaintext node token is generated by the caller; only its hash is stored.
func (d *DB) ApproveEnrollmentRequest(ctx context.Context, reqID, nodeName, nodeTokenHash string, hw contracts.HardwareInfo, maxConcurrent int) (*Node, error) {
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	hwJ, _ := json.Marshal(hw)
	var nodeID string
	err = tx.QueryRow(ctx,
		`INSERT INTO nodes (name, token_hash, hardware, status, max_concurrent)
		 VALUES ($1,$2,$3,'approved',$4) RETURNING id`,
		nodeName, nodeTokenHash, hwJ, maxConcurrent).Scan(&nodeID)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, ErrDuplicateCredential
		}
		return nil, err
	}
	tag, err := tx.Exec(ctx,
		`UPDATE enrollment_requests
		 SET status='approved', node_id=$2, decided_at=now()
		 WHERE id=$1 AND status='pending'`, reqID, nodeID)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrAlreadyDecided
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return d.GetNode(ctx, nodeID)
}

func (d *DB) DenyEnrollmentRequest(ctx context.Context, reqID string) error {
	tag, err := d.Pool.Exec(ctx,
		`UPDATE enrollment_requests SET status='denied', decided_at=now()
		 WHERE id=$1 AND status='pending'`, reqID)
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

// ---------------------------------------------------------------------------
// Dashboard session revocation (durable denylist — survives restarts)
// ---------------------------------------------------------------------------

// RevokeSessionNonce records a logged-out session nonce until its
// cookie expires. Insert is idempotent (logout is retryable).
func (d *DB) RevokeSessionNonce(ctx context.Context, nonce string, expiresAt time.Time) error {
	_, err := d.Pool.Exec(ctx,
		`INSERT INTO session_revocations (nonce, expires_at) VALUES ($1,$2)
		 ON CONFLICT (nonce) DO NOTHING`, nonce, expiresAt)
	return err
}

// IsSessionRevoked reports whether a nonce was explicitly revoked.
func (d *DB) IsSessionRevoked(ctx context.Context, nonce string) (bool, error) {
	var exists bool
	err := d.Pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM session_revocations WHERE nonce=$1 AND expires_at > now())`,
		nonce).Scan(&exists)
	return exists, err
}

// SweepSessionRevocations deletes rows past their cookie expiry — the
// tokens they covered can no longer be valid anyway.
func (d *DB) SweepSessionRevocations(ctx context.Context) error {
	_, err := d.Pool.Exec(ctx, `DELETE FROM session_revocations WHERE expires_at <= now()`)
	return err
}
