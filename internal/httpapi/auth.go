package httpapi

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/augusto-dmh/herald/internal/herald"
	"github.com/augusto-dmh/herald/internal/store"
)

var (
	errUnauthorized = &apiError{
		status:  http.StatusUnauthorized,
		code:    "unauthorized",
		message: "a valid credential is required",
	}
	errForbidden = &apiError{
		status:  http.StatusForbidden,
		code:    "forbidden",
		message: "this key's scope does not permit this operation",
	}
)

// caller is who a request turned out to be: the tenant its key belongs
// to and the authority that key carries. It is handed to a handler as
// an argument rather than hidden in the request context, so a handler
// cannot be written that forgets to scope its work to a tenant — there
// is no way to compile one that never received it.
type caller struct {
	tenantID uuid.UUID
	scope    herald.Scope
}

// authedHandler is a handler that runs only once a request has been
// attributed to a tenant.
type authedHandler func(http.ResponseWriter, *http.Request, caller) error

// requireKey guards a route with an API key of at least the given
// scope. A key herald does not know and a request carrying none are
// answered identically: an attacker learns nothing about which of their
// guesses was a real key.
func (s *Server) requireKey(want herald.Scope, h authedHandler) http.Handler {
	return s.handle(func(w http.ResponseWriter, r *http.Request) error {
		key, err := s.authenticate(r)
		if err != nil {
			return err
		}
		if !key.Scope.Permits(want) {
			return errForbidden
		}
		return h(w, r, caller{tenantID: key.TenantID, scope: key.Scope})
	})
}

// authenticate resolves the credential a request presents to the key it
// stands for. The plaintext is hashed and the hash is what is looked
// up, so herald never has to hold a key it could leak.
func (s *Server) authenticate(r *http.Request) (herald.APIKey, error) {
	presented, ok := bearerToken(r)
	if !ok {
		return herald.APIKey{}, errUnauthorized
	}
	key, err := s.store.APIKeyByHash(r.Context(), herald.HashAPIKey(presented))
	if errors.Is(err, store.ErrNotFound) {
		return herald.APIKey{}, errUnauthorized
	}
	if err != nil {
		return herald.APIKey{}, err
	}
	s.recordKeyUse(r, key)
	return key, nil
}

// apiKeyTouchInterval is how stale a key's recorded last use may become
// before authentication refreshes it. What the timestamp answers is
// "has anything used this credential lately?", which an operator asks
// before revoking a key — a question minutes are precise enough for.
// Recording every request instead would turn one busy key into an
// endless stream of updates to a single row, which is a contention and
// bloat source paid for nothing (ADR-0002).
const apiKeyTouchInterval = time.Minute

// recordKeyUse notes that a credential was used, at most once per
// apiKeyTouchInterval per key.
//
// It is deliberately best-effort. The caller has already been
// authenticated by the time this runs, and an audit timestamp that
// could not be written is not a reason to refuse a request that is
// otherwise entitled to be served: failing here would turn a
// bookkeeping problem into an outage.
func (s *Server) recordKeyUse(r *http.Request, key herald.APIKey) {
	now := time.Now()
	if key.LastUsedAt != nil && now.Sub(*key.LastUsedAt) < apiKeyTouchInterval {
		return
	}
	if err := s.store.TouchAPIKey(r.Context(), key.ID, now); err != nil {
		s.log.Warn("record api key use", "api_key_id", key.ID, "error", err)
	}
}

// requireBootstrapToken guards tenant creation, the one operation with
// no tenant to authenticate as. An unconfigured token closes the route
// rather than opening it: a deployment that forgot to set one is not a
// deployment where anybody may mint tenants.
func (s *Server) requireBootstrapToken(h handlerFunc) http.Handler {
	return s.handle(func(w http.ResponseWriter, r *http.Request) error {
		presented, ok := bearerToken(r)
		if !ok || s.bootstrapToken == "" {
			return errUnauthorized
		}
		// Both sides are hashed before they are compared so the
		// comparison runs over two equal-length values: a constant-time
		// compare of the raw strings would still betray their lengths.
		if !herald.EqualHash(herald.HashAPIKey(presented), herald.HashAPIKey(s.bootstrapToken)) {
			return errUnauthorized
		}
		return h(w, r)
	})
}

// bearerToken returns the credential in the Authorization header.
func bearerToken(r *http.Request) (string, bool) {
	const scheme = "bearer "
	header := r.Header.Get("Authorization")
	if len(header) <= len(scheme) || !strings.EqualFold(header[:len(scheme)], scheme) {
		return "", false
	}
	token := strings.TrimSpace(header[len(scheme):])
	if token == "" {
		return "", false
	}
	return token, true
}
