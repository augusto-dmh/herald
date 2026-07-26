package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/augusto-dmh/herald/internal/herald"
)

// newServer returns a server whose database is unreachable on purpose.
// Every request in this file must be answered before anything is read
// or written, so a test that reaches the store fails loudly instead of
// passing for the wrong reason.
func newServer(t *testing.T, cfg Config) *Server {
	t.Helper()
	cfg.Logger = slog.New(slog.DiscardHandler)
	srv, ok := NewServer(nil, nil, cfg).(*Server)
	if !ok {
		t.Fatalf("the API is not served by *Server")
	}
	return srv
}

// send runs one request against a handler and returns the recorded
// response together with the decoded error envelope, which is empty for
// a response that carried none.
func send(t *testing.T, h http.Handler, r *http.Request) (*httptest.ResponseRecorder, errorDetail) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)

	var envelope errorEnvelope
	if body := rec.Body.Bytes(); len(body) > 0 {
		if err := json.Unmarshal(body, &envelope); err != nil {
			t.Fatalf("response body is not JSON: %v (%s)", err, body)
		}
	}
	return rec, envelope.Error
}

func TestARouteTheAPIDoesNotServeIsReportedAsMissingInJSON(t *testing.T) {
	srv := newServer(t, Config{})

	rec, detail := send(t, srv, httptest.NewRequest(http.MethodGet, "/v1/nothing-here", nil))

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("content type = %q, want application/json", got)
	}
	if detail.Code != "not_found" {
		t.Errorf("error code = %q, want not_found", detail.Code)
	}
}

func TestABodyLargerThanTheCapIsRejectedWithoutBeingProcessed(t *testing.T) {
	srv := newServer(t, Config{})

	if maxBodyBytes != 1<<20 {
		t.Errorf("body cap = %d, want the documented 1 MiB", maxBodyBytes)
	}

	decoded := false
	handler := capBody(srv.handle(func(_ http.ResponseWriter, r *http.Request) error {
		var body map[string]string
		if err := decodeJSON(r, &body); err != nil {
			return err
		}
		decoded = true
		return nil
	}))

	oversized := `{"note":"` + strings.Repeat("x", maxBodyBytes) + `"}`
	rec, detail := send(t, handler,
		httptest.NewRequest(http.MethodPost, "/v1/anything", strings.NewReader(oversized)))

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
	}
	if detail.Code != "payload_too_large" {
		t.Errorf("error code = %q, want payload_too_large", detail.Code)
	}
	if decoded {
		t.Errorf("the handler was handed a body larger than the cap")
	}
}

func TestABodyWithinTheCapReachesTheHandler(t *testing.T) {
	srv := newServer(t, Config{})

	var got map[string]string
	handler := capBody(srv.handle(func(_ http.ResponseWriter, r *http.Request) error {
		return decodeJSON(r, &got)
	}))

	sized := `{"note":"` + strings.Repeat("x", maxBodyBytes-64) + `"}`
	rec, _ := send(t, handler,
		httptest.NewRequest(http.MethodPost, "/v1/anything", strings.NewReader(sized)))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if len(got["note"]) != maxBodyBytes-64 {
		t.Errorf("the handler read %d bytes of the body, want the whole of it", len(got["note"]))
	}
}

func TestABodyThatIsNotJSONIsRejectedAsInvalid(t *testing.T) {
	srv := newServer(t, Config{})
	handler := srv.handle(func(_ http.ResponseWriter, r *http.Request) error {
		var body map[string]string
		return decodeJSON(r, &body)
	})

	rec, detail := send(t, handler,
		httptest.NewRequest(http.MethodPost, "/v1/anything", strings.NewReader(`{"note":`)))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnprocessableEntity)
	}
	if detail.Code != "invalid_request" {
		t.Errorf("error code = %q, want invalid_request", detail.Code)
	}
}

func TestARejectedInputNamesTheFieldToFix(t *testing.T) {
	srv := newServer(t, Config{})
	handler := srv.handle(func(http.ResponseWriter, *http.Request) error {
		return herald.Endpoint{URL: "not-a-url"}.Validate()
	})

	rec, detail := send(t, handler, httptest.NewRequest(http.MethodPost, "/v1/anything", nil))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnprocessableEntity)
	}
	if detail.Field != "url" {
		t.Errorf("error field = %q, want url", detail.Field)
	}
	if detail.Message == "" {
		t.Errorf("a rejected input was reported with no explanation")
	}
}

func TestAnUnexpectedFailureTellsTheCallerNothingAboutItsCause(t *testing.T) {
	srv := newServer(t, Config{})
	cause := "dial tcp 10.0.0.7:5432: connect: hunter2 is not the password"
	handler := srv.handle(func(http.ResponseWriter, *http.Request) error {
		return errors.New(cause)
	})

	rec, detail := send(t, handler, httptest.NewRequest(http.MethodGet, "/v1/anything", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	if detail.Code != "internal_error" {
		t.Errorf("error code = %q, want internal_error", detail.Code)
	}
	if strings.Contains(rec.Body.String(), "hunter2") || strings.Contains(rec.Body.String(), "5432") {
		t.Errorf("the response leaked the internal failure: %s", rec.Body)
	}
}
