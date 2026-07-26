package store

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/augusto-dmh/herald/internal/herald"
)

const tenantColumns = `id, name, created_at`

// CreateTenant inserts a tenant and returns it with the timestamp the
// database assigned.
func (s *Store) CreateTenant(ctx context.Context, t herald.Tenant) (herald.Tenant, error) {
	return createTenant(ctx, s.pool, t)
}

// CreateTenantTx inserts a tenant in the caller's transaction, so a
// tenant and its first API key are created together or not at all.
func (s *Store) CreateTenantTx(ctx context.Context, tx pgx.Tx, t herald.Tenant) (herald.Tenant, error) {
	return createTenant(ctx, tx, t)
}

func createTenant(ctx context.Context, q querier, t herald.Tenant) (herald.Tenant, error) {
	var out herald.Tenant
	err := q.QueryRow(ctx, `
		INSERT INTO tenants (id, name)
		VALUES ($1, $2)
		RETURNING `+tenantColumns,
		t.ID, t.Name).Scan(&out.ID, &out.Name, &out.CreatedAt)
	if err != nil {
		return herald.Tenant{}, wrap("create tenant", err)
	}
	return out, nil
}

//nolint:gosec // G101: a column list, not a credential
const apiKeyColumns = `id, tenant_id, key_hash, prefix, scope, last_used_at, created_at`

func scanAPIKey(row pgx.Row) (herald.APIKey, error) {
	var k herald.APIKey
	err := row.Scan(&k.ID, &k.TenantID, &k.Hash, &k.Prefix, &k.Scope, &k.LastUsedAt, &k.CreatedAt)
	return k, err
}

// CreateAPIKey stores a credential. Only what the key's record carries
// is written, which is a hash and a display prefix: this package has no
// way to persist a plaintext key even if one were handed to it.
func (s *Store) CreateAPIKey(ctx context.Context, k herald.APIKey) (herald.APIKey, error) {
	return createAPIKey(ctx, s.pool, k)
}

// CreateAPIKeyTx stores a credential in the caller's transaction.
func (s *Store) CreateAPIKeyTx(ctx context.Context, tx pgx.Tx, k herald.APIKey) (herald.APIKey, error) {
	return createAPIKey(ctx, tx, k)
}

func createAPIKey(ctx context.Context, q querier, k herald.APIKey) (herald.APIKey, error) {
	out, err := scanAPIKey(q.QueryRow(ctx, `
		INSERT INTO api_keys (id, tenant_id, key_hash, prefix, scope)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING `+apiKeyColumns,
		k.ID, k.TenantID, k.Hash, k.Prefix, k.Scope))
	if err != nil {
		return herald.APIKey{}, wrap("create api key", err)
	}
	return out, nil
}

// APIKeyByHash resolves a presented credential to the tenant and scope
// it grants. The caller hashes the plaintext; the hash is what this
// looks up, so an attacker with database access holds no usable key.
func (s *Store) APIKeyByHash(ctx context.Context, hash []byte) (herald.APIKey, error) {
	out, err := scanAPIKey(s.pool.QueryRow(ctx, `
		SELECT `+apiKeyColumns+`
		FROM api_keys
		WHERE key_hash = $1`, hash))
	if err != nil {
		return herald.APIKey{}, wrap("read api key", err)
	}
	return out, nil
}

// TouchAPIKey records that a key was used, for operators auditing which
// credentials are still live.
func (s *Store) TouchAPIKey(ctx context.Context, id uuid.UUID, at time.Time) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE api_keys SET last_used_at = $2 WHERE id = $1`, id, at)
	if err != nil {
		return wrap("touch api key", err)
	}
	if tag.RowsAffected() == 0 {
		return wrap("touch api key", pgx.ErrNoRows)
	}
	return nil
}
