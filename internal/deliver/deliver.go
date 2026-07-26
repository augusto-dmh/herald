// Package deliver carries a message to one endpoint. Ingest enqueues
// one job per delivery and this package's worker executes them.
//
// One execution leaves exactly one attempt row, whatever happened. A
// 2xx response, a 500, a refused connection and a timeout are four
// different records of the same thing — that herald tried once and here
// is what came back — and only the first of them marks the delivery
// delivered. Anything else marks it failed and is reported to the queue
// as a failed job, so the queue's record of the work and herald's
// record of the delivery never disagree.
package deliver

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/augusto-dmh/drover"
	"github.com/google/uuid"

	"github.com/augusto-dmh/herald/internal/herald"
	"github.com/augusto-dmh/herald/internal/store"
)

const (
	// DefaultTimeout bounds one attempt end to end — connection,
	// request, response and the reading of the body. A receiver that
	// never answers costs herald this long and no longer.
	DefaultTimeout = 15 * time.Second

	// snippetBytes is how much of a response body is kept alongside the
	// attempt (ADR-0006). It exists to make a failure diagnosable, not to
	// archive what the receiver said, so a body larger than this is
	// truncated and the outcome is decided by the status code either way.
	snippetBytes = 4 << 10
)

// Args is what an enqueued delivery job carries: the identifier of the
// delivery it is for, and nothing else. The worker re-reads the
// delivery, its endpoint and its message when it runs, so a URL changed
// between enqueue and execution is honored rather than frozen into the
// job at the moment it was created.
type Args struct {
	DeliveryID uuid.UUID `json:"delivery_id"`
}

// Kind names the job type the queue dispatches on.
func (Args) Kind() string { return "webhook_delivery" }

// Config carries what the worker needs beyond its store.
type Config struct {
	// Timeout bounds one attempt. Zero means DefaultTimeout — never "no
	// limit", which would let one unresponsive receiver hold the queue's
	// only loop forever.
	Timeout time.Duration

	// Logger receives what happened around an attempt that the attempt
	// row itself does not carry. Defaults to slog.Default().
	Logger *slog.Logger
}

// Worker executes delivery jobs.
type Worker struct {
	drover.WorkerDefaults[Args]

	store  *store.Store
	client *http.Client
	log    *slog.Logger
}

// NewWorker returns a worker that reads and records through the given
// store.
//
// Its HTTP client does not follow redirects. A 3xx is a destination
// telling herald to deliver somewhere else, and where a webhook goes is
// the tenant's registration to decide, not the receiver's answer
// (ADR-0003) — so a redirect is recorded as the non-2xx response it is.
func NewWorker(s *store.Store, cfg Config) *Worker {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Worker{
		store: s,
		client: &http.Client{
			Timeout: timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		log: log,
	}
}

// Work delivers one message to one endpoint and records what happened.
//
// The attempt is written before the outcome is returned, on every path,
// so a delivery that failed and a delivery that succeeded are equally
// accounted for. The error returned on failure is what tells the queue
// the job did not succeed; the delivery row marked failed is herald's
// own record of the same fact, and the API reports that one.
func (w *Worker) Work(ctx context.Context, job *drover.Job[Args]) error {
	work, err := w.store.DeliveryWorkForJob(ctx, job.Args.DeliveryID)
	if err != nil {
		return fmt.Errorf("deliver: load delivery %s: %w", job.Args.DeliveryID, err)
	}

	result := w.post(ctx, work.Endpoint.URL, work.Message.Payload)

	attemptID, err := herald.NewID()
	if err != nil {
		return err
	}
	if _, err := w.store.CreateAttempt(ctx, herald.DeliveryAttempt{
		ID:              attemptID,
		TenantID:        work.Delivery.TenantID,
		DeliveryID:      work.Delivery.ID,
		AttemptNumber:   attemptNumber(job.Attempt),
		StatusCode:      result.statusCode,
		Success:         result.success,
		Error:           result.errorText,
		ResponseSnippet: result.snippet,
		Duration:        result.duration,
	}); err != nil {
		return fmt.Errorf("deliver: record attempt for delivery %s: %w", work.Delivery.ID, err)
	}

	status := herald.DeliveryFailed
	if result.success {
		status = herald.DeliveryDelivered
	}
	if err := w.store.UpdateDeliveryStatus(ctx, work.Delivery.ID, status); err != nil {
		return fmt.Errorf("deliver: mark delivery %s %s: %w", work.Delivery.ID, status, err)
	}

	if !result.success {
		w.log.Warn("delivery failed",
			"delivery_id", work.Delivery.ID, "endpoint_id", work.Endpoint.ID,
			"status_code", result.statusCode, "error", result.errorText)
		return fmt.Errorf("deliver: delivery %s to %s: %s",
			work.Delivery.ID, work.Endpoint.URL, result.errorText)
	}
	return nil
}

// attemptNumber turns the queue's execution counter into the number
// recorded on the attempt. The queue counts from one on the first run;
// the guard is here because an attempt that claims to be the zeroth
// would be rejected by the schema, and losing the record of a delivery
// over an off-by-one is worse than recording it as the first.
func attemptNumber(queueAttempt int) int {
	if queueAttempt < 1 {
		return 1
	}
	return queueAttempt
}

// outcome is what one POST produced, in the terms the attempt row is
// written in. statusCode is nil exactly when no response arrived at
// all, which is what separates a receiver that refused the message from
// one that never answered.
type outcome struct {
	statusCode *int
	success    bool
	errorText  string
	snippet    string
	duration   time.Duration
}

// post sends the payload and reports what came back. It never returns
// an error: a transport failure is an outcome to be recorded, not a
// reason to skip recording one.
func (w *Worker) post(ctx context.Context, url string, payload []byte) outcome {
	start := time.Now()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return outcome{errorText: err.Error(), duration: time.Since(start)}
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := w.client.Do(req)
	if err != nil {
		return outcome{errorText: err.Error(), duration: time.Since(start)}
	}
	defer func() { _ = resp.Body.Close() }()

	// Only the snippet's worth of body is read. What is left unread is
	// discarded with the connection rather than drained, which costs a
	// connection and saves reading a body herald has no use for.
	body, err := io.ReadAll(io.LimitReader(resp.Body, snippetBytes))
	if err != nil {
		// A body that could not be finished does not change what the
		// endpoint answered: the status code is the outcome.
		w.log.Warn("read response body", "url", url, "error", err)
	}

	code := resp.StatusCode
	result := outcome{
		statusCode: &code,
		success:    code >= 200 && code < 300,
		snippet:    string(body),
		duration:   time.Since(start),
	}
	if !result.success {
		result.errorText = fmt.Sprintf("endpoint responded %d %s",
			code, http.StatusText(code))
	}
	return result
}
