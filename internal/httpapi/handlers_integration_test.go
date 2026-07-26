//go:build integration

package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/augusto-dmh/herald/internal/herald"
)

// createApp registers an application through the API and returns its
// uid.
func (a *api) createApp(t *testing.T, key, uid string) string {
	t.Helper()
	rec := a.do(t, http.MethodPost, "/v1/applications", key,
		`{"uid":"`+uid+`","name":"`+uid+` app"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create application %s: status %d (%s)", uid, rec.Code, rec.Body)
	}
	return uid
}

// seedMessage writes a message with one delivery and one attempt
// directly, for the tests that report on deliveries rather than create
// them.
func (a *api) seedMessage(
	t *testing.T, tenantID uuid.UUID, appUID string,
) (herald.Message, herald.Delivery, herald.DeliveryAttempt) {
	t.Helper()
	ctx := context.Background()

	application, err := a.store.ApplicationByUID(ctx, tenantID, appUID)
	if err != nil {
		t.Fatalf("read application %s: %v", appUID, err)
	}
	endpoint, err := a.store.CreateEndpoint(ctx, herald.Endpoint{
		ID: newID(t), TenantID: tenantID, ApplicationID: application.ID,
		URL: "https://example.com/hooks",
	})
	if err != nil {
		t.Fatalf("create endpoint: %v", err)
	}

	tx, err := a.store.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	message, err := a.store.CreateMessageTx(ctx, tx, herald.Message{
		ID: newID(t), TenantID: tenantID, ApplicationID: application.ID,
		EventType: "invoice.paid", Payload: json.RawMessage(`{"total":10}`),
	})
	if err != nil {
		t.Fatalf("create message: %v", err)
	}
	delivery, err := a.store.CreateDeliveryTx(ctx, tx, herald.Delivery{
		ID: newID(t), TenantID: tenantID, MessageID: message.ID, EndpointID: endpoint.ID,
	})
	if err != nil {
		t.Fatalf("create delivery: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	statusCode := 502
	attempt, err := a.store.CreateAttempt(ctx, herald.DeliveryAttempt{
		ID: newID(t), TenantID: tenantID, DeliveryID: delivery.ID, AttemptNumber: 1,
		StatusCode: &statusCode, Success: false, Error: "", ResponseSnippet: "bad gateway",
		Duration: 250 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("record attempt: %v", err)
	}
	if err := a.store.UpdateDeliveryStatus(ctx, delivery.ID, herald.DeliveryFailed); err != nil {
		t.Fatalf("mark delivery failed: %v", err)
	}
	return message, delivery, attempt
}

func (a *api) countRows(t *testing.T, table string) int {
	t.Helper()
	var count int
	if err := a.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM `+table).Scan(&count); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return count
}

// The key handed back when a tenant is bootstrapped is the only copy
// there will ever be: it works, and what the database kept in its place
// cannot be turned back into it.
func TestBootstrappingATenantReturnsTheOnlyCopyOfItsKey(t *testing.T) {
	a := newAPI(t)

	rec := a.do(t, http.MethodPost, "/v1/tenants", bootstrapForTests, `{"name":"Acme"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d (%s)", rec.Code, http.StatusCreated, rec.Body)
	}

	var created createTenantResponse
	decodeBody(t, rec, &created)
	if created.Tenant.Name != "Acme" {
		t.Errorf("tenant name = %q, want Acme", created.Tenant.Name)
	}
	if !strings.HasPrefix(created.APIKey.Key, herald.APIKeyPrefix) {
		t.Errorf("issued key %q does not carry herald's key prefix", created.APIKey.Key)
	}
	if created.APIKey.Scope != herald.ScopeFull {
		t.Errorf("issued key scope = %q, want full", created.APIKey.Scope)
	}

	// The key is real: it authenticates the tenant it was issued for.
	rec = a.do(t, http.MethodPost, "/v1/applications", created.APIKey.Key,
		`{"uid":"billing","name":"Billing"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("the issued key was not accepted: status %d (%s)", rec.Code, rec.Body)
	}

	// What was stored is the hash and a display fragment, and neither is
	// the key.
	stored, err := a.store.APIKeyByHash(context.Background(), herald.HashAPIKey(created.APIKey.Key))
	if err != nil {
		t.Fatalf("the issued key does not resolve to its stored record: %v", err)
	}
	if stored.TenantID != created.Tenant.ID {
		t.Errorf("the key resolves to tenant %s, want %s", stored.TenantID, created.Tenant.ID)
	}
	secret := strings.TrimPrefix(created.APIKey.Key, herald.APIKeyPrefix)
	if strings.Contains(stored.Prefix, secret) || stored.Prefix != created.APIKey.Prefix {
		t.Errorf("the stored display prefix %q is not a fragment of the key", stored.Prefix)
	}
	if strings.Contains(string(stored.Hash), secret) {
		t.Errorf("the stored hash holds the key in the clear")
	}

	var withPlaintext int
	if err := a.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM api_keys WHERE prefix = $1 OR encode(key_hash, 'escape') = $1`,
		created.APIKey.Key).Scan(&withPlaintext); err != nil {
		t.Fatalf("search for the plaintext key: %v", err)
	}
	if withPlaintext != 0 {
		t.Errorf("%d stored key rows hold the plaintext", withPlaintext)
	}
}

func TestTenantCreationWithoutTheOperatorTokenCreatesNothing(t *testing.T) {
	a := newAPI(t)

	for _, credential := range []string{"", "not-the-operator-token", bootstrapForTests + "x"} {
		rec := a.do(t, http.MethodPost, "/v1/tenants", credential, `{"name":"Acme"}`)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("credential %q got status %d, want %d", credential, rec.Code, http.StatusUnauthorized)
		}
	}
	if got := a.countRows(t, "tenants"); got != 0 {
		t.Errorf("tenants = %d after unauthorized creation attempts, want 0", got)
	}
	if got := a.countRows(t, "api_keys"); got != 0 {
		t.Errorf("api_keys = %d after unauthorized creation attempts, want 0", got)
	}
}

func TestAnApplicationAndItsEndpointsBelongToTheKeysTenant(t *testing.T) {
	a := newAPI(t)
	acme, acmeKey := a.newTenant(t, "acme", herald.ScopeFull)
	globex, globexKey := a.newTenant(t, "globex", herald.ScopeFull)
	ctx := context.Background()

	a.createApp(t, acmeKey, "billing")

	application, err := a.store.ApplicationByUID(ctx, acme.ID, "billing")
	if err != nil {
		t.Fatalf("the created application is not the caller's: %v", err)
	}

	rec := a.do(t, http.MethodPost, "/v1/applications/billing/endpoints", acmeKey,
		`{"url":"https://acme.example.com/hooks","description":"main","filter_types":["invoice.paid"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create endpoint: status %d (%s)", rec.Code, rec.Body)
	}
	var endpoint endpointResponse
	decodeBody(t, rec, &endpoint)
	if endpoint.Disabled {
		t.Errorf("a new endpoint is disabled")
	}

	stored, err := a.store.EndpointByID(ctx, acme.ID, endpoint.ID)
	if err != nil {
		t.Fatalf("the created endpoint is not the caller's: %v", err)
	}
	if stored.ApplicationID != application.ID {
		t.Errorf("the endpoint hangs off application %s, want %s", stored.ApplicationID, application.ID)
	}

	// A uid is the caller's own name for its application: taken once
	// within a tenant, free to repeat across tenants.
	rec = a.do(t, http.MethodPost, "/v1/applications", acmeKey, `{"uid":"billing","name":"again"}`)
	if rec.Code != http.StatusConflict {
		t.Errorf("reusing a uid in the same tenant: status %d, want %d", rec.Code, http.StatusConflict)
	}
	a.createApp(t, globexKey, "billing")
	theirs, err := a.store.ApplicationByUID(ctx, globex.ID, "billing")
	if err != nil {
		t.Fatalf("read the second tenant's application: %v", err)
	}
	if theirs.ID == application.ID {
		t.Errorf("both tenants' billing applications are the same row")
	}
}

// A subscription to nothing is never what a caller meant, so an
// endpoint that lists no event types is one that receives them all.
func TestAnEndpointListingNoEventTypesReceivesEveryType(t *testing.T) {
	a := newAPI(t)
	tenant, key := a.newTenant(t, "acme", herald.ScopeFull)
	a.createApp(t, key, "billing")
	ctx := context.Background()

	bodies := map[string]string{
		"an empty list": `{"url":"https://example.com/empty","filter_types":[]}`,
		"no list":       `{"url":"https://example.com/absent"}`,
		"a null list":   `{"url":"https://example.com/null","filter_types":null}`,
	}
	for name, body := range bodies {
		rec := a.do(t, http.MethodPost, "/v1/applications/billing/endpoints", key, body)
		if rec.Code != http.StatusCreated {
			t.Fatalf("%s: status %d (%s)", name, rec.Code, rec.Body)
		}
		var endpoint endpointResponse
		decodeBody(t, rec, &endpoint)
		if len(endpoint.FilterTypes) != 0 {
			t.Errorf("%s: endpoint came back filtering on %v", name, endpoint.FilterTypes)
		}
	}

	application, err := a.store.ApplicationByUID(ctx, tenant.ID, "billing")
	if err != nil {
		t.Fatalf("read application: %v", err)
	}
	matched, err := a.store.EndpointsForEvent(ctx, tenant.ID, application.ID, "some.event.nobody.named")
	if err != nil {
		t.Fatalf("select endpoints: %v", err)
	}
	if len(matched) != len(bodies) {
		t.Errorf("%d of %d unfiltered endpoints receive an arbitrary event type",
			len(matched), len(bodies))
	}
}

// Naming another tenant's application is answered exactly as naming one
// that does not exist, so the API cannot be used to discover what other
// tenants own.
func TestOneTenantCannotAddressAnothersApplicationOrMessages(t *testing.T) {
	a := newAPI(t)
	acme, acmeKey := a.newTenant(t, "acme", herald.ScopeFull)
	_, globexKey := a.newTenant(t, "globex", herald.ScopeFull)
	a.createApp(t, acmeKey, "billing")
	message, _, _ := a.seedMessage(t, acme.ID, "billing")

	intruding := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{
			"registering an endpoint", http.MethodPost,
			"/v1/applications/billing/endpoints", `{"url":"https://evil.example.com/hooks"}`,
		},
		{
			"reading a message", http.MethodGet,
			"/v1/applications/billing/messages/" + message.ID.String(), "",
		},
	}
	for _, c := range intruding {
		rec := a.do(t, c.method, c.path, globexKey, c.body)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s in another tenant: status %d, want %d", c.name, rec.Code, http.StatusNotFound)
		}
		var envelope errorEnvelope
		decodeBody(t, rec, &envelope)
		if envelope.Error.Code != "not_found" {
			t.Errorf("%s in another tenant: error code %q, want not_found", c.name, envelope.Error.Code)
		}
	}

	// Nothing was written under the addressed application either.
	if got := a.countRows(t, "endpoints"); got != 1 {
		t.Errorf("endpoints = %d, want only the one the owner seeded", got)
	}

	// The owner still gets its own message, so the answers above are not
	// merely a route that never works.
	rec := a.do(t, http.MethodGet, "/v1/applications/billing/messages/"+message.ID.String(), acmeKey, "")
	if rec.Code != http.StatusOK {
		t.Errorf("the owning tenant could not read its own message: status %d (%s)", rec.Code, rec.Body)
	}
}

func TestAMessageReportsEveryDeliveryAndWhatEachAttemptDid(t *testing.T) {
	a := newAPI(t)
	tenant, key := a.newTenant(t, "acme", herald.ScopeFull)
	a.createApp(t, key, "billing")
	message, delivery, attempt := a.seedMessage(t, tenant.ID, "billing")

	rec := a.do(t, http.MethodGet, "/v1/applications/billing/messages/"+message.ID.String(), key, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
	}

	var got messageResponse
	decodeBody(t, rec, &got)
	if got.ID != message.ID || got.EventType != "invoice.paid" {
		t.Errorf("read back message %s (%s), want %s (invoice.paid)", got.ID, got.EventType, message.ID)
	}
	var payload map[string]int
	if err := json.Unmarshal(got.Payload, &payload); err != nil {
		t.Fatalf("the payload did not come back as JSON: %v", err)
	}
	if payload["total"] != 10 {
		t.Errorf("payload = %v, want the submitted document", payload)
	}
	if len(got.Deliveries) != 1 {
		t.Fatalf("deliveries = %d, want 1", len(got.Deliveries))
	}

	d := got.Deliveries[0]
	if d.ID != delivery.ID || d.EndpointID != delivery.EndpointID {
		t.Errorf("delivery %s to endpoint %s, want %s to %s",
			d.ID, d.EndpointID, delivery.ID, delivery.EndpointID)
	}
	if d.Status != herald.DeliveryFailed {
		t.Errorf("delivery status = %q, want failed", d.Status)
	}
	if len(d.Attempts) != 1 {
		t.Fatalf("attempts = %d, want 1", len(d.Attempts))
	}

	made := d.Attempts[0]
	if made.ID != attempt.ID || made.AttemptNumber != 1 {
		t.Errorf("attempt %s number %d, want %s number 1", made.ID, made.AttemptNumber, attempt.ID)
	}
	if made.StatusCode == nil || *made.StatusCode != 502 {
		t.Errorf("attempt status code = %v, want 502", made.StatusCode)
	}
	if made.Success {
		t.Errorf("an attempt that got a 502 is reported as successful")
	}
	if made.ResponseSnippet != "bad gateway" {
		t.Errorf("attempt snippet = %q, want the captured response", made.ResponseSnippet)
	}
	if made.DurationMS != 250 {
		t.Errorf("attempt duration = %dms, want 250ms", made.DurationMS)
	}
}

// An id that is not an identifier names nothing, and is answered the
// same way as one naming another tenant's message: a caller learns
// nothing from the difference between a malformed id and a real one it
// may not have.
func TestAMessageIDThatIsNotAnIdentifierIsAnsweredAsMissing(t *testing.T) {
	a := newAPI(t)
	tenant, key := a.newTenant(t, "acme", herald.ScopeFull)
	a.createApp(t, key, "billing")
	message, _, _ := a.seedMessage(t, tenant.ID, "billing")

	for _, id := range []string{"not-an-id", "42", "0", message.ID.String() + "x"} {
		rec := a.do(t, http.MethodGet, "/v1/applications/billing/messages/"+id, key, "")
		if rec.Code != http.StatusNotFound {
			t.Errorf("message id %q: status = %d, want %d (%s)",
				id, rec.Code, http.StatusNotFound, rec.Body)
		}
		var envelope errorEnvelope
		decodeBody(t, rec, &envelope)
		if envelope.Error.Code != "not_found" {
			t.Errorf("message id %q: error code = %q, want not_found", id, envelope.Error.Code)
		}
	}

	// The route works for a well-formed id the caller does own, so the
	// answers above are refusals rather than a route that never serves.
	rec := a.do(t, http.MethodGet, "/v1/applications/billing/messages/"+message.ID.String(), key, "")
	if rec.Code != http.StatusOK {
		t.Errorf("the owner could not read its own message: status %d (%s)", rec.Code, rec.Body)
	}
}

// Registering an endpoint decides where a tenant's messages are sent,
// which is management authority. A key that may only submit messages
// is a valid credential that may not do it — a different answer from
// one herald does not recognize at all.
func TestRegisteringAnEndpointRefusesAKeyThatMayOnlyIngest(t *testing.T) {
	a := newAPI(t)
	tenant, ingestKey := a.newTenant(t, "acme", herald.ScopeIngest)

	// The application exists, so a refusal can only be about the scope.
	if _, err := a.store.CreateApplication(context.Background(), herald.Application{
		ID: newID(t), TenantID: tenant.ID, UID: "billing", Name: "Billing",
	}); err != nil {
		t.Fatalf("create application: %v", err)
	}

	cases := []struct {
		name       string
		credential string
		want       int
		wantCode   string
	}{
		{"an ingest key", ingestKey, http.StatusForbidden, "forbidden"},
		{"no key", "", http.StatusUnauthorized, "unauthorized"},
		{"an unissued key", herald.APIKeyPrefix + "nothing", http.StatusUnauthorized, "unauthorized"},
	}
	for _, c := range cases {
		rec := a.do(t, http.MethodPost, "/v1/applications/billing/endpoints", c.credential,
			`{"url":"https://acme.example.com/hooks"}`)
		if rec.Code != c.want {
			t.Errorf("%s: status = %d, want %d (%s)", c.name, rec.Code, c.want, rec.Body)
		}
		var envelope errorEnvelope
		decodeBody(t, rec, &envelope)
		if envelope.Error.Code != c.wantCode {
			t.Errorf("%s: error code = %q, want %q", c.name, envelope.Error.Code, c.wantCode)
		}
	}
	if got := a.countRows(t, "endpoints"); got != 0 {
		t.Errorf("endpoints = %d after refused requests, want 0", got)
	}
}

func TestManagementRefusesAKeyThatMayOnlyIngest(t *testing.T) {
	a := newAPI(t)
	_, ingestKey := a.newTenant(t, "acme", herald.ScopeIngest)

	cases := []struct {
		name       string
		credential string
		want       int
		wantCode   string
	}{
		{"an ingest key", ingestKey, http.StatusForbidden, "forbidden"},
		{"no key", "", http.StatusUnauthorized, "unauthorized"},
		{"an unissued key", herald.APIKeyPrefix + "nothing", http.StatusUnauthorized, "unauthorized"},
	}
	for _, c := range cases {
		rec := a.do(t, http.MethodPost, "/v1/applications", c.credential,
			`{"uid":"billing","name":"Billing"}`)
		if rec.Code != c.want {
			t.Errorf("%s: status = %d, want %d", c.name, rec.Code, c.want)
		}
		var envelope errorEnvelope
		decodeBody(t, rec, &envelope)
		if envelope.Error.Code != c.wantCode {
			t.Errorf("%s: error code = %q, want %q", c.name, envelope.Error.Code, c.wantCode)
		}
	}
	if got := a.countRows(t, "applications"); got != 0 {
		t.Errorf("applications = %d after refused requests, want 0", got)
	}
}

func TestAnUnusableRequestNamesWhatToFix(t *testing.T) {
	a := newAPI(t)
	_, key := a.newTenant(t, "acme", herald.ScopeFull)
	a.createApp(t, key, "billing")

	cases := []struct {
		name      string
		path      string
		body      string
		wantField string
	}{
		{"an application with no uid", "/v1/applications", `{"name":"Billing"}`, "uid"},
		{"an application with no name", "/v1/applications", `{"uid":"shipping"}`, "name"},
		{
			"an endpoint with an unusable url", "/v1/applications/billing/endpoints",
			`{"url":"ftp://example.com/hooks"}`, "url",
		},
		{
			"an endpoint filtering on an empty event type",
			"/v1/applications/billing/endpoints",
			`{"url":"https://example.com/hooks","filter_types":[""]}`, "filter_types",
		},
	}
	for _, c := range cases {
		rec := a.do(t, http.MethodPost, c.path, key, c.body)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d, want %d (%s)", c.name, rec.Code,
				http.StatusUnprocessableEntity, rec.Body)
		}
		var envelope errorEnvelope
		decodeBody(t, rec, &envelope)
		if envelope.Error.Field != c.wantField {
			t.Errorf("%s: named field %q, want %q", c.name, envelope.Error.Field, c.wantField)
		}
	}
	if got := a.countRows(t, "endpoints"); got != 0 {
		t.Errorf("endpoints = %d after rejected requests, want 0", got)
	}
}
