//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/augusto-dmh/drover"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/augusto-dmh/herald/internal/herald"
	"github.com/augusto-dmh/herald/internal/store"
)

// deliveryJob stands in for the job the delivery worker will consume.
// All ingest needs from it here is that it is a real queue job written
// through the same transaction as the rows it refers to.
type deliveryJob struct {
	DeliveryID uuid.UUID `json:"delivery_id"`
}

func (deliveryJob) Kind() string { return "webhook_delivery" }

func newQueue(t *testing.T, pool *pgxpool.Pool) *drover.Client {
	t.Helper()
	client, err := drover.NewClient(pool, drover.Config{})
	if err != nil {
		t.Fatalf("new queue client: %v", err)
	}
	return client
}

func countRows(t *testing.T, pool *pgxpool.Pool, table string) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM `+table).Scan(&count); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return count
}

// ingest is the composition the API handler will perform: one
// transaction carrying the message, a delivery per matching endpoint,
// and a queue job per delivery.
func ingest(
	t *testing.T, s *store.Store, pool *pgxpool.Pool,
	f tenantFixture, eventType, payload string,
) (herald.Message, []herald.Delivery) {
	t.Helper()
	ctx := context.Background()
	queue := newQueue(t, pool)

	tx, err := s.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // a rollback after commit is a no-op

	message, err := s.CreateMessageTx(ctx, tx, herald.Message{
		ID: newID(t), TenantID: f.tenant.ID, ApplicationID: f.application.ID,
		EventType: eventType, Payload: json.RawMessage(payload),
	})
	if err != nil {
		t.Fatalf("create message: %v", err)
	}

	endpoints, err := s.EndpointsForEventTx(ctx, tx, f.tenant.ID, f.application.ID, eventType)
	if err != nil {
		t.Fatalf("select endpoints: %v", err)
	}

	deliveries := make([]herald.Delivery, 0, len(endpoints))
	for _, endpoint := range endpoints {
		delivery, err := s.CreateDeliveryTx(ctx, tx, herald.Delivery{
			ID: newID(t), TenantID: f.tenant.ID, MessageID: message.ID, EndpointID: endpoint.ID,
		})
		if err != nil {
			t.Fatalf("create delivery: %v", err)
		}
		if _, err := queue.InsertTx(ctx, tx, deliveryJob{DeliveryID: delivery.ID}); err != nil {
			t.Fatalf("enqueue delivery job: %v", err)
		}
		deliveries = append(deliveries, delivery)
	}

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return message, deliveries
}

// An accepted message and the jobs that will carry it out become
// visible at the same instant, so there is no window in which a message
// exists with nothing scheduled to deliver it.
func TestAMessageItsDeliveriesAndItsJobsBecomeVisibleTogether(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	f := newTenant(t, s, "acme", "billing")
	newEndpoint(t, s, f, "https://example.com/one", nil, false)
	newEndpoint(t, s, f, "https://example.com/two", []string{"invoice.paid"}, false)
	queue := newQueue(t, pool)

	tx, err := s.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // a rollback after commit is a no-op

	message, err := s.CreateMessageTx(ctx, tx, herald.Message{
		ID: newID(t), TenantID: f.tenant.ID, ApplicationID: f.application.ID,
		EventType: "invoice.paid", Payload: json.RawMessage(`{"total":10}`),
	})
	if err != nil {
		t.Fatalf("create message: %v", err)
	}
	endpoints, err := s.EndpointsForEventTx(ctx, tx, f.tenant.ID, f.application.ID, "invoice.paid")
	if err != nil {
		t.Fatalf("select endpoints: %v", err)
	}
	if len(endpoints) != 2 {
		t.Fatalf("fan-out selected %d endpoints, want 2", len(endpoints))
	}
	for _, endpoint := range endpoints {
		delivery, err := s.CreateDeliveryTx(ctx, tx, herald.Delivery{
			ID: newID(t), TenantID: f.tenant.ID, MessageID: message.ID, EndpointID: endpoint.ID,
		})
		if err != nil {
			t.Fatalf("create delivery: %v", err)
		}
		if _, err := queue.InsertTx(ctx, tx, deliveryJob{DeliveryID: delivery.ID}); err != nil {
			t.Fatalf("enqueue delivery job: %v", err)
		}
	}

	// Nothing is observable outside the transaction until it commits.
	for _, table := range []string{"messages", "deliveries", "drover_jobs"} {
		if got := countRows(t, pool, table); got != 0 {
			t.Errorf("%s holds %d rows before the ingest commits, want 0", table, got)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if got := countRows(t, pool, "messages"); got != 1 {
		t.Errorf("messages = %d, want 1", got)
	}
	deliveries, err := s.DeliveriesByMessage(ctx, f.tenant.ID, message.ID)
	if err != nil {
		t.Fatalf("read deliveries: %v", err)
	}
	if len(deliveries) != 2 {
		t.Errorf("deliveries = %d, want one per matching endpoint (2)", len(deliveries))
	}
	if got := countRows(t, pool, "drover_jobs"); got != len(deliveries) {
		t.Errorf("queued jobs = %d, want one per delivery (%d)", got, len(deliveries))
	}

	// Each job names a delivery that exists.
	rows, err := pool.Query(ctx, `SELECT args FROM drover_jobs`)
	if err != nil {
		t.Fatalf("read jobs: %v", err)
	}
	defer rows.Close()
	queued := make(map[uuid.UUID]bool)
	for rows.Next() {
		var args []byte
		if err := rows.Scan(&args); err != nil {
			t.Fatalf("scan job args: %v", err)
		}
		var job deliveryJob
		if err := json.Unmarshal(args, &job); err != nil {
			t.Fatalf("decode job args: %v", err)
		}
		queued[job.DeliveryID] = true
	}
	for _, delivery := range deliveries {
		if !queued[delivery.ID] {
			t.Errorf("delivery %s committed without a job to carry it out", delivery.ID)
		}
	}
}

// The other half of the same guarantee: a failure part-way through
// ingest leaves nothing at all, not a message without its jobs.
func TestAFailedIngestLeavesNoMessageDeliveryOrJobBehind(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	f := newTenant(t, s, "acme", "billing")
	endpoint := newEndpoint(t, s, f, "https://example.com/one", nil, false)
	queue := newQueue(t, pool)

	tx, err := s.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}

	message, err := s.CreateMessageTx(ctx, tx, herald.Message{
		ID: newID(t), TenantID: f.tenant.ID, ApplicationID: f.application.ID,
		EventType: "invoice.paid", Payload: json.RawMessage(`{"total":10}`),
	})
	if err != nil {
		t.Fatalf("create message: %v", err)
	}
	delivery, err := s.CreateDeliveryTx(ctx, tx, herald.Delivery{
		ID: newID(t), TenantID: f.tenant.ID, MessageID: message.ID, EndpointID: endpoint.ID,
	})
	if err != nil {
		t.Fatalf("create delivery: %v", err)
	}
	if _, err := queue.InsertTx(ctx, tx, deliveryJob{DeliveryID: delivery.ID}); err != nil {
		t.Fatalf("enqueue delivery job: %v", err)
	}

	// A later delivery fails: this one names an endpoint that does not
	// exist, which is what a mid-ingest error looks like.
	_, err = s.CreateDeliveryTx(ctx, tx, herald.Delivery{
		ID: newID(t), TenantID: f.tenant.ID, MessageID: message.ID, EndpointID: newID(t),
	})
	if err == nil {
		t.Fatalf("a delivery to an unknown endpoint was accepted")
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	for _, table := range []string{"messages", "deliveries", "drover_jobs"} {
		if got := countRows(t, pool, table); got != 0 {
			t.Errorf("%s holds %d rows after a failed ingest, want 0", table, got)
		}
	}
}

func TestAMessageWithNoMatchingEndpointIsStoredWithNoDeliveries(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	f := newTenant(t, s, "acme", "billing")
	newEndpoint(t, s, f, "https://example.com/other", []string{"user.created"}, false)

	// A document that would not survive being parsed and re-serialized:
	// the keys are out of order, one is repeated, and the whitespace is
	// the submitter's own.
	submitted := `{"zebra":1,  "alpha":2,"zebra":3}`
	message, deliveries := ingest(t, s, pool, f, "invoice.paid", submitted)
	if len(deliveries) != 0 {
		t.Errorf("ingest created %d deliveries, want 0", len(deliveries))
	}

	stored, err := s.MessageByID(ctx, f.tenant.ID, f.application.ID, message.ID)
	if err != nil {
		t.Fatalf("read the stored message: %v", err)
	}
	if string(stored.Payload) != submitted {
		t.Errorf("payload came back as %s, want the submitted bytes %s", stored.Payload, submitted)
	}
	if stored.EventType != "invoice.paid" {
		t.Errorf("event type came back as %q, want invoice.paid", stored.EventType)
	}
	if got := countRows(t, pool, "drover_jobs"); got != 0 {
		t.Errorf("queued jobs = %d, want 0", got)
	}
}

// What a delivery attempt did is recorded as it happened: the status
// code when a response arrived, an error and no status code when none
// did, and one row per execution either way.
func TestEachDeliveryExecutionIsRecordedAsItsOwnAttempt(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	f := newTenant(t, s, "acme", "billing")
	newEndpoint(t, s, f, "https://example.com/hooks", nil, false)

	message, deliveries := ingest(t, s, pool, f, "invoice.paid", `{"total":10}`)
	if len(deliveries) != 1 {
		t.Fatalf("ingest produced %d deliveries, want 1", len(deliveries))
	}
	delivery := deliveries[0]
	if delivery.Status != herald.DeliveryPending {
		t.Errorf("a new delivery is %q, want pending", delivery.Status)
	}

	serverError := 500
	if _, err := s.CreateAttempt(ctx, herald.DeliveryAttempt{
		ID: newID(t), TenantID: f.tenant.ID, DeliveryID: delivery.ID, AttemptNumber: 1,
		StatusCode: &serverError, Success: false, ResponseSnippet: "boom",
		Duration: 120 * time.Millisecond,
	}); err != nil {
		t.Fatalf("record the failed attempt: %v", err)
	}
	if _, err := s.CreateAttempt(ctx, herald.DeliveryAttempt{
		ID: newID(t), TenantID: f.tenant.ID, DeliveryID: delivery.ID, AttemptNumber: 1,
		Success: false, Error: "dial tcp: connection refused", Duration: 30 * time.Millisecond,
	}); err != nil {
		t.Fatalf("record a re-run of the same attempt: %v", err)
	}
	if err := s.UpdateDeliveryStatus(ctx, delivery.ID, herald.DeliveryFailed); err != nil {
		t.Fatalf("mark the delivery failed: %v", err)
	}

	attempts, err := s.AttemptsByMessage(ctx, f.tenant.ID, message.ID)
	if err != nil {
		t.Fatalf("read attempts: %v", err)
	}
	if len(attempts) != 2 {
		t.Fatalf("attempts = %d, want one row per execution (2)", len(attempts))
	}

	first, second := attempts[0], attempts[1]
	if first.StatusCode == nil || *first.StatusCode != serverError {
		t.Errorf("first attempt status code = %v, want %d", first.StatusCode, serverError)
	}
	if first.Duration != 120*time.Millisecond {
		t.Errorf("first attempt lasted %v, want 120ms", first.Duration)
	}
	if first.ResponseSnippet != "boom" {
		t.Errorf("first attempt snippet = %q, want the captured response", first.ResponseSnippet)
	}
	if second.StatusCode != nil {
		t.Errorf("an attempt that got no response reports status code %d", *second.StatusCode)
	}
	if second.Error == "" {
		t.Errorf("an attempt that got no response recorded no error text")
	}

	after, err := s.DeliveriesByMessage(ctx, f.tenant.ID, message.ID)
	if err != nil {
		t.Fatalf("read deliveries: %v", err)
	}
	if after[0].Status != herald.DeliveryFailed {
		t.Errorf("delivery status = %q, want failed", after[0].Status)
	}
	if !after[0].UpdatedAt.After(after[0].CreatedAt) {
		t.Errorf("the delivery's updated_at was not advanced by the status change")
	}
}

func TestADeliveryOnlyEverHoldsADocumentedStatus(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	f := newTenant(t, s, "acme", "billing")
	newEndpoint(t, s, f, "https://example.com/hooks", nil, false)
	_, deliveries := ingest(t, s, pool, f, "invoice.paid", `{}`)

	if err := s.UpdateDeliveryStatus(ctx, deliveries[0].ID, herald.DeliveryStatus("sending")); err == nil {
		t.Errorf("an undocumented delivery status was accepted")
	}
	if err := s.UpdateDeliveryStatus(ctx, newID(t), herald.DeliveryDelivered); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("updating an unknown delivery returned %v, want ErrNotFound", err)
	}
	if err := s.UpdateDeliveryStatus(ctx, deliveries[0].ID, herald.DeliveryDelivered); err != nil {
		t.Errorf("marking a delivery delivered failed: %v", err)
	}
}

// The transaction the store hands out is a plain pgx transaction, so
// the queue's insert and the store's writes are the same unit of work
// rather than two that merely look alike.
func TestTheTransactionTheStoreHandsOutIsTheOneTheQueueWritesThrough(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()

	tx, err := s.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // a rollback after commit is a no-op

	var _ pgx.Tx = tx

	if _, err := newQueue(t, pool).InsertTx(ctx, tx, deliveryJob{DeliveryID: newID(t)}); err != nil {
		t.Fatalf("the queue could not write through the store's transaction: %v", err)
	}
	if got := countRows(t, pool, "drover_jobs"); got != 0 {
		t.Errorf("the queue's insert was visible outside the transaction: %d jobs", got)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if got := countRows(t, pool, "drover_jobs"); got != 1 {
		t.Errorf("jobs after commit = %d, want 1", got)
	}
}
