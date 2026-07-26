package httpapi

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/augusto-dmh/herald/internal/herald"
)

type attemptResponse struct {
	ID              uuid.UUID `json:"id"`
	AttemptNumber   int       `json:"attempt_number"`
	StatusCode      *int      `json:"status_code"`
	Success         bool      `json:"success"`
	Error           string    `json:"error"`
	ResponseSnippet string    `json:"response_snippet"`
	DurationMS      int64     `json:"duration_ms"`
	AttemptedAt     time.Time `json:"attempted_at"`
}

type deliveryResponse struct {
	ID         uuid.UUID             `json:"id"`
	EndpointID uuid.UUID             `json:"endpoint_id"`
	Status     herald.DeliveryStatus `json:"status"`
	CreatedAt  time.Time             `json:"created_at"`
	UpdatedAt  time.Time             `json:"updated_at"`
	Attempts   []attemptResponse     `json:"attempts"`
}

type messageResponse struct {
	ID         uuid.UUID          `json:"id"`
	EventType  string             `json:"event_type"`
	Payload    json.RawMessage    `json:"payload"`
	CreatedAt  time.Time          `json:"created_at"`
	Deliveries []deliveryResponse `json:"deliveries"`
}

// showMessage reports what became of one message: every delivery it was
// fanned out to and every attempt made at each. The reads are the
// caller's tenant's, so a message id belonging to anyone else is
// answered as missing whether or not it exists.
func (s *Server) showMessage(w http.ResponseWriter, r *http.Request, c caller) error {
	ctx := r.Context()

	messageID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		// An id that is not an identifier names nothing, which is the
		// same answer as an id that names another tenant's message.
		return errNotFound
	}
	application, err := s.store.ApplicationByUID(ctx, c.tenantID, r.PathValue("uid"))
	if err != nil {
		return err
	}
	message, err := s.store.MessageByID(ctx, c.tenantID, application.ID, messageID)
	if err != nil {
		return err
	}
	deliveries, err := s.store.DeliveriesByMessage(ctx, c.tenantID, message.ID)
	if err != nil {
		return err
	}
	attempts, err := s.store.AttemptsByMessage(ctx, c.tenantID, message.ID)
	if err != nil {
		return err
	}

	byDelivery := make(map[uuid.UUID][]attemptResponse, len(deliveries))
	for _, a := range attempts {
		byDelivery[a.DeliveryID] = append(byDelivery[a.DeliveryID], attemptResponse{
			ID:              a.ID,
			AttemptNumber:   a.AttemptNumber,
			StatusCode:      a.StatusCode,
			Success:         a.Success,
			Error:           a.Error,
			ResponseSnippet: a.ResponseSnippet,
			DurationMS:      a.Duration.Milliseconds(),
			AttemptedAt:     a.AttemptedAt,
		})
	}

	body := messageResponse{
		ID:         message.ID,
		EventType:  message.EventType,
		Payload:    message.Payload,
		CreatedAt:  message.CreatedAt,
		Deliveries: make([]deliveryResponse, 0, len(deliveries)),
	}
	for _, d := range deliveries {
		made := byDelivery[d.ID]
		if made == nil {
			made = []attemptResponse{}
		}
		body.Deliveries = append(body.Deliveries, deliveryResponse{
			ID:         d.ID,
			EndpointID: d.EndpointID,
			Status:     d.Status,
			CreatedAt:  d.CreatedAt,
			UpdatedAt:  d.UpdatedAt,
			Attempts:   made,
		})
	}
	s.writeJSON(w, http.StatusOK, body)
	return nil
}
