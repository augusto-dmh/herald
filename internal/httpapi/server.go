// Package httpapi is herald's HTTP surface: the routes an application
// calls, the credential check in front of them, and the JSON shapes
// they speak.
//
// Two rules run through it. A request is answered out of the tenant its
// key resolves to and out of nowhere else, so a caller naming another
// tenant's resource is told the resource does not exist rather than
// that it may not have it — the difference between those two answers is
// itself information. And every response body, errors included, is
// JSON, so a client never has to guess whether it received a document
// or a page of prose.
package httpapi

import (
	"log/slog"
	"net/http"

	"github.com/augusto-dmh/drover"

	"github.com/augusto-dmh/herald/internal/herald"
	"github.com/augusto-dmh/herald/internal/store"
)

// maxBodyBytes is the largest request body herald will read. It is
// enforced on every route rather than on ingest alone: no request this
// API accepts has any business being larger, and a cap that applies
// everywhere cannot be forgotten on the next route added.
const maxBodyBytes = 1 << 20

// Config carries what the server needs beyond its dependencies.
type Config struct {
	// BootstrapToken authorizes tenant creation, the one operation with
	// no tenant to authenticate as. Leaving it empty closes tenant
	// creation entirely rather than opening it to everyone.
	BootstrapToken string

	// Logger receives the detail of failures whose cause is never sent
	// to the caller. Defaults to slog.Default().
	Logger *slog.Logger
}

// Server answers herald's HTTP requests.
type Server struct {
	store          *store.Store
	queue          *drover.Client
	bootstrapToken string
	log            *slog.Logger
	handler        http.Handler
}

// NewServer returns the API as a handler, reading and writing through
// the given store and enqueueing delivery work on the given queue.
func NewServer(s *store.Store, queue *drover.Client, cfg Config) http.Handler {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	srv := &Server{
		store:          s,
		queue:          queue,
		bootstrapToken: cfg.BootstrapToken,
		log:            log,
	}
	srv.handler = capBody(srv.routes())
	return srv
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

// routes is the whole surface. Anything it does not name is reported
// as missing in the same JSON shape as every other error.
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.Handle("POST /v1/tenants", s.requireBootstrapToken(s.createTenant))
	mux.Handle("POST /v1/applications", s.requireKey(herald.ScopeFull, s.createApplication))
	mux.Handle("POST /v1/applications/{uid}/endpoints", s.requireKey(herald.ScopeFull, s.createEndpoint))
	mux.Handle("GET /v1/applications/{uid}/messages/{id}", s.requireKey(herald.ScopeFull, s.showMessage))

	mux.Handle("/", s.handle(func(http.ResponseWriter, *http.Request) error {
		return errNotFound
	}))
	return mux
}

// capBody bounds what a handler can be made to read. The bound is
// applied by replacing the body rather than by consulting
// Content-Length, which a client controls and may omit, so an oversized
// body fails when it is read and is reported as such.
func capBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		}
		next.ServeHTTP(w, r)
	})
}
