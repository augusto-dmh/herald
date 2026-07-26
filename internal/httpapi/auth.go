package httpapi

import (
	"errors"
	"net/http"
	"strings"

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
	return key, nil
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
