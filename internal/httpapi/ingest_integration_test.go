//go:build integration

package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/augusto-dmh/herald/internal/deliver"
	"github.com/augusto-dmh/herald/internal/herald"
)

// newEndpoint registers a destination directly, which is the only way
// to start one out disabled: the API has no route for disabling yet.
func (a *api) newEndpoint(
	t *testing.T, tenantID uuid.UUID, appUID, url string, filters []string, disabled bool,
) herald.Endpoint {
	t.Helper()
	ctx := context.Background()

	application, err := a.store.ApplicationByUID(ctx, tenantID, appUID)
	if err != nil {
		t.Fatalf("read application %s: %v", appUID, err)
	}
	endpoint, err := a.store.CreateEndpoint(ctx, herald.Endpoint{
		ID: newID(t), TenantID: tenantID, ApplicationID: application.ID,
		URL: url, FilterTypes: filters, Disabled: disabled,
	})
	if err != nil {
		t.Fatalf("create endpoint %s: %v", url, err)
	}
	return endpoint
}

// queuedDeliveries returns the delivery each enqueued job was created
// for, which is what the delivery worker will be handed.
func (a *api) queuedDeliveries(t *testing.T) []uuid.UUID {
	t.Helper()
	rows, err := a.pool.Query(context.Background(), `SELECT kind, args FROM drover_jobs ORDER BY id`)
	if err != nil {
		t.Fatalf("read queued jobs: %v", err)
	}
	defer rows.Close()

	var queued []uuid.UUID
	for rows.Next() {
		var kind string
		var raw []byte
		if err := rows.Scan(&kind, &raw); err != nil {
			t.Fatalf("scan queued job: %v", err)
		}
		if want := (deliver.Args{}).Kind(); kind != want {
			t.Errorf("queued job kind = %q, want %q", kind, want)
		}
		var args deliver.Args
		if err := json.Unmarshal(raw, &args); err != nil {
			t.Fatalf("decode queued job args: %v", err)
		}
		queued = append(queued, args.DeliveryID)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read queued jobs: %v", err)
	}
	return queued
}

// assertNothingIngested checks that a refused request left no trace of
// itself anywhere ingest writes.
func (a *api) assertNothingIngested(t *testing.T) {
	t.Helper()
	for _, table := range []string{"messages", "deliveries", "drover_jobs"} {
		if got := a.countRows(t, table); got != 0 {
			t.Errorf("%s holds %d rows, want 0", table, got)
		}
	}
}

// A message reaches the endpoints that asked for its event type and no
// others: an endpoint listing other types is passed over, and so is one
// that is disabled however well it matches.
func TestAMessageIsFannedOutToEveryEndpointThatWantsItAndNoOthers(t *testing.T) {
	a := newAPI(t)
	tenant, key := a.newTenant(t, "acme", herald.ScopeFull)
	a.createApp(t, key, "billing")

	everything := a.newEndpoint(t, tenant.ID, "billing", "https://example.com/all", nil, false)
	paid := a.newEndpoint(t, tenant.ID, "billing", "https://example.com/paid",
		[]string{"invoice.paid", "invoice.voided"}, false)
	a.newEndpoint(t, tenant.ID, "billing", "https://example.com/other",
		[]string{"user.created"}, false)
	a.newEndpoint(t, tenant.ID, "billing", "https://example.com/off",
		[]string{"invoice.paid"}, true)

	rec := a.do(t, http.MethodPost, "/v1/applications/billing/messages", key,
		`{"event_type":"invoice.paid","payload":{"total":10}}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d (%s)", rec.Code, http.StatusAccepted, rec.Body)
	}

	var accepted acceptedMessageResponse
	decodeBody(t, rec, &accepted)
	if accepted.ID == uuid.Nil {
		t.Fatalf("an accepted message came back without an id")
	}

	deliveries, err := a.store.DeliveriesByMessage(context.Background(), tenant.ID, accepted.ID)
	if err != nil {
		t.Fatalf("read deliveries: %v", err)
	}
	wanted := map[uuid.UUID]string{everything.ID: everything.URL, paid.ID: paid.URL}
	if len(deliveries) != len(wanted) {
		t.Fatalf("deliveries = %d, want one per interested endpoint (%d)", len(deliveries), len(wanted))
	}
	for _, d := range deliveries {
		if _, ok := wanted[d.EndpointID]; !ok {
			t.Errorf("a delivery was created for endpoint %s, which did not want this event", d.EndpointID)
		}
		if d.Status != herald.DeliveryPending {
			t.Errorf("a new delivery is %q, want pending", d.Status)
		}
	}

	// Every delivery has exactly one job to carry it out, and every job
	// names a delivery that exists.
	queued := a.queuedDeliveries(t)
	if len(queued) != len(deliveries) {
		t.Fatalf("queued jobs = %d, want one per delivery (%d)", len(queued), len(deliveries))
	}
	created := make(map[uuid.UUID]bool, len(deliveries))
	for _, d := range deliveries {
		created[d.ID] = true
	}
	for _, deliveryID := range queued {
		if !created[deliveryID] {
			t.Errorf("a job was queued for delivery %s, which was never created", deliveryID)
		}
	}

	// The accepted message is readable back through the API.
	rec = a.do(t, http.MethodGet, "/v1/applications/billing/messages/"+accepted.ID.String(), key, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("reading the accepted message: status %d (%s)", rec.Code, rec.Body)
	}
	var reported messageResponse
	decodeBody(t, rec, &reported)
	if len(reported.Deliveries) != len(wanted) {
		t.Errorf("the message reports %d deliveries, want %d", len(reported.Deliveries), len(wanted))
	}
}

func TestAMessageNoEndpointWantsIsStillAccepted(t *testing.T) {
	a := newAPI(t)
	tenant, key := a.newTenant(t, "acme", herald.ScopeFull)
	a.createApp(t, key, "billing")
	a.newEndpoint(t, tenant.ID, "billing", "https://example.com/other", []string{"user.created"}, false)

	rec := a.do(t, http.MethodPost, "/v1/applications/billing/messages", key,
		`{"event_type":"invoice.paid","payload":{"total":10}}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d (%s)", rec.Code, http.StatusAccepted, rec.Body)
	}

	var accepted acceptedMessageResponse
	decodeBody(t, rec, &accepted)
	stored, err := a.store.MessageByID(context.Background(), tenant.ID,
		a.applicationID(t, tenant.ID, "billing"), accepted.ID)
	if err != nil {
		t.Fatalf("the accepted message was not stored: %v", err)
	}
	if stored.EventType != "invoice.paid" {
		t.Errorf("stored event type = %q, want invoice.paid", stored.EventType)
	}
	if got := a.countRows(t, "deliveries"); got != 0 {
		t.Errorf("deliveries = %d, want 0", got)
	}
	if got := a.countRows(t, "drover_jobs"); got != 0 {
		t.Errorf("queued jobs = %d, want 0", got)
	}
}

func (a *api) applicationID(t *testing.T, tenantID uuid.UUID, uid string) uuid.UUID {
	t.Helper()
	application, err := a.store.ApplicationByUID(context.Background(), tenantID, uid)
	if err != nil {
		t.Fatalf("read application %s: %v", uid, err)
	}
	return application.ID
}

// An ingest that fails after some of its rows are already written
// leaves none of them: the message, its deliveries and the jobs behind
// them are one unit of work, so a caller who was not told the message
// was accepted can be sure nothing of it survived.
func TestAnIngestThatFailsPartWayThroughLeavesNothingBehind(t *testing.T) {
	a := newAPI(t)
	tenant, key := a.newTenant(t, "acme", herald.ScopeFull)
	a.createApp(t, key, "billing")
	a.newEndpoint(t, tenant.ID, "billing", "https://example.com/first", nil, false)
	a.newEndpoint(t, tenant.ID, "billing", "https://example.com/second", nil, false)

	// The failure is injected in the database so that it strikes after
	// the message, the first delivery and the first job are written.
	if _, err := a.pool.Exec(context.Background(), `
		CREATE FUNCTION fail_once_a_delivery_exists() RETURNS trigger
		LANGUAGE plpgsql AS $$
		BEGIN
			IF (SELECT count(*) FROM deliveries) > 0 THEN
				RAISE EXCEPTION 'injected failure part-way through ingest';
			END IF;
			RETURN NEW;
		END $$;

		CREATE TRIGGER fail_once_a_delivery_exists
			BEFORE INSERT ON deliveries FOR EACH ROW
			EXECUTE FUNCTION fail_once_a_delivery_exists();`); err != nil {
		t.Fatalf("install the injected failure: %v", err)
	}

	rec := a.do(t, http.MethodPost, "/v1/applications/billing/messages", key,
		`{"event_type":"invoice.paid","payload":{"total":10}}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d (%s)", rec.Code, http.StatusInternalServerError, rec.Body)
	}
	var envelope errorEnvelope
	decodeBody(t, rec, &envelope)
	if envelope.Error.Code != "internal_error" {
		t.Errorf("error code = %q, want internal_error", envelope.Error.Code)
	}

	a.assertNothingIngested(t)
}

func TestABodyOverTheCapIsRefusedAndNothingIsIngested(t *testing.T) {
	a := newAPI(t)
	tenant, key := a.newTenant(t, "acme", herald.ScopeFull)
	a.createApp(t, key, "billing")
	a.newEndpoint(t, tenant.ID, "billing", "https://example.com/all", nil, false)

	oversized := `{"event_type":"invoice.paid","payload":{"note":"` +
		strings.Repeat("x", maxBodyBytes) + `"}}`
	rec := a.do(t, http.MethodPost, "/v1/applications/billing/messages", key, oversized)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want %d (%s)", rec.Code, http.StatusRequestEntityTooLarge, rec.Body)
	}
	var envelope errorEnvelope
	decodeBody(t, rec, &envelope)
	if envelope.Error.Code != "payload_too_large" {
		t.Errorf("error code = %q, want payload_too_large", envelope.Error.Code)
	}
	a.assertNothingIngested(t)
}

func TestAnUnusableMessageIsRefusedAndNothingIsIngested(t *testing.T) {
	a := newAPI(t)
	tenant, key := a.newTenant(t, "acme", herald.ScopeFull)
	a.createApp(t, key, "billing")
	a.newEndpoint(t, tenant.ID, "billing", "https://example.com/all", nil, false)

	cases := []struct {
		name      string
		body      string
		wantField string
	}{
		{"a body that is not JSON", `{"event_type":"invoice.paid","payload":`, ""},
		{"a body that is not an object", `"invoice.paid"`, ""},
		{"no event type", `{"payload":{"total":10}}`, "event_type"},
		{"a padded event type", `{"event_type":" invoice.paid ","payload":{}}`, "event_type"},
		{"no payload", `{"event_type":"invoice.paid"}`, "payload"},
	}
	for _, c := range cases {
		rec := a.do(t, http.MethodPost, "/v1/applications/billing/messages", key, c.body)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d, want %d (%s)", c.name, rec.Code,
				http.StatusUnprocessableEntity, rec.Body)
		}
		var envelope errorEnvelope
		decodeBody(t, rec, &envelope)
		if c.wantField != "" && envelope.Error.Field != c.wantField {
			t.Errorf("%s: named field %q, want %q", c.name, envelope.Error.Field, c.wantField)
		}
	}
	a.assertNothingIngested(t)
}

func TestIngestingIntoAnotherTenantsApplicationIsNotFound(t *testing.T) {
	a := newAPI(t)
	acme, acmeKey := a.newTenant(t, "acme", herald.ScopeFull)
	_, globexKey := a.newTenant(t, "globex", herald.ScopeFull)
	a.createApp(t, acmeKey, "billing")
	a.newEndpoint(t, acme.ID, "billing", "https://acme.example.com/hooks", nil, false)

	rec := a.do(t, http.MethodPost, "/v1/applications/billing/messages", globexKey,
		`{"event_type":"invoice.paid","payload":{"total":10}}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d (%s)", rec.Code, http.StatusNotFound, rec.Body)
	}
	rec = a.do(t, http.MethodPost, "/v1/applications/no-such-app/messages", acmeKey,
		`{"event_type":"invoice.paid","payload":{"total":10}}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("an application that exists nowhere: status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	a.assertNothingIngested(t)
}

// Ingest is the one route an ingest-scoped key is for.
func TestAKeyThatMayOnlyIngestMayIngest(t *testing.T) {
	a := newAPI(t)
	tenant, fullKey := a.newTenant(t, "acme", herald.ScopeFull)
	a.createApp(t, fullKey, "billing")
	a.newEndpoint(t, tenant.ID, "billing", "https://example.com/all", nil, false)

	generated, err := herald.NewAPIKey(tenant.ID, herald.ScopeIngest)
	if err != nil {
		t.Fatalf("mint an ingest key: %v", err)
	}
	if _, err := a.store.CreateAPIKey(context.Background(), generated.Key); err != nil {
		t.Fatalf("store the ingest key: %v", err)
	}

	rec := a.do(t, http.MethodPost, "/v1/applications/billing/messages", generated.Plaintext,
		`{"event_type":"invoice.paid","payload":{"total":10}}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d (%s)", rec.Code, http.StatusAccepted, rec.Body)
	}
	if got := a.countRows(t, "drover_jobs"); got != 1 {
		t.Errorf("queued jobs = %d, want 1", got)
	}

	// Reading the message back is management, which this key may not do.
	var accepted acceptedMessageResponse
	decodeBody(t, rec, &accepted)
	rec = a.do(t, http.MethodGet, "/v1/applications/billing/messages/"+accepted.ID.String(),
		generated.Plaintext, "")
	if rec.Code != http.StatusForbidden {
		t.Errorf("reading a message with an ingest key: status = %d, want %d",
			rec.Code, http.StatusForbidden)
	}
}
