//go:build integration

// Package e2e runs herald whole: the HTTP API, the queue's worker loop
// and the delivery worker over one database, with real HTTP in both
// directions.
//
// Every other suite in this repository can only prove that one layer
// keeps its own promise. These tests exist for the promise no layer
// makes alone — that a message a caller POSTs is carried to the URL
// that caller registered, and that what happened on the way is
// afterwards readable back through the same API.
package e2e

import (
	"bytes"
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
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/augusto-dmh/herald/internal/deliver"
	"github.com/augusto-dmh/herald/internal/httpapi"
	"github.com/augusto-dmh/herald/internal/migrate"
	"github.com/augusto-dmh/herald/internal/store"
	"github.com/augusto-dmh/herald/internal/testdb"
)

func TestMain(m *testing.M) { os.Exit(testdb.RunMain(m)) }

const (
	// operatorCredential is what this suite configures the server to
	// accept for tenant bootstrap.
	operatorCredential = "operator-bootstrap-value"

	// settleTimeout bounds how long a test waits for the system to
	// finish. It is generous because a slow container should fail slowly
	// rather than flakily; nothing waits for it when the work is done.
	settleTimeout = 30 * time.Second

	// pollInterval is how often the loop looks for work and how often
	// these tests look at the result. Both are short so the suite
	// finishes in the time the work actually takes.
	pollInterval = 10 * time.Millisecond
)

// stack is herald as an operator would run it, plus the database behind
// it so a test can also read what no route exposes.
type stack struct {
	url    string
	pool   *pgxpool.Pool
	client *http.Client
}

func newStack(t *testing.T) *stack {
	t.Helper()
	discard := slog.New(slog.DiscardHandler)
	pool := testdb.NewDB(t, migrate.Migrate, drover.Migrate)
	s := store.New(pool)

	workers := drover.NewWorkers()
	drover.Register[deliver.Args](workers, deliver.NewWorker(s, deliver.Config{Logger: discard}))

	queue, err := drover.NewClient(pool, drover.Config{
		Workers:      workers,
		Logger:       discard,
		PollInterval: pollInterval,
	})
	if err != nil {
		t.Fatalf("new queue client: %v", err)
	}

	api := httptest.NewServer(httpapi.NewServer(s, queue, httpapi.Config{
		BootstrapToken: operatorCredential,
		Logger:         discard,
	}))
	t.Cleanup(api.Close)

	// The same client the API enqueues through is the one that runs the
	// jobs, exactly as a single herald process would.
	ctx, cancel := context.WithCancel(context.Background())
	loop := make(chan error, 1)
	go func() { loop <- queue.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-loop; err != nil {
			t.Errorf("worker loop: %v", err)
		}
	})

	return &stack{url: api.URL, pool: pool, client: api.Client()}
}

// call sends one real HTTP request to the running API.
func (s *stack) call(t *testing.T, method, path, credential, body string) (int, []byte) {
	t.Helper()

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, s.url+path, reader)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, path, err)
	}
	if credential != "" {
		req.Header.Set("Authorization", "Bearer "+credential)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := s.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	answered, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: read body: %v", method, path, err)
	}
	return resp.StatusCode, answered
}

// expect sends a request and insists on the status the caller planned
// for, decoding the answer into dst.
func (s *stack) expect(t *testing.T, want int, method, path, credential, body string, dst any) {
	t.Helper()
	status, answered := s.call(t, method, path, credential, body)
	if status != want {
		t.Fatalf("%s %s: status %d, want %d (%s)", method, path, status, want, answered)
	}
	if dst == nil {
		return
	}
	if err := json.Unmarshal(answered, dst); err != nil {
		t.Fatalf("%s %s: answer is not JSON: %v (%s)", method, path, err, answered)
	}
}

// jobState reports what the queue made of the job behind a delivery.
// The API deliberately never exposes this, so the test reads it where
// it lives.
func (s *stack) jobState(t *testing.T, deliveryID string) string {
	t.Helper()
	var state string
	if err := s.pool.QueryRow(context.Background(),
		`SELECT state FROM drover_jobs WHERE args->>'delivery_id' = $1`,
		deliveryID).Scan(&state); err != nil {
		t.Fatalf("read the job behind delivery %s: %v", deliveryID, err)
	}
	return state
}

// eventually waits for the running system to reach a state, checking
// rather than sleeping for a guessed duration, and gives up loudly.
func eventually(t *testing.T, what string, reached func() bool) {
	t.Helper()
	deadline := time.Now().Add(settleTimeout)
	for time.Now().Before(deadline) {
		if reached() {
			return
		}
		time.Sleep(pollInterval)
	}
	t.Fatalf("timed out after %v waiting for %s", settleTimeout, what)
}

// The JSON this API answers with, named here rather than borrowed from
// the server, so these tests read the contract a client reads.

type createdTenant struct {
	Tenant struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"tenant"`
	APIKey struct {
		Key   string `json:"key"`
		Scope string `json:"scope"`
	} `json:"api_key"`
}

type createdEndpoint struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

type acceptedMessage struct {
	ID string `json:"id"`
}

type reportedAttempt struct {
	AttemptNumber   int    `json:"attempt_number"`
	StatusCode      *int   `json:"status_code"`
	Success         bool   `json:"success"`
	Error           string `json:"error"`
	ResponseSnippet string `json:"response_snippet"`
	DurationMS      int64  `json:"duration_ms"`
}

type reportedDelivery struct {
	ID         string            `json:"id"`
	EndpointID string            `json:"endpoint_id"`
	Status     string            `json:"status"`
	Attempts   []reportedAttempt `json:"attempts"`
}

type reportedMessage struct {
	ID         string             `json:"id"`
	EventType  string             `json:"event_type"`
	Payload    json.RawMessage    `json:"payload"`
	Deliveries []reportedDelivery `json:"deliveries"`
}

// tenant bootstraps a tenant with an application, the setting every
// test below starts from.
func (s *stack) tenant(t *testing.T, name, appUID string) string {
	t.Helper()

	var created createdTenant
	s.expect(t, http.StatusCreated, http.MethodPost, "/v1/tenants",
		operatorCredential, `{"name":"`+name+`"}`, &created)
	if created.APIKey.Key == "" {
		t.Fatalf("bootstrapping %s returned no key", name)
	}
	s.expect(t, http.StatusCreated, http.MethodPost, "/v1/applications",
		created.APIKey.Key, `{"uid":"`+appUID+`","name":"`+name+` app"}`, nil)
	return created.APIKey.Key
}

// endpoint registers url as a destination of the application.
func (s *stack) endpoint(t *testing.T, key, appUID, url string) string {
	t.Helper()
	var created createdEndpoint
	s.expect(t, http.StatusCreated, http.MethodPost, "/v1/applications/"+appUID+"/endpoints",
		key, `{"url":"`+url+`"}`, &created)
	return created.ID
}

// receiver is a destination herald delivers to, together with what it
// observed.
type receiver struct {
	url    string
	method string
	body   []byte
	calls  int
}

func newReceiver(t *testing.T, status int, answer string) *receiver {
	t.Helper()
	got := &receiver{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("receiver: read body: %v", err)
		}
		got.calls++
		got.method = r.Method
		got.body = body
		w.WriteHeader(status)
		if _, err := io.WriteString(w, answer); err != nil {
			t.Errorf("receiver: write body: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	got.url = srv.URL + "/hooks"
	return got
}

// settle waits until the message's deliveries have all left pending and
// returns what the API then reports about it.
func (s *stack) settle(t *testing.T, key, appUID, messageID string, deliveries int) reportedMessage {
	t.Helper()
	path := "/v1/applications/" + appUID + "/messages/" + messageID

	var reported reportedMessage
	eventually(t, "every delivery of the message to be attempted", func() bool {
		reported = reportedMessage{}
		s.expect(t, http.StatusOK, http.MethodGet, path, key, "", &reported)
		if len(reported.Deliveries) != deliveries {
			return false
		}
		for _, d := range reported.Deliveries {
			if d.Status == "pending" {
				return false
			}
		}
		return true
	})
	return reported
}

// The whole point of the service, proved once: a caller POSTs a
// message and the URL that caller registered receives it, then the same
// caller reads back what happened.
func TestAPostedMessageReachesItsEndpointAndTheAttemptIsQueryable(t *testing.T) {
	s := newStack(t)
	endpoint := newReceiver(t, http.StatusOK, "received")
	key := s.tenant(t, "Acme", "billing")
	endpointID := s.endpoint(t, key, "billing", endpoint.url)

	// A document nothing may quietly tidy: the keys are out of order,
	// one of them is repeated, and the whitespace is the submitter's.
	// Parsing and re-serializing this — which a JSON column would do —
	// changes every one of those, and a signature over the tidied bytes
	// would not verify against the bytes the tenant sent.
	payload := `{"zebra":1,  "alpha":2,"zebra":3}`
	var accepted acceptedMessage
	s.expect(t, http.StatusAccepted, http.MethodPost, "/v1/applications/billing/messages",
		key, `{"event_type":"invoice.paid","payload":`+payload+`}`, &accepted)

	reported := s.settle(t, key, "billing", accepted.ID, 1)

	if reported.ID != accepted.ID || reported.EventType != "invoice.paid" {
		t.Errorf("read back message %s (%s), want %s (invoice.paid)",
			reported.ID, reported.EventType, accepted.ID)
	}

	// The API answers with the stored document as JSON, not as an
	// encoded blob. The one thing between it and the submitted bytes is
	// the response encoder's whitespace compaction; key order and the
	// repeated key — the things normalization destroys — must survive.
	var compacted bytes.Buffer
	if err := json.Compact(&compacted, []byte(payload)); err != nil {
		t.Fatalf("the submitted payload is not JSON: %v", err)
	}
	if !bytes.Equal(reported.Payload, compacted.Bytes()) {
		t.Errorf("the API reports payload %s, want %s", reported.Payload, compacted.Bytes())
	}

	delivery := reported.Deliveries[0]
	if delivery.EndpointID != endpointID {
		t.Errorf("the delivery names endpoint %s, want the registered %s",
			delivery.EndpointID, endpointID)
	}
	if delivery.Status != "delivered" {
		t.Errorf("delivery status = %q, want delivered", delivery.Status)
	}
	if len(delivery.Attempts) != 1 {
		t.Fatalf("attempts = %d, want exactly one for the one execution", len(delivery.Attempts))
	}

	made := delivery.Attempts[0]
	if !made.Success {
		t.Errorf("the attempt the endpoint accepted is reported as unsuccessful")
	}
	if made.StatusCode == nil || *made.StatusCode != http.StatusOK {
		t.Errorf("attempt status code = %v, want the 200 the receiver answered", made.StatusCode)
	}
	if made.ResponseSnippet != "received" {
		t.Errorf("attempt snippet = %q, want what the receiver answered", made.ResponseSnippet)
	}
	if made.AttemptNumber != 1 {
		t.Errorf("attempt number = %d, want 1", made.AttemptNumber)
	}
	// The reported duration is a measurement of the round trip. It is
	// bounded rather than required to be positive because a delivery to
	// a receiver on this machine can legitimately take under a
	// millisecond and round to zero; what it cannot be is negative, or
	// longer than an attempt is allowed to last.
	if made.DurationMS < 0 || made.DurationMS > deliver.DefaultTimeout.Milliseconds() {
		t.Errorf("attempt duration = %dms, want a measurement within the %v an attempt may take",
			made.DurationMS, deliver.DefaultTimeout)
	}

	// The message really travelled: the receiver was called once, with
	// the document that was submitted.
	if endpoint.calls != 1 {
		t.Fatalf("the receiver was called %d time(s), want 1", endpoint.calls)
	}
	if endpoint.method != http.MethodPost {
		t.Errorf("the receiver was called with %s, want POST", endpoint.method)
	}
	// Byte for byte, whitespace and repeated key included: what herald
	// carries to an endpoint is what the tenant handed it, because that
	// is what a signature will have to cover.
	if !bytes.Equal(endpoint.body, []byte(payload)) {
		t.Errorf("the receiver got %s, want the submitted bytes %s", endpoint.body, payload)
	}

	eventually(t, "the queue to finalize the job", func() bool {
		return s.jobState(t, delivery.ID) != "running"
	})
	if got := s.jobState(t, delivery.ID); got != "completed" {
		t.Errorf("the queue recorded the job as %q, want completed", got)
	}
}

// A receiver that refuses the message is reported as the failure it
// was, in full detail — and its failure says nothing about the endpoint
// next to it, which got the same message and took it.
func TestARefusedDeliveryIsReportedFailedAndLeavesTheOtherAlone(t *testing.T) {
	s := newStack(t)
	accepting := newReceiver(t, http.StatusOK, "ok")
	refusing := newReceiver(t, http.StatusInternalServerError, "no thanks")

	key := s.tenant(t, "Acme", "billing")
	acceptingID := s.endpoint(t, key, "billing", accepting.url)
	refusingID := s.endpoint(t, key, "billing", refusing.url)

	var accepted acceptedMessage
	s.expect(t, http.StatusAccepted, http.MethodPost, "/v1/applications/billing/messages",
		key, `{"event_type":"invoice.paid","payload":{"total":10}}`, &accepted)

	reported := s.settle(t, key, "billing", accepted.ID, 2)

	byEndpoint := make(map[string]reportedDelivery, len(reported.Deliveries))
	for _, d := range reported.Deliveries {
		byEndpoint[d.EndpointID] = d
	}

	delivered, ok := byEndpoint[acceptingID]
	if !ok {
		t.Fatalf("the accepting endpoint has no delivery: %+v", reported.Deliveries)
	}
	failed, ok := byEndpoint[refusingID]
	if !ok {
		t.Fatalf("the refusing endpoint has no delivery: %+v", reported.Deliveries)
	}

	if delivered.Status != "delivered" {
		t.Errorf("the accepting endpoint's delivery is %q, want delivered", delivered.Status)
	}
	if failed.Status != "failed" {
		t.Errorf("the refusing endpoint's delivery is %q, want failed", failed.Status)
	}
	if len(delivered.Attempts) != 1 || len(failed.Attempts) != 1 {
		t.Fatalf("attempts = %d delivered, %d failed, want one execution each",
			len(delivered.Attempts), len(failed.Attempts))
	}

	refused := failed.Attempts[0]
	if refused.Success {
		t.Errorf("the refused attempt is reported as successful")
	}
	if refused.StatusCode == nil || *refused.StatusCode != http.StatusInternalServerError {
		t.Errorf("refused attempt status code = %v, want 500", refused.StatusCode)
	}
	if refused.ResponseSnippet != "no thanks" {
		t.Errorf("refused attempt snippet = %q, want what the receiver answered",
			refused.ResponseSnippet)
	}
	if refused.Error == "" {
		t.Errorf("the refused attempt says nothing about why it failed")
	}
	if !delivered.Attempts[0].Success {
		t.Errorf("the accepted attempt is reported as unsuccessful")
	}

	if accepting.calls != 1 {
		t.Errorf("the accepting receiver was called %d time(s), want 1", accepting.calls)
	}
	if refusing.calls != 1 {
		t.Errorf("the refusing receiver was called %d time(s), want 1", refusing.calls)
	}

	// The queue's record agrees with herald's: the delivery that failed
	// is a job that failed, not one quietly marked done.
	eventually(t, "the queue to finalize both jobs", func() bool {
		return s.jobState(t, delivered.ID) != "running" && s.jobState(t, failed.ID) != "running"
	})
	if got := s.jobState(t, delivered.ID); got != "completed" {
		t.Errorf("the delivered job is recorded as %q, want completed", got)
	}
	if got := s.jobState(t, failed.ID); got != "dead" {
		t.Errorf("the failed job is recorded as %q, want dead", got)
	}
}
