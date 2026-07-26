package httpapi

import (
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/augusto-dmh/herald/internal/herald"
)

type createApplicationRequest struct {
	UID  string `json:"uid"`
	Name string `json:"name"`
}

type applicationResponse struct {
	ID        uuid.UUID `json:"id"`
	UID       string    `json:"uid"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// createApplication registers a stream of messages under the calling
// tenant. The uid is the caller's own name for it and is what appears
// in every later path.
func (s *Server) createApplication(w http.ResponseWriter, r *http.Request, c caller) error {
	var req createApplicationRequest
	if err := decodeJSON(r, &req); err != nil {
		return err
	}
	id, err := herald.NewID()
	if err != nil {
		return err
	}
	application := herald.Application{
		ID:       id,
		TenantID: c.tenantID,
		UID:      req.UID,
		Name:     strings.TrimSpace(req.Name),
	}
	if err := application.Validate(); err != nil {
		return err
	}

	created, err := s.store.CreateApplication(r.Context(), application)
	if err != nil {
		return err
	}
	s.writeJSON(w, http.StatusCreated, applicationResponse{
		ID:        created.ID,
		UID:       created.UID,
		Name:      created.Name,
		CreatedAt: created.CreatedAt,
	})
	return nil
}

type createEndpointRequest struct {
	URL         string   `json:"url"`
	Description string   `json:"description"`
	FilterTypes []string `json:"filter_types"`
}

type endpointResponse struct {
	ID          uuid.UUID `json:"id"`
	URL         string    `json:"url"`
	Description string    `json:"description"`
	FilterTypes []string  `json:"filter_types"`
	Disabled    bool      `json:"disabled"`
	CreatedAt   time.Time `json:"created_at"`
}

// createEndpoint registers a destination under one of the caller's
// applications.
//
// A request that lists no event types is stored as filtering on nothing
// at all, which is what makes the endpoint receive every type. The
// distinction matters at exactly this boundary: JSON can express an
// empty list, the schema cannot store one, and a subscription to
// nothing is never what a caller meant by sending `[]`.
func (s *Server) createEndpoint(w http.ResponseWriter, r *http.Request, c caller) error {
	ctx := r.Context()

	var req createEndpointRequest
	if err := decodeJSON(r, &req); err != nil {
		return err
	}
	application, err := s.store.ApplicationByUID(ctx, c.tenantID, r.PathValue("uid"))
	if err != nil {
		return err
	}
	id, err := herald.NewID()
	if err != nil {
		return err
	}
	var filters []string
	if len(req.FilterTypes) > 0 {
		filters = req.FilterTypes
	}
	endpoint := herald.Endpoint{
		ID:            id,
		TenantID:      c.tenantID,
		ApplicationID: application.ID,
		URL:           strings.TrimSpace(req.URL),
		Description:   req.Description,
		FilterTypes:   filters,
	}
	if err := endpoint.Validate(); err != nil {
		return err
	}

	created, err := s.store.CreateEndpoint(ctx, endpoint)
	if err != nil {
		return err
	}
	s.writeJSON(w, http.StatusCreated, endpointResponse{
		ID:          created.ID,
		URL:         created.URL,
		Description: created.Description,
		FilterTypes: created.FilterTypes,
		Disabled:    created.Disabled,
		CreatedAt:   created.CreatedAt,
	})
	return nil
}
