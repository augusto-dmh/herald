package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/augusto-dmh/herald/internal/herald"
)

// A credential herald cannot even parse is refused before the store is
// consulted: the server under test has no database, so a lookup would
// crash rather than quietly return nothing.
func TestARequestWithNoUsableCredentialIsRefusedBeforeAnyLookup(t *testing.T) {
	srv := newServer(t, Config{BootstrapToken: "operator-token"})

	reached := false
	keyed := srv.requireKey(herald.ScopeIngest, func(http.ResponseWriter, *http.Request, caller) error {
		reached = true
		return nil
	})
	bootstrapped := srv.requireBootstrapToken(func(http.ResponseWriter, *http.Request) error {
		reached = true
		return nil
	})

	headers := []struct {
		name  string
		value string
	}{
		{"no header at all", ""},
		{"another scheme", "Basic aGk6dGhlcmU="},
		{"the scheme alone", "Bearer"},
		{"an empty credential", "Bearer "},
		{"a key-shaped word without a key", "hrld_live_abcdefgh"},
	}
	for _, guarded := range []struct {
		route   string
		handler http.Handler
	}{{"a key-guarded route", keyed}, {"the bootstrap route", bootstrapped}} {
		for _, header := range headers {
			t.Run(guarded.route+" with "+header.name, func(t *testing.T) {
				reached = false
				r := httptest.NewRequest(http.MethodPost, "/v1/anything", nil)
				if header.value != "" {
					r.Header.Set("Authorization", header.value)
				}

				rec, detail := send(t, guarded.handler, r)

				if rec.Code != http.StatusUnauthorized {
					t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
				}
				if detail.Code != "unauthorized" {
					t.Errorf("error code = %q, want unauthorized", detail.Code)
				}
				if reached {
					t.Errorf("the guarded handler ran for an unauthenticated request")
				}
			})
		}
	}
}

func TestTheWrongBootstrapTokenIsRefused(t *testing.T) {
	srv := newServer(t, Config{BootstrapToken: "operator-token"})

	reached := false
	handler := srv.requireBootstrapToken(func(http.ResponseWriter, *http.Request) error {
		reached = true
		return nil
	})

	r := httptest.NewRequest(http.MethodPost, "/v1/tenants", nil)
	r.Header.Set("Authorization", "Bearer operator-toke")

	rec, _ := send(t, handler, r)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	if reached {
		t.Errorf("a near-miss token was accepted")
	}
}

// A deployment with no bootstrap token configured is one where nobody
// can bootstrap, rather than one where anybody can.
func TestWithNoBootstrapTokenConfiguredNoTokenOpensTheRoute(t *testing.T) {
	srv := newServer(t, Config{})

	reached := false
	handler := srv.requireBootstrapToken(func(http.ResponseWriter, *http.Request) error {
		reached = true
		return nil
	})

	for _, presented := range []string{"", "anything", "hrld_live_whatever"} {
		r := httptest.NewRequest(http.MethodPost, "/v1/tenants", nil)
		r.Header.Set("Authorization", "Bearer "+presented)

		rec, _ := send(t, handler, r)

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("token %q got status %d, want %d", presented, rec.Code, http.StatusUnauthorized)
		}
	}
	if reached {
		t.Errorf("tenant creation ran with no bootstrap token configured")
	}
}

func TestTheCorrectBootstrapTokenOpensTheRoute(t *testing.T) {
	srv := newServer(t, Config{BootstrapToken: "operator-token"})

	reached := false
	handler := srv.requireBootstrapToken(func(w http.ResponseWriter, _ *http.Request) error {
		reached = true
		w.WriteHeader(http.StatusCreated)
		return nil
	})

	r := httptest.NewRequest(http.MethodPost, "/v1/tenants", nil)
	r.Header.Set("Authorization", "Bearer operator-token")

	rec, _ := send(t, handler, r)

	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusCreated)
	}
	if !reached {
		t.Errorf("the correct bootstrap token did not reach the handler")
	}
}
