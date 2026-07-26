//go:build integration

package deliver

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/augusto-dmh/drover"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/augusto-dmh/herald/internal/herald"
	"github.com/augusto-dmh/herald/internal/migrate"
	"github.com/augusto-dmh/herald/internal/store"
	"github.com/augusto-dmh/herald/internal/testdb"
)

func TestMain(m *testing.M) { os.Exit(testdb.RunMain(m)) }

const testPayload = `{"invoice":"inv_1","total":10}`

// fixture is one tenant's message, ready to be delivered to whatever
// endpoints a test registers for it.
type fixture struct {
	store   *store.Store
	pool    *pgxpool.Pool
	tenant  herald.Tenant
	message herald.Message
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	pool := testdb.NewDB(t, migrate.Migrate, drover.Migrate)
	s := store.New(pool)

	tenant, err := s.CreateTenant(ctx, herald.Tenant{ID: newID(t), Name: "acme"})
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	application, err := s.CreateApplication(ctx, herald.Application{
		ID: newID(t), TenantID: tenant.ID, UID: "billing", Name: "Billing",
	})
	if err != nil {
		t.Fatalf("create application: %v", err)
	}

	tx, err := s.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	message, err := s.CreateMessageTx(ctx, tx, herald.Message{
		ID: newID(t), TenantID: tenant.ID, ApplicationID: application.ID,
		EventType: "invoice.paid", Payload: json.RawMessage(testPayload),
	})
	if err != nil {
		t.Fatalf("create message: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	return &fixture{store: s, pool: pool, tenant: tenant, message: message}
}

// deliveryTo registers an endpoint at url and the pending delivery of
// the fixture's message to it.
func (f *fixture) deliveryTo(t *testing.T, url string) herald.Delivery {
	t.Helper()
	ctx := context.Background()

	endpoint, err := f.store.CreateEndpoint(ctx, herald.Endpoint{
		ID: newID(t), TenantID: f.tenant.ID, ApplicationID: f.message.ApplicationID, URL: url,
	})
	if err != nil {
		t.Fatalf("create endpoint: %v", err)
	}

	tx, err := f.store.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	delivery, err := f.store.CreateDeliveryTx(ctx, tx, herald.Delivery{
		ID: newID(t), TenantID: f.tenant.ID, MessageID: f.message.ID, EndpointID: endpoint.ID,
	})
	if err != nil {
		t.Fatalf("create delivery: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return delivery
}

// attemptsFor returns what was recorded for one delivery.
func (f *fixture) attemptsFor(t *testing.T, deliveryID uuid.UUID) []herald.DeliveryAttempt {
	t.Helper()
	all, err := f.store.AttemptsByMessage(context.Background(), f.tenant.ID, f.message.ID)
	if err != nil {
		t.Fatalf("read attempts: %v", err)
	}
	var mine []herald.DeliveryAttempt
	for _, a := range all {
		if a.DeliveryID == deliveryID {
			mine = append(mine, a)
		}
	}
	return mine
}

// statusOf reads back the delivery the worker was asked to run.
func (f *fixture) statusOf(t *testing.T, deliveryID uuid.UUID) herald.DeliveryStatus {
	t.Helper()
	work, err := f.store.DeliveryWorkForJob(context.Background(), deliveryID)
	if err != nil {
		t.Fatalf("read delivery %s: %v", deliveryID, err)
	}
	return work.Delivery.Status
}

// worker returns a worker recording through the fixture's store.
func (f *fixture) worker(t *testing.T, cfg Config) *Worker {
	t.Helper()
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	return NewWorker(f.store, cfg)
}

func newID(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := herald.NewID()
	if err != nil {
		t.Fatalf("new id: %v", err)
	}
	return id
}

// run executes one delivery job the way the queue would.
func run(t *testing.T, w *Worker, delivery herald.Delivery, attempt int) error {
	t.Helper()
	return w.Work(context.Background(), &drover.Job[Args]{
		ID: 1, Attempt: attempt, Args: Args{DeliveryID: delivery.ID},
	})
}

// unreachableURL is an address nothing is listening on.
func unreachableURL(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve an address nothing listens on: %v", err)
	}
	url := "http://" + listener.Addr().String() + "/hooks"
	if err := listener.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	return url
}

// countRows reports how many rows a table holds.
func (f *fixture) countRows(t *testing.T, table string) int {
	t.Helper()
	var count int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM `+table).Scan(&count); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return count
}

// One execution leaves one attempt — except when there is nothing to
// leave it against. A job naming a delivery herald cannot read has no
// tenant, no endpoint and no message, so there is no row it could
// honestly write; the queue is told the job failed and the log stays
// empty rather than gaining an attempt at a delivery that is not there.
func TestAJobNamingADeliveryHeraldCannotReadRecordsNothing(t *testing.T) {
	f := newFixture(t)
	missing := herald.Delivery{ID: newID(t)}

	if err := run(t, f.worker(t, Config{}), missing, 1); err == nil {
		t.Errorf("the queue was told a job for an unknown delivery succeeded")
	}
	if got := f.countRows(t, "delivery_attempts"); got != 0 {
		t.Errorf("delivery_attempts holds %d rows, want none: there was nothing to attempt", got)
	}
}

// A delivery the endpoint accepted leaves one attempt saying so and a
// delivery marked delivered, and the queue is told the job succeeded.
func TestAnAcceptedMessageIsRecordedOnceAndMarkedDelivered(t *testing.T) {
	f := newFixture(t)
	endpoint := newReceiver(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte("thanks")); err != nil {
			t.Errorf("receiver: write body: %v", err)
		}
	})
	delivery := f.deliveryTo(t, endpoint.url)

	if err := run(t, f.worker(t, Config{}), delivery, 1); err != nil {
		t.Fatalf("the queue was told a delivered message failed: %v", err)
	}

	if endpoint.calls != 1 {
		t.Errorf("the endpoint was called %d time(s), want 1", endpoint.calls)
	}
	var arrived map[string]any
	if err := json.Unmarshal(endpoint.body, &arrived); err != nil {
		t.Fatalf("the endpoint did not receive JSON: %v (%s)", err, endpoint.body)
	}
	if arrived["invoice"] != "inv_1" {
		t.Errorf("the endpoint received %v, want the submitted payload", arrived)
	}

	if got := f.statusOf(t, delivery.ID); got != herald.DeliveryDelivered {
		t.Errorf("delivery status = %q, want delivered", got)
	}
	attempts := f.attemptsFor(t, delivery.ID)
	if len(attempts) != 1 {
		t.Fatalf("attempts = %d, want exactly 1", len(attempts))
	}
	made := attempts[0]
	if !made.Success {
		t.Errorf("the attempt that got a 200 is recorded as unsuccessful")
	}
	if made.StatusCode == nil || *made.StatusCode != http.StatusOK {
		t.Errorf("attempt status code = %v, want 200", made.StatusCode)
	}
	if made.ResponseSnippet != "thanks" {
		t.Errorf("attempt snippet = %q, want what the endpoint answered", made.ResponseSnippet)
	}
	if made.Error != "" {
		t.Errorf("a delivered attempt carries error text %q", made.Error)
	}
	if made.AttemptNumber != 1 {
		t.Errorf("attempt number = %d, want 1", made.AttemptNumber)
	}
	if made.TenantID != f.tenant.ID {
		t.Errorf("the attempt was filed under tenant %s, want %s", made.TenantID, f.tenant.ID)
	}
}

// Every way a delivery can fail is still an execution, so each leaves
// exactly one attempt, marks the delivery failed, and reports the job
// as failed to the queue.
func TestEveryFailedExecutionStillLeavesExactlyOneAttempt(t *testing.T) {
	refusing := newReceiver(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		if _, err := w.Write([]byte("boom")); err != nil {
			t.Errorf("receiver: write body: %v", err)
		}
	})

	cases := []struct {
		name           string
		url            string
		wantStatusCode *int
	}{
		{"an endpoint that refused the message", refusing.url, ptr(http.StatusInternalServerError)},
		{"an endpoint that could not be reached", unreachableURL(t), nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			delivery := f.deliveryTo(t, c.url)

			if err := run(t, f.worker(t, Config{}), delivery, 1); err == nil {
				t.Errorf("the queue was told a failed delivery succeeded")
			}

			if got := f.statusOf(t, delivery.ID); got != herald.DeliveryFailed {
				t.Errorf("delivery status = %q, want failed", got)
			}
			attempts := f.attemptsFor(t, delivery.ID)
			if len(attempts) != 1 {
				t.Fatalf("attempts = %d, want exactly 1", len(attempts))
			}
			made := attempts[0]
			if made.Success {
				t.Errorf("a failed delivery is recorded as successful")
			}
			if made.Error == "" {
				t.Errorf("the attempt says nothing about why it failed")
			}
			switch {
			case c.wantStatusCode == nil && made.StatusCode != nil:
				t.Errorf("attempt status code = %d, want none: nothing answered", *made.StatusCode)
			case c.wantStatusCode != nil && (made.StatusCode == nil || *made.StatusCode != *c.wantStatusCode):
				t.Errorf("attempt status code = %v, want %d", made.StatusCode, *c.wantStatusCode)
			}
		})
	}
}

// Deliveries of one message are independent: what one endpoint does
// with it says nothing about what happened at another.
func TestOneEndpointFailingLeavesTheOtherDeliveryAlone(t *testing.T) {
	f := newFixture(t)
	accepting := newReceiver(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	refusing := newReceiver(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	delivered := f.deliveryTo(t, accepting.url)
	failed := f.deliveryTo(t, refusing.url)

	worker := f.worker(t, Config{})
	if err := run(t, worker, failed, 1); err == nil {
		t.Errorf("the refused delivery was reported as succeeding")
	}
	if err := run(t, worker, delivered, 1); err != nil {
		t.Errorf("the accepted delivery failed after another endpoint refused: %v", err)
	}

	if got := f.statusOf(t, delivered.ID); got != herald.DeliveryDelivered {
		t.Errorf("the accepted endpoint's delivery is %q, want delivered", got)
	}
	if got := f.statusOf(t, failed.ID); got != herald.DeliveryFailed {
		t.Errorf("the refusing endpoint's delivery is %q, want failed", got)
	}
	if got := len(f.attemptsFor(t, delivered.ID)); got != 1 {
		t.Errorf("the accepted delivery has %d attempts, want 1", got)
	}
	if got := len(f.attemptsFor(t, failed.ID)); got != 1 {
		t.Errorf("the failed delivery has %d attempts, want 1", got)
	}
	if accepting.calls != 1 {
		t.Errorf("the accepting endpoint was called %d time(s), want 1", accepting.calls)
	}
}

// The job carries an identifier, not a snapshot, so the endpoint the
// message goes to is the one registered when the job runs.
func TestTheMessageGoesToTheEndpointsCurrentURL(t *testing.T) {
	f := newFixture(t)
	stale := newReceiver(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	current := newReceiver(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	delivery := f.deliveryTo(t, stale.url)

	if _, err := f.pool.Exec(context.Background(),
		`UPDATE endpoints SET url = $2 WHERE id = $1`, delivery.EndpointID, current.url); err != nil {
		t.Fatalf("move the endpoint: %v", err)
	}

	if err := run(t, f.worker(t, Config{}), delivery, 1); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	if current.calls != 1 {
		t.Errorf("the endpoint's current URL was called %d time(s), want 1", current.calls)
	}
	if stale.calls != 0 {
		t.Errorf("the message went to the URL the endpoint no longer has")
	}
}

// A delivery run twice is two executions, and the log says so rather
// than overwriting what the first one found.
func TestASecondExecutionOfADeliveryIsItsOwnAttempt(t *testing.T) {
	f := newFixture(t)
	answers := []int{http.StatusInternalServerError, http.StatusOK}
	endpoint := newReceiver(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(answers[0])
		answers = answers[1:]
	})
	delivery := f.deliveryTo(t, endpoint.url)

	worker := f.worker(t, Config{})
	if err := run(t, worker, delivery, 1); err == nil {
		t.Errorf("the refused first execution was reported as succeeding")
	}
	if err := run(t, worker, delivery, 2); err != nil {
		t.Errorf("the accepted second execution was reported as failing: %v", err)
	}

	attempts := f.attemptsFor(t, delivery.ID)
	if len(attempts) != 2 {
		t.Fatalf("attempts = %d, want one per execution", len(attempts))
	}
	if attempts[0].AttemptNumber != 1 || attempts[1].AttemptNumber != 2 {
		t.Errorf("attempt numbers = %d, %d, want 1, 2",
			attempts[0].AttemptNumber, attempts[1].AttemptNumber)
	}
	if attempts[0].Success || !attempts[1].Success {
		t.Errorf("the log does not distinguish the refused execution from the accepted one")
	}
	if got := f.statusOf(t, delivery.ID); got != herald.DeliveryDelivered {
		t.Errorf("delivery status = %q, want the last execution's outcome", got)
	}
}

// An attempt is recorded whatever the receiver answered with. A body
// full of NULs, cut mid-character at the capture cap, is exactly the
// kind of answer that must not be able to strand a delivery pending by
// making its attempt row unwritable.
func TestAReceiverAnsweringRawBytesStillLeavesAnAttemptRow(t *testing.T) {
	f := newFixture(t)
	body := hostileBody()
	endpoint := newReceiver(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write(body); err != nil {
			t.Errorf("receiver: write body: %v", err)
		}
	})
	delivery := f.deliveryTo(t, endpoint.url)

	if err := run(t, f.worker(t, Config{}), delivery, 1); err != nil {
		t.Fatalf("what the receiver put in its body failed the delivery: %v", err)
	}

	if got := f.statusOf(t, delivery.ID); got != herald.DeliveryDelivered {
		t.Errorf("delivery status = %q, want delivered", got)
	}
	attempts := f.attemptsFor(t, delivery.ID)
	if len(attempts) != 1 {
		t.Fatalf("attempts = %d, want exactly 1", len(attempts))
	}
	stored := attempts[0].ResponseSnippet
	if !utf8.ValidString(stored) {
		t.Errorf("the stored snippet is not valid UTF-8")
	}
	if strings.ContainsRune(stored, 0) {
		t.Errorf("the stored snippet carries a NUL")
	}
	if !strings.Contains(stored, "x") {
		t.Errorf("the readable part of the response was not stored: %q", stored)
	}
}

func ptr[T any](v T) *T { return &v }
