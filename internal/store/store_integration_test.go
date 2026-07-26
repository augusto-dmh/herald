//go:build integration

package store_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/augusto-dmh/drover"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/augusto-dmh/herald/internal/herald"
	"github.com/augusto-dmh/herald/internal/migrate"
	"github.com/augusto-dmh/herald/internal/store"
	"github.com/augusto-dmh/herald/internal/testdb"
)

func TestMain(m *testing.M) { os.Exit(testdb.RunMain(m)) }

// newStore returns a store over a database carrying both herald's
// schema and the queue's, since ingest writes to both.
func newStore(t *testing.T) (*store.Store, *pgxpool.Pool) {
	t.Helper()
	pool := testdb.NewDB(t, migrate.Migrate, drover.Migrate)
	return store.New(pool), pool
}

// tenantFixture is a tenant with one application, the smallest setting
// in which anything else can be created.
type tenantFixture struct {
	tenant      herald.Tenant
	application herald.Application
}

func newTenant(t *testing.T, s *store.Store, name, appUID string) tenantFixture {
	t.Helper()
	ctx := context.Background()

	tenant, err := s.CreateTenant(ctx, herald.Tenant{ID: newID(t), Name: name})
	if err != nil {
		t.Fatalf("create tenant %s: %v", name, err)
	}
	application, err := s.CreateApplication(ctx, herald.Application{
		ID: newID(t), TenantID: tenant.ID, UID: appUID, Name: name + " app",
	})
	if err != nil {
		t.Fatalf("create application %s: %v", appUID, err)
	}
	return tenantFixture{tenant: tenant, application: application}
}

func newID(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := herald.NewID()
	if err != nil {
		t.Fatalf("new id: %v", err)
	}
	return id
}

func newEndpoint(
	t *testing.T, s *store.Store, f tenantFixture, url string, filters []string, disabled bool,
) herald.Endpoint {
	t.Helper()
	e, err := s.CreateEndpoint(context.Background(), herald.Endpoint{
		ID:            newID(t),
		TenantID:      f.tenant.ID,
		ApplicationID: f.application.ID,
		URL:           url,
		FilterTypes:   filters,
		Disabled:      disabled,
	})
	if err != nil {
		t.Fatalf("create endpoint %s: %v", url, err)
	}
	return e
}

func endpointURLs(endpoints []herald.Endpoint) []string {
	urls := make([]string, 0, len(endpoints))
	for _, e := range endpoints {
		urls = append(urls, e.URL)
	}
	slices.Sort(urls)
	return urls
}

func TestAnAPIKeyIsStoredAsAHashAndResolvesBackToItsTenantAndScope(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	f := newTenant(t, s, "acme", "billing")

	generated, err := herald.NewAPIKey(f.tenant.ID, herald.ScopeIngest)
	if err != nil {
		t.Fatalf("mint api key: %v", err)
	}
	if _, err := s.CreateAPIKey(ctx, generated.Key); err != nil {
		t.Fatalf("store api key: %v", err)
	}

	found, err := s.APIKeyByHash(ctx, herald.HashAPIKey(generated.Plaintext))
	if err != nil {
		t.Fatalf("authenticate with the issued key: %v", err)
	}
	if found.TenantID != f.tenant.ID {
		t.Errorf("key resolved to tenant %s, want %s", found.TenantID, f.tenant.ID)
	}
	if found.Scope != herald.ScopeIngest {
		t.Errorf("key resolved to scope %q, want %q", found.Scope, herald.ScopeIngest)
	}
	if found.LastUsedAt != nil {
		t.Errorf("a never-used key reports last use at %v", found.LastUsedAt)
	}

	// What the database holds must be the hash, and nothing that could
	// be replayed as a credential.
	var storedHash []byte
	var storedPrefix string
	if err := pool.QueryRow(ctx,
		`SELECT key_hash, prefix FROM api_keys WHERE id = $1`, generated.Key.ID).
		Scan(&storedHash, &storedPrefix); err != nil {
		t.Fatalf("read stored key: %v", err)
	}
	want := sha256.Sum256([]byte(generated.Plaintext))
	if !herald.EqualHash(storedHash, want[:]) {
		t.Errorf("stored hash is not the SHA-256 of the issued key")
	}
	secret := strings.TrimPrefix(generated.Plaintext, herald.APIKeyPrefix)
	if strings.Contains(storedPrefix, secret) {
		t.Errorf("the stored display prefix contains the key secret")
	}
	if strings.Contains(string(storedHash), secret) {
		t.Errorf("the stored hash contains the key secret in the clear")
	}
}

func TestAnUnknownKeyResolvesToNothing(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	f := newTenant(t, s, "acme", "billing")

	generated, err := herald.NewAPIKey(f.tenant.ID, herald.ScopeFull)
	if err != nil {
		t.Fatalf("mint api key: %v", err)
	}
	if _, err := s.CreateAPIKey(ctx, generated.Key); err != nil {
		t.Fatalf("store api key: %v", err)
	}

	_, err = s.APIKeyByHash(ctx, herald.HashAPIKey("hrld_live_not-a-real-key"))
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("looking up an unknown key returned %v, want ErrNotFound", err)
	}
}

func TestTouchingAKeyRecordsWhenItWasLastUsed(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	f := newTenant(t, s, "acme", "billing")

	generated, err := herald.NewAPIKey(f.tenant.ID, herald.ScopeFull)
	if err != nil {
		t.Fatalf("mint api key: %v", err)
	}
	stored, err := s.CreateAPIKey(ctx, generated.Key)
	if err != nil {
		t.Fatalf("store api key: %v", err)
	}

	used := stored.CreatedAt.Add(time.Hour)
	if err := s.TouchAPIKey(ctx, stored.ID, used); err != nil {
		t.Fatalf("touch key: %v", err)
	}
	found, err := s.APIKeyByHash(ctx, generated.Key.Hash)
	if err != nil {
		t.Fatalf("read key back: %v", err)
	}
	if found.LastUsedAt == nil || !found.LastUsedAt.Equal(used) {
		t.Errorf("last used at = %v, want %v", found.LastUsedAt, used)
	}

	if err := s.TouchAPIKey(ctx, newID(t), used); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("touching an unknown key returned %v, want ErrNotFound", err)
	}
}

func TestAnApplicationUIDIsTakenOnlyWithinItsOwnTenant(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	acme := newTenant(t, s, "acme", "billing")
	globex := newTenant(t, s, "globex", "billing")

	_, err := s.CreateApplication(ctx, herald.Application{
		ID: newID(t), TenantID: acme.tenant.ID, UID: "billing", Name: "again",
	})
	if !errors.Is(err, store.ErrConflict) {
		t.Errorf("reusing a uid within a tenant returned %v, want ErrConflict", err)
	}

	// The two tenants each got their own "billing" application above.
	mine, err := s.ApplicationByUID(ctx, acme.tenant.ID, "billing")
	if err != nil {
		t.Fatalf("read own application: %v", err)
	}
	if mine.ID != acme.application.ID {
		t.Errorf("uid billing resolved to %s, want the caller's own %s", mine.ID, acme.application.ID)
	}
	theirs, err := s.ApplicationByUID(ctx, globex.tenant.ID, "billing")
	if err != nil {
		t.Fatalf("read the other tenant's application: %v", err)
	}
	if theirs.ID == mine.ID {
		t.Errorf("both tenants resolved uid billing to the same application")
	}
}

// The fan-out rule: an endpoint receives an event when it is enabled
// and either lists that event type or lists none at all.
func TestOnlyEnabledEndpointsSubscribedToTheEventAreSelected(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	f := newTenant(t, s, "acme", "billing")

	newEndpoint(t, s, f, "https://example.com/all", nil, false)
	newEndpoint(t, s, f, "https://example.com/paid", []string{"invoice.paid"}, false)
	newEndpoint(t, s, f, "https://example.com/other", []string{"user.created"}, false)
	newEndpoint(t, s, f, "https://example.com/paid-but-off", []string{"invoice.paid"}, true)
	newEndpoint(t, s, f, "https://example.com/all-but-off", nil, true)

	matched, err := s.EndpointsForEvent(ctx, f.tenant.ID, f.application.ID, "invoice.paid")
	if err != nil {
		t.Fatalf("select endpoints: %v", err)
	}
	want := []string{"https://example.com/all", "https://example.com/paid"}
	if got := endpointURLs(matched); !slices.Equal(got, want) {
		t.Errorf("endpoints for invoice.paid = %v, want %v", got, want)
	}

	matched, err = s.EndpointsForEvent(ctx, f.tenant.ID, f.application.ID, "user.created")
	if err != nil {
		t.Fatalf("select endpoints: %v", err)
	}
	want = []string{"https://example.com/all", "https://example.com/other"}
	if got := endpointURLs(matched); !slices.Equal(got, want) {
		t.Errorf("endpoints for user.created = %v, want %v", got, want)
	}

	// An event type nobody names still reaches the unfiltered endpoint.
	matched, err = s.EndpointsForEvent(ctx, f.tenant.ID, f.application.ID, "nobody.listens")
	if err != nil {
		t.Fatalf("select endpoints: %v", err)
	}
	want = []string{"https://example.com/all"}
	if got := endpointURLs(matched); !slices.Equal(got, want) {
		t.Errorf("endpoints for an unlisted event type = %v, want %v", got, want)
	}
}

func TestEndpointsOfAnotherApplicationAreNotSelected(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	f := newTenant(t, s, "acme", "billing")
	other, err := s.CreateApplication(ctx, herald.Application{
		ID: newID(t), TenantID: f.tenant.ID, UID: "shipping", Name: "Shipping",
	})
	if err != nil {
		t.Fatalf("create second application: %v", err)
	}

	newEndpoint(t, s, f, "https://example.com/billing", nil, false)
	if _, err := s.CreateEndpoint(ctx, herald.Endpoint{
		ID: newID(t), TenantID: f.tenant.ID, ApplicationID: other.ID,
		URL: "https://example.com/shipping",
	}); err != nil {
		t.Fatalf("create endpoint on second application: %v", err)
	}

	matched, err := s.EndpointsForEvent(ctx, f.tenant.ID, f.application.ID, "invoice.paid")
	if err != nil {
		t.Fatalf("select endpoints: %v", err)
	}
	if got := endpointURLs(matched); !slices.Equal(got, []string{"https://example.com/billing"}) {
		t.Errorf("endpoints = %v, want only the addressed application's", got)
	}
}

// Nothing a tenant asks for may ever be answered with another tenant's
// rows, whichever read is used and whoever's identifiers are supplied.
func TestReadsScopedToOneTenantNeverReturnAnothersRows(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	acme := newTenant(t, s, "acme", "billing")
	globex := newTenant(t, s, "globex", "shipping")

	endpoint := newEndpoint(t, s, acme, "https://acme.example.com/hooks", nil, false)
	message, deliveries := ingest(t, s, pool, acme, "invoice.paid", `{"total":10}`)
	if len(deliveries) != 1 {
		t.Fatalf("ingest produced %d deliveries, want 1", len(deliveries))
	}
	if _, err := s.CreateAttempt(ctx, herald.DeliveryAttempt{
		ID: newID(t), TenantID: acme.tenant.ID, DeliveryID: deliveries[0].ID,
		AttemptNumber: 1, Success: true, Duration: 5 * time.Millisecond,
	}); err != nil {
		t.Fatalf("record attempt: %v", err)
	}

	intruder := globex.tenant.ID

	if _, err := s.ApplicationByUID(ctx, intruder, "billing"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("reading another tenant's application returned %v, want ErrNotFound", err)
	}
	if _, err := s.EndpointByID(ctx, intruder, endpoint.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("reading another tenant's endpoint returned %v, want ErrNotFound", err)
	}
	if _, err := s.MessageByID(ctx, intruder, acme.application.ID, message.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("reading another tenant's message returned %v, want ErrNotFound", err)
	}

	found, err := s.EndpointsForEvent(ctx, intruder, acme.application.ID, "invoice.paid")
	if err != nil {
		t.Fatalf("select endpoints as another tenant: %v", err)
	}
	if len(found) != 0 {
		t.Errorf("fan-out as another tenant selected %d endpoints, want 0", len(found))
	}

	gotDeliveries, err := s.DeliveriesByMessage(ctx, intruder, message.ID)
	if err != nil {
		t.Fatalf("read deliveries as another tenant: %v", err)
	}
	if len(gotDeliveries) != 0 {
		t.Errorf("another tenant saw %d deliveries, want 0", len(gotDeliveries))
	}

	gotAttempts, err := s.AttemptsByMessage(ctx, intruder, message.ID)
	if err != nil {
		t.Fatalf("read attempts as another tenant: %v", err)
	}
	if len(gotAttempts) != 0 {
		t.Errorf("another tenant saw %d attempts, want 0", len(gotAttempts))
	}

	// The owner still sees everything, so the checks above are not
	// passing merely because the rows are unreadable.
	if _, err := s.MessageByID(ctx, acme.tenant.ID, acme.application.ID, message.ID); err != nil {
		t.Errorf("the owning tenant could not read its own message: %v", err)
	}
	ownDeliveries, err := s.DeliveriesByMessage(ctx, acme.tenant.ID, message.ID)
	if err != nil || len(ownDeliveries) != 1 {
		t.Errorf("the owning tenant saw %d deliveries (err %v), want 1", len(ownDeliveries), err)
	}
	ownAttempts, err := s.AttemptsByMessage(ctx, acme.tenant.ID, message.ID)
	if err != nil || len(ownAttempts) != 1 {
		t.Errorf("the owning tenant saw %d attempts (err %v), want 1", len(ownAttempts), err)
	}
}
