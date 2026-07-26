package deliver

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func testWorker(t *testing.T, cfg Config) *Worker {
	t.Helper()
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	// The store is only reached by Work; the POST these tests exercise
	// does not touch it.
	return NewWorker(nil, cfg)
}

// receiver is an endpoint under test together with what it observed, so
// a test can assert both what herald recorded and what actually arrived.
type receiver struct {
	url     string
	method  string
	content string
	body    []byte
	calls   int
}

func newReceiver(t *testing.T, respond func(w http.ResponseWriter, r *http.Request)) *receiver {
	t.Helper()
	got := &receiver{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("receiver: read body: %v", err)
		}
		got.calls++
		got.method = r.Method
		got.content = r.Header.Get("Content-Type")
		got.body = body
		respond(w, r)
	}))
	t.Cleanup(srv.Close)
	got.url = srv.URL
	return got
}

// What the endpoint answered is what decides the outcome: a 2xx is a
// delivery, anything else is a failure carrying the status that made it
// one.
func TestTheAnsweredStatusDecidesWhetherADeliverySucceeded(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		status      int
		wantSuccess bool
	}{
		{"200 OK", http.StatusOK, true},
		{"201 Created", http.StatusCreated, true},
		{"204 No Content", http.StatusNoContent, true},
		{"299", 299, true},
		{"400 Bad Request", http.StatusBadRequest, false},
		{"404 Not Found", http.StatusNotFound, false},
		{"500 Internal Server Error", http.StatusInternalServerError, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			endpoint := newReceiver(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(c.status)
			})
			w := testWorker(t, Config{})

			payload := []byte(`{"invoice":"inv_1","total":10}`)
			got := w.post(context.Background(), endpoint.url, payload)

			if got.success != c.wantSuccess {
				t.Errorf("success = %v, want %v", got.success, c.wantSuccess)
			}
			if got.statusCode == nil || *got.statusCode != c.status {
				t.Errorf("status code = %v, want %d", got.statusCode, c.status)
			}
			if c.wantSuccess && got.errorText != "" {
				t.Errorf("a delivered attempt carries error text %q", got.errorText)
			}
			if !c.wantSuccess && got.errorText == "" {
				t.Errorf("a failed attempt says nothing about why")
			}
			if got.duration < 0 {
				t.Errorf("duration = %v", got.duration)
			}
		})
	}
}

// The message reaches the endpoint as the JSON document it was
// submitted as, announced as such.
func TestTheEndpointReceivesThePayloadAsAJSONPost(t *testing.T) {
	t.Parallel()
	endpoint := newReceiver(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	w := testWorker(t, Config{})

	payload := []byte(`{"invoice":"inv_1","lines":[{"total":10}]}`)
	if got := w.post(context.Background(), endpoint.url, payload); !got.success {
		t.Fatalf("post: %+v", got)
	}

	if endpoint.method != http.MethodPost {
		t.Errorf("endpoint was called with %s, want POST", endpoint.method)
	}
	if mediaType, _, _ := strings.Cut(endpoint.content, ";"); mediaType != "application/json" {
		t.Errorf("content type = %q, want application/json", endpoint.content)
	}

	var sent, arrived any
	if err := json.Unmarshal(payload, &sent); err != nil {
		t.Fatalf("submitted payload is not JSON: %v", err)
	}
	if err := json.Unmarshal(endpoint.body, &arrived); err != nil {
		t.Fatalf("the endpoint did not receive JSON: %v (%s)", err, endpoint.body)
	}
	if !reflect.DeepEqual(sent, arrived) {
		t.Errorf("the endpoint received %s, want %s", endpoint.body, payload)
	}
}

// Where a webhook goes is the tenant's registration to decide, so a
// redirect is recorded as the answer it is rather than obeyed.
func TestARedirectIsRecordedRatherThanFollowed(t *testing.T) {
	t.Parallel()
	elsewhere := newReceiver(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	endpoint := newReceiver(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.url, http.StatusFound)
	})
	w := testWorker(t, Config{})

	got := w.post(context.Background(), endpoint.url, []byte(`{"a":1}`))

	if got.success {
		t.Errorf("a redirect was treated as a delivery")
	}
	if got.statusCode == nil || *got.statusCode != http.StatusFound {
		t.Errorf("status code = %v, want 302", got.statusCode)
	}
	if elsewhere.calls != 0 {
		t.Errorf("the message was delivered to the redirect target %d time(s)", elsewhere.calls)
	}
}

// A receiver that never answered and one that answered badly are
// different facts: only the second has a status code.
func TestAnEndpointThatNeverAnswersIsRecordedWithNoStatusCode(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve an address nothing listens on: %v", err)
	}
	unreachable := "http://" + listener.Addr().String() + "/hooks"
	if err := listener.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	silent := newReceiver(t, func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})

	cases := []struct {
		name string
		url  string
		cfg  Config
	}{
		{"a refused connection", unreachable, Config{}},
		{"a receiver that stops answering", silent.url, Config{Timeout: 100 * time.Millisecond}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			w := testWorker(t, c.cfg)

			got := w.post(context.Background(), c.url, []byte(`{"a":1}`))

			if got.success {
				t.Errorf("%s counted as a delivery", c.name)
			}
			if got.statusCode != nil {
				t.Errorf("%s carries status code %d, want none", c.name, *got.statusCode)
			}
			if got.errorText == "" {
				t.Errorf("%s was recorded without saying what went wrong", c.name)
			}
		})
	}
}

// An attempt is bounded whether or not anyone configured a bound: the
// zero value is the documented deadline, never "wait forever".
func TestAnAttemptIsAlwaysBounded(t *testing.T) {
	t.Parallel()
	if got := testWorker(t, Config{}).client.Timeout; got != DefaultTimeout {
		t.Errorf("unconfigured timeout = %v, want %v", got, DefaultTimeout)
	}
	if got := testWorker(t, Config{Timeout: -1}).client.Timeout; got != DefaultTimeout {
		t.Errorf("a nonsense timeout gave %v, want %v", got, DefaultTimeout)
	}
	if got := testWorker(t, Config{Timeout: 2 * time.Second}).client.Timeout; got != 2*time.Second {
		t.Errorf("configured timeout = %v, want 2s", got)
	}
}

// The snippet exists to make a failure readable, not to store what the
// receiver said, so a large body is cut and the outcome is untouched.
func TestALargeResponseIsCutWithoutChangingTheOutcome(t *testing.T) {
	t.Parallel()
	oversized := strings.Repeat("x", snippetBytes*3)

	cases := []struct {
		name        string
		status      int
		wantSuccess bool
	}{
		{"an accepted message", http.StatusOK, true},
		{"a refused message", http.StatusInternalServerError, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			endpoint := newReceiver(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(c.status)
				if _, err := io.WriteString(w, oversized); err != nil {
					t.Errorf("receiver: write body: %v", err)
				}
			})
			w := testWorker(t, Config{})

			got := w.post(context.Background(), endpoint.url, []byte(`{"a":1}`))

			if len(got.snippet) != snippetBytes {
				t.Errorf("snippet is %d bytes, want the %d-byte cap", len(got.snippet), snippetBytes)
			}
			if got.success != c.wantSuccess {
				t.Errorf("%s: success = %v, want %v", c.name, got.success, c.wantSuccess)
			}
			if got.statusCode == nil || *got.statusCode != c.status {
				t.Errorf("%s: status code = %v, want %d", c.name, got.statusCode, c.status)
			}
		})
	}
}
