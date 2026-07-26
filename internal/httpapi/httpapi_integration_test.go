//go:build integration

package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
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

// bootstrapForTests is the operator credential the test server is
// configured with.
const bootstrapForTests = "operator-bootstrap-value"

// api is the API under test together with the database behind it, so a
// test can assert both what a caller was told and what was stored.
type api struct {
	handler http.Handler
	server  *Server
	store   *store.Store
	pool    *pgxpool.Pool
}

func newAPI(t *testing.T) *api {
	t.Helper()
	discard := slog.New(slog.DiscardHandler)
	pool := testdb.NewDB(t, migrate.Migrate, drover.Migrate)
	s := store.New(pool)

	queue, err := drover.NewClient(pool, drover.Config{Logger: discard})
	if err != nil {
		t.Fatalf("new queue client: %v", err)
	}
	handler := NewServer(s, queue, Config{BootstrapToken: bootstrapForTests, Logger: discard})
	srv, ok := handler.(*Server)
	if !ok {
		t.Fatalf("the API is not served by *Server")
	}
	return &api{handler: handler, server: srv, store: s, pool: pool}
}

// do sends one request carrying the given credential and returns the
// recorded response.
func (a *api) do(t *testing.T, method, path, credential, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, path, reader)
	if credential != "" {
		r.Header.Set("Authorization", "Bearer "+credential)
	}
	rec := httptest.NewRecorder()
	a.handler.ServeHTTP(rec, r)
	return rec
}

// decodeBody reads a response body into dst.
func decodeBody(t *testing.T, rec *httptest.ResponseRecorder, dst any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), dst); err != nil {
		t.Fatalf("response body is not JSON: %v (%s)", err, rec.Body)
	}
}

func newID(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := herald.NewID()
	if err != nil {
		t.Fatalf("new id: %v", err)
	}
	return id
}

// newTenant creates a tenant directly and issues it a key of the given
// scope, for the tests whose subject is not tenant creation.
func (a *api) newTenant(t *testing.T, name string, scope herald.Scope) (herald.Tenant, string) {
	t.Helper()
	ctx := context.Background()

	tenant, err := a.store.CreateTenant(ctx, herald.Tenant{ID: newID(t), Name: name})
	if err != nil {
		t.Fatalf("create tenant %s: %v", name, err)
	}
	generated, err := herald.NewAPIKey(tenant.ID, scope)
	if err != nil {
		t.Fatalf("mint key for %s: %v", name, err)
	}
	if _, err := a.store.CreateAPIKey(ctx, generated.Key); err != nil {
		t.Fatalf("store key for %s: %v", name, err)
	}
	return tenant, generated.Plaintext
}

// A key is answered as the tenant that was issued it, never as any
// other: the tenant a request runs under comes from the credential
// alone and cannot be influenced by anything the caller sends.
func TestAKeyIsAnsweredAsTheTenantThatWasIssuedIt(t *testing.T) {
	a := newAPI(t)
	acme, acmeKey := a.newTenant(t, "acme", herald.ScopeFull)
	globex, globexKey := a.newTenant(t, "globex", herald.ScopeFull)

	var seen caller
	guarded := a.server.requireKey(herald.ScopeFull,
		func(_ http.ResponseWriter, _ *http.Request, c caller) error {
			seen = c
			return nil
		})

	for _, want := range []struct {
		tenant herald.Tenant
		key    string
	}{{acme, acmeKey}, {globex, globexKey}} {
		seen = caller{}
		r := httptest.NewRequest(http.MethodPost, "/v1/anything", nil)
		r.Header.Set("Authorization", "Bearer "+want.key)
		rec := httptest.NewRecorder()
		guarded.ServeHTTP(rec, r)

		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200 (%s)", want.tenant.Name, rec.Code, rec.Body)
		}
		if seen.tenantID != want.tenant.ID {
			t.Errorf("%s's key ran as tenant %s, want %s", want.tenant.Name, seen.tenantID, want.tenant.ID)
		}
		if seen.scope != herald.ScopeFull {
			t.Errorf("%s's key ran with scope %q, want full", want.tenant.Name, seen.scope)
		}
	}
}

// An operator about to revoke a key needs to know whether anything is
// still using it, which is what last_used_at answers. Answering it
// costs one write per key per interval, not one per request: a key
// carrying an application's whole traffic must not turn every call
// into an update of the same row.
func TestUsingAKeyIsRecordedButNotOnceARequest(t *testing.T) {
	a := newAPI(t)
	_, key := a.newTenant(t, "acme", herald.ScopeFull)
	ctx := context.Background()

	lastUsed := func() *time.Time {
		t.Helper()
		stored, err := a.store.APIKeyByHash(ctx, herald.HashAPIKey(key))
		if err != nil {
			t.Fatalf("read the key back: %v", err)
		}
		return stored.LastUsedAt
	}
	authenticate := func() {
		t.Helper()
		if rec := a.do(t, http.MethodPost, "/v1/applications", key,
			`{"uid":"`+uuid.NewString()+`","name":"Billing"}`); rec.Code != http.StatusCreated {
			t.Fatalf("authenticate: status %d (%s)", rec.Code, rec.Body)
		}
	}

	if got := lastUsed(); got != nil {
		t.Fatalf("a key reports last use at %v before it was ever used", got)
	}

	authenticate()
	first := lastUsed()
	if first == nil {
		t.Fatalf("a key that authenticated a request is still recorded as never used")
	}

	authenticate()
	if second := lastUsed(); second == nil || !second.Equal(*first) {
		t.Errorf("last use moved from %v to %v on a request moments later, want one write per %v",
			first, second, apiKeyTouchInterval)
	}

	// Once the record is older than the interval, the next request
	// refreshes it — the throttle delays the write, it does not skip it.
	stale := first.Add(-2 * apiKeyTouchInterval)
	if _, err := a.pool.Exec(ctx,
		`UPDATE api_keys SET last_used_at = $2 WHERE key_hash = $1`,
		herald.HashAPIKey(key), stale); err != nil {
		t.Fatalf("age the recorded use: %v", err)
	}
	authenticate()
	if got := lastUsed(); got == nil || !got.After(stale) {
		t.Errorf("last use is still %v after a request past the interval, want it refreshed", got)
	}
}

// A key's audit trail is bookkeeping, not a precondition. A request
// that has been authenticated is served even when the timestamp behind
// it cannot be written.
func TestARequestIsServedEvenIfItsKeysLastUseCannotBeRecorded(t *testing.T) {
	a := newAPI(t)
	_, key := a.newTenant(t, "acme", herald.ScopeFull)
	ctx := context.Background()

	// Nothing may update the column from here on.
	if _, err := a.pool.Exec(ctx, `
		CREATE RULE no_touch AS ON UPDATE TO api_keys DO INSTEAD NOTHING`); err != nil {
		t.Fatalf("block the audit write: %v", err)
	}

	rec := a.do(t, http.MethodPost, "/v1/applications", key, `{"uid":"billing","name":"Billing"}`)
	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d: a failed audit write refused an authenticated request (%s)",
			rec.Code, http.StatusCreated, rec.Body)
	}
}

func TestAKeyHeraldNeverIssuedIsUnauthorized(t *testing.T) {
	a := newAPI(t)
	_, valid := a.newTenant(t, "acme", herald.ScopeFull)

	reached := false
	guarded := a.server.requireKey(herald.ScopeFull,
		func(http.ResponseWriter, *http.Request, caller) error {
			reached = true
			return nil
		})

	for _, presented := range []string{
		herald.APIKeyPrefix + "not-a-key-that-was-ever-issued",
		valid + "x",
		strings.TrimSuffix(valid, valid[len(valid)-1:]),
	} {
		r := httptest.NewRequest(http.MethodPost, "/v1/anything", nil)
		r.Header.Set("Authorization", "Bearer "+presented)
		rec := httptest.NewRecorder()
		guarded.ServeHTTP(rec, r)

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("an unissued key got status %d, want %d", rec.Code, http.StatusUnauthorized)
		}
	}
	if reached {
		t.Errorf("an unissued key reached the guarded handler")
	}
}

// Scope is authority, not identity: an ingest key is a valid credential
// that may do less, so it is refused with a different answer than an
// invalid one.
func TestAnIngestKeyMayIngestAndNothingElse(t *testing.T) {
	a := newAPI(t)
	_, fullKey := a.newTenant(t, "acme", herald.ScopeFull)
	_, ingestKey := a.newTenant(t, "globex", herald.ScopeIngest)

	ok := func(http.ResponseWriter, *http.Request, caller) error { return nil }
	management := a.server.requireKey(herald.ScopeFull, ok)
	ingest := a.server.requireKey(herald.ScopeIngest, ok)

	cases := []struct {
		name    string
		route   http.Handler
		key     string
		want    int
		wantErr string
	}{
		{"full key on management", management, fullKey, http.StatusOK, ""},
		{"full key on ingest", ingest, fullKey, http.StatusOK, ""},
		{"ingest key on ingest", ingest, ingestKey, http.StatusOK, ""},
		{"ingest key on management", management, ingestKey, http.StatusForbidden, "forbidden"},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodPost, "/v1/anything", nil)
		r.Header.Set("Authorization", "Bearer "+c.key)
		rec := httptest.NewRecorder()
		c.route.ServeHTTP(rec, r)

		if rec.Code != c.want {
			t.Errorf("%s: status = %d, want %d", c.name, rec.Code, c.want)
		}
		if c.wantErr != "" {
			var envelope errorEnvelope
			decodeBody(t, rec, &envelope)
			if envelope.Error.Code != c.wantErr {
				t.Errorf("%s: error code = %q, want %q", c.name, envelope.Error.Code, c.wantErr)
			}
		}
	}
}
