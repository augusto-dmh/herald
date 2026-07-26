package httpapi

import (
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/augusto-dmh/herald/internal/herald"
)

type createTenantRequest struct {
	Name string `json:"name"`
}

type tenantResponse struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// apiKeyResponse carries the one and only copy of a key's plaintext.
// Nothing else in this API has a Key field, and nothing in the database
// can produce one: this response is the single moment the secret exists
// outside the caller's hands.
type apiKeyResponse struct {
	ID        uuid.UUID    `json:"id"`
	Key       string       `json:"key"`
	Prefix    string       `json:"prefix"`
	Scope     herald.Scope `json:"scope"`
	CreatedAt time.Time    `json:"created_at"`
}

type createTenantResponse struct {
	Tenant tenantResponse `json:"tenant"`
	APIKey apiKeyResponse `json:"api_key"`
}

// createTenant bootstraps a tenant and the key that speaks for it. The
// two are written in one transaction: a tenant nobody holds a key for
// is unreachable, and this is the only route that can issue its first
// one.
func (s *Server) createTenant(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()

	var req createTenantRequest
	if err := decodeJSON(r, &req); err != nil {
		return err
	}
	id, err := herald.NewID()
	if err != nil {
		return err
	}
	tenant := herald.Tenant{ID: id, Name: strings.TrimSpace(req.Name)}
	if err := tenant.Validate(); err != nil {
		return err
	}
	generated, err := herald.NewAPIKey(tenant.ID, herald.ScopeFull)
	if err != nil {
		return err
	}

	tx, err := s.store.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tenant, err = s.store.CreateTenantTx(ctx, tx, tenant)
	if err != nil {
		return err
	}
	key, err := s.store.CreateAPIKeyTx(ctx, tx, generated.Key)
	if err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}

	s.writeJSON(w, http.StatusCreated, createTenantResponse{
		Tenant: tenantResponse{ID: tenant.ID, Name: tenant.Name, CreatedAt: tenant.CreatedAt},
		APIKey: apiKeyResponse{
			ID:        key.ID,
			Key:       generated.Plaintext,
			Prefix:    key.Prefix,
			Scope:     key.Scope,
			CreatedAt: key.CreatedAt,
		},
	})
	return nil
}
