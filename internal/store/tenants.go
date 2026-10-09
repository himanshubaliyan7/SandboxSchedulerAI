package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Tenant is one row of the tenants table.
type Tenant struct {
	ID            string
	Name          string
	MaxConcurrent int     // concurrent ASSIGNED+RUNNING+PAUSED sandboxes, enforced by Claim
	RatePerSec    float64 // API token-bucket refill rate
	Burst         int     // API token-bucket size
	Weight        int     // scheduling weight; zero is stored as 1
	CreatedAt     time.Time
}

const tenantCols = `id, name, max_concurrent, rate_per_sec, burst, weight, created_at`

func scanTenant(row pgx.Row) (Tenant, error) {
	var t Tenant
	err := row.Scan(&t.ID, &t.Name, &t.MaxConcurrent, &t.RatePerSec, &t.Burst, &t.Weight, &t.CreatedAt)
	return t, err
}

// CreateTenant inserts a tenant.
func (s *Store) CreateTenant(ctx context.Context, t Tenant) (Tenant, error) {
	if t.ID == "" {
		return Tenant{}, errors.New("store: tenant needs an id")
	}
	if t.Weight <= 0 {
		t.Weight = 1
	}
	out, err := scanTenant(s.pool.QueryRow(ctx, `INSERT INTO tenants (id, name, max_concurrent, rate_per_sec, burst, weight)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING `+tenantCols,
		t.ID, t.Name, t.MaxConcurrent, t.RatePerSec, t.Burst, t.Weight))
	if err != nil {
		if isUniqueViolation(err) {
			return Tenant{}, fmt.Errorf("%w: tenant %s already exists", ErrConflict, t.ID)
		}
		return Tenant{}, fmt.Errorf("store: create tenant: %w", err)
	}
	return out, nil
}

// UpdateTenant replaces a tenant's name, quota, rate limit and weight.
func (s *Store) UpdateTenant(ctx context.Context, t Tenant) (Tenant, error) {
	if t.Weight <= 0 {
		t.Weight = 1
	}
	out, err := scanTenant(s.pool.QueryRow(ctx, `UPDATE tenants
		SET name = $2, max_concurrent = $3, rate_per_sec = $4, burst = $5, weight = $6
		WHERE id = $1 RETURNING `+tenantCols,
		t.ID, t.Name, t.MaxConcurrent, t.RatePerSec, t.Burst, t.Weight))
	if errors.Is(err, pgx.ErrNoRows) {
		return Tenant{}, fmt.Errorf("%w: tenant %s", ErrNotFound, t.ID)
	}
	if err != nil {
		return Tenant{}, fmt.Errorf("store: update tenant: %w", err)
	}
	return out, nil
}

// GetTenant returns one tenant.
func (s *Store) GetTenant(ctx context.Context, id string) (Tenant, error) {
	t, err := scanTenant(s.pool.QueryRow(ctx, `SELECT `+tenantCols+` FROM tenants WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Tenant{}, fmt.Errorf("%w: tenant %s", ErrNotFound, id)
	}
	if err != nil {
		return Tenant{}, fmt.Errorf("store: get tenant %s: %w", id, err)
	}
	return t, nil
}

// ListTenants returns every tenant ordered by id.
func (s *Store) ListTenants(ctx context.Context) ([]Tenant, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+tenantCols+` FROM tenants ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: list tenants: %w", err)
	}
	defer rows.Close()
	var out []Tenant
	for rows.Next() {
		t, err := scanTenant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// DeleteTenant removes a tenant and, by cascade, its API keys. Sandbox
// rows and events keep the tenant id for audit; the caller should release
// the tenant's live sandboxes first.
func (s *Store) DeleteTenant(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM tenants WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("store: delete tenant %s: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: tenant %s", ErrNotFound, id)
	}
	return nil
}

// --- API keys ------------------------------------------------------------

// APIKey is one row of api_keys. The plaintext key is never stored.
type APIKey struct {
	ID        string
	TenantID  string
	Prefix    string // short, non-secret lookup prefix
	KeyHash   []byte // SHA-256 of the full plaintext key
	CreatedAt time.Time
	RevokedAt *time.Time
}

// APIKeyScheme is the leading tag of every generated key.
const APIKeyScheme = "sbx"

// HashAPIKey returns the SHA-256 of a plaintext key. Keys are 256-bit
// random strings, so a fast unsalted hash is appropriate (this is not a
// password).
func HashAPIKey(plaintext string) []byte {
	h := sha256.Sum256([]byte(plaintext))
	return h[:]
}

// GenerateAPIKey returns a new random key of the form
// "sbx_<8 hex prefix>_<64 hex secret>" and its lookup prefix.
func GenerateAPIKey() (plaintext, prefix string, err error) {
	buf := make([]byte, 36)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("store: generate api key: %w", err)
	}
	prefix = hex.EncodeToString(buf[:4])
	return APIKeyScheme + "_" + prefix + "_" + hex.EncodeToString(buf[4:]), prefix, nil
}

// APIKeyPrefix extracts the lookup prefix from a plaintext key.
func APIKeyPrefix(plaintext string) (string, bool) {
	parts := strings.Split(plaintext, "_")
	if len(parts) != 3 || parts[0] != APIKeyScheme || parts[1] == "" || parts[2] == "" {
		return "", false
	}
	return parts[1], true
}

const apiKeyCols = `id, tenant_id, prefix, key_hash, created_at, revoked_at`

func scanAPIKey(row pgx.Row) (APIKey, error) {
	var k APIKey
	err := row.Scan(&k.ID, &k.TenantID, &k.Prefix, &k.KeyHash, &k.CreatedAt, &k.RevokedAt)
	return k, err
}

// CreateAPIKey generates a key for a tenant, stores its hash and prefix,
// and returns the plaintext exactly once.
func (s *Store) CreateAPIKey(ctx context.Context, tenantID string) (plaintext string, key APIKey, err error) {
	plaintext, prefix, err := GenerateAPIKey()
	if err != nil {
		return "", APIKey{}, err
	}
	idBuf := make([]byte, 8)
	if _, err := rand.Read(idBuf); err != nil {
		return "", APIKey{}, fmt.Errorf("store: generate api key id: %w", err)
	}
	key, err = s.InsertAPIKey(ctx, APIKey{
		ID: "key_" + hex.EncodeToString(idBuf), TenantID: tenantID, Prefix: prefix, KeyHash: HashAPIKey(plaintext),
	})
	if err != nil {
		return "", APIKey{}, err
	}
	return plaintext, key, nil
}

// InsertAPIKey stores a key row built by the caller (ID, TenantID, Prefix
// and KeyHash must be set).
func (s *Store) InsertAPIKey(ctx context.Context, k APIKey) (APIKey, error) {
	if k.ID == "" || k.TenantID == "" || k.Prefix == "" || len(k.KeyHash) == 0 {
		return APIKey{}, errors.New("store: api key needs id, tenant id, prefix and hash")
	}
	out, err := scanAPIKey(s.pool.QueryRow(ctx, `INSERT INTO api_keys (id, tenant_id, prefix, key_hash)
		VALUES ($1, $2, $3, $4) RETURNING `+apiKeyCols, k.ID, k.TenantID, k.Prefix, k.KeyHash))
	if err != nil {
		var pgErr interface{ SQLState() string }
		switch {
		case isUniqueViolation(err):
			return APIKey{}, fmt.Errorf("%w: api key already exists", ErrConflict)
		case errors.As(err, &pgErr) && pgErr.SQLState() == "23503": // foreign_key_violation
			return APIKey{}, fmt.Errorf("%w: tenant %s", ErrNotFound, k.TenantID)
		}
		return APIKey{}, fmt.Errorf("store: insert api key: %w", err)
	}
	return out, nil
}

// AuthenticateAPIKey resolves a plaintext key to its row and tenant. It
// finds candidates by prefix and compares hashes in constant time. A
// malformed, unknown or revoked key returns ErrNotFound.
func (s *Store) AuthenticateAPIKey(ctx context.Context, plaintext string) (APIKey, Tenant, error) {
	prefix, ok := APIKeyPrefix(plaintext)
	if !ok {
		return APIKey{}, Tenant{}, fmt.Errorf("%w: api key", ErrNotFound)
	}
	rows, err := s.pool.Query(ctx, `SELECT `+apiKeyCols+` FROM api_keys WHERE prefix = $1 AND revoked_at IS NULL`, prefix)
	if err != nil {
		return APIKey{}, Tenant{}, fmt.Errorf("store: look up api key: %w", err)
	}
	var (
		want  = HashAPIKey(plaintext)
		found *APIKey
	)
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			rows.Close()
			return APIKey{}, Tenant{}, err
		}
		if subtle.ConstantTimeCompare(k.KeyHash, want) == 1 {
			found = &k
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return APIKey{}, Tenant{}, fmt.Errorf("store: look up api key: %w", err)
	}
	if found == nil {
		return APIKey{}, Tenant{}, fmt.Errorf("%w: api key", ErrNotFound)
	}
	tenant, err := s.GetTenant(ctx, found.TenantID)
	if err != nil {
		return APIKey{}, Tenant{}, err
	}
	return *found, tenant, nil
}

// ListAPIKeys lists a tenant's keys, including revoked ones, oldest first.
func (s *Store) ListAPIKeys(ctx context.Context, tenantID string) ([]APIKey, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+apiKeyCols+` FROM api_keys WHERE tenant_id = $1 ORDER BY created_at, id`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("store: list api keys: %w", err)
	}
	defer rows.Close()
	var out []APIKey
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// RevokeAPIKey marks a key revoked. Revoking twice is not an error.
func (s *Store) RevokeAPIKey(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE api_keys SET revoked_at = COALESCE(revoked_at, now()) WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("store: revoke api key %s: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: api key %s", ErrNotFound, id)
	}
	return nil
}
