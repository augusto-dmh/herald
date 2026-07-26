package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/augusto-dmh/herald/internal/herald"
	"github.com/augusto-dmh/herald/internal/store"
)

// handlerFunc is a handler that may fail. Returning the failure instead
// of writing it keeps the decision of what a caller is told in one
// place, which is the only way the rule that a missing row and another
// tenant's row look identical can be enforced rather than remembered.
type handlerFunc func(http.ResponseWriter, *http.Request) error

// handle turns a failable handler into an http.Handler.
func (s *Server) handle(h handlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := h(w, r); err != nil {
			s.writeError(w, r, err)
		}
	})
}

// apiError is a failure with a caller-facing status and message. Its
// message is written to the response, so it never carries anything the
// caller did not already know.
type apiError struct {
	status  int
	code    string
	message string
	field   string
}

func (e *apiError) Error() string { return e.code + ": " + e.message }

var (
	// errNotFound answers both a resource that does not exist and one
	// that belongs to another tenant. Telling them apart would let a
	// caller enumerate what other tenants own.
	errNotFound = &apiError{
		status:  http.StatusNotFound,
		code:    "not_found",
		message: "no such resource",
	}
	errConflict = &apiError{
		status:  http.StatusConflict,
		code:    "conflict",
		message: "a resource with that identifier already exists",
	}
	errTooLarge = &apiError{
		status:  http.StatusRequestEntityTooLarge,
		code:    "payload_too_large",
		message: "the request body exceeds 1 MiB",
	}
	errMalformedJSON = &apiError{
		status:  http.StatusUnprocessableEntity,
		code:    "invalid_request",
		message: "the request body must be a JSON object",
	}
	errInternal = &apiError{
		status:  http.StatusInternalServerError,
		code:    "internal_error",
		message: "the request could not be completed",
	}
)

// errorEnvelope is the single shape of every failure this API reports.
type errorEnvelope struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	// Field names the part of the request to fix, when the failure is
	// about one part of it.
	Field string `json:"field,omitempty"`
}

// writeError answers a failed request. A failure it does not recognize
// is a bug or an outage: the caller is told only that the request did
// not complete, and the detail goes to the log.
func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error) {
	apiErr := asAPIError(err)
	if apiErr == nil {
		s.log.Error("request failed",
			"method", r.Method, "path", r.URL.Path, "error", err)
		apiErr = errInternal
	}
	s.writeJSON(w, apiErr.status, errorEnvelope{Error: errorDetail{
		Code:    apiErr.code,
		Message: apiErr.message,
		Field:   apiErr.field,
	}})
}

// asAPIError translates the failures the layers below raise into what
// the caller is told, and returns nil for anything else.
func asAPIError(err error) *apiError {
	var apiErr *apiError
	if errors.As(err, &apiErr) {
		return apiErr
	}
	var invalid *herald.ValidationError
	if errors.As(err, &invalid) {
		return &apiError{
			status:  http.StatusUnprocessableEntity,
			code:    "invalid_request",
			message: invalid.Message,
			field:   invalid.Field,
		}
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		return errNotFound
	case errors.Is(err, store.ErrConflict):
		return errConflict
	}
	return nil
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		s.log.Error("write response body", "error", err)
	}
}

// decodeJSON reads a request body into dst. A body that ran past the
// cap is reported as too large rather than as malformed: the caller's
// JSON may have been perfectly good, there was just too much of it.
func decodeJSON(r *http.Request, dst any) error {
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return errTooLarge
		}
		return errMalformedJSON
	}
	return nil
}
