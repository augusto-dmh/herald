package httpapi

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/augusto-dmh/herald/internal/deliver"
	"github.com/augusto-dmh/herald/internal/herald"
)

type ingestRequest struct {
	EventType string          `json:"event_type"`
	Payload   json.RawMessage `json:"payload"`
}

type acceptedMessageResponse struct {
	ID        uuid.UUID `json:"id"`
	EventType string    `json:"event_type"`
	CreatedAt time.Time `json:"created_at"`
}

// ingestMessage accepts a message and schedules its delivery.
//
// The message, one delivery per endpoint that wants the event type, and
// the queue job behind each delivery are all written through a single
// transaction. That is the whole point of the route: a message herald
// has acknowledged is one that something is already scheduled to
// deliver, and an ingest that fails part-way leaves no trace rather than
// an accepted message nothing will ever act on.
func (s *Server) ingestMessage(w http.ResponseWriter, r *http.Request, c caller) error {
	ctx := r.Context()

	var req ingestRequest
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
	message := herald.Message{
		ID:            id,
		TenantID:      c.tenantID,
		ApplicationID: application.ID,
		EventType:     req.EventType,
		Payload:       req.Payload,
	}
	if err := message.Validate(); err != nil {
		return err
	}

	tx, err := s.store.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	message, err = s.store.CreateMessageTx(ctx, tx, message)
	if err != nil {
		return err
	}
	// The fan-out is decided inside the transaction, so the endpoints
	// the deliveries are written for are the ones that existed on the
	// same snapshot the message was written to.
	endpoints, err := s.store.EndpointsForEventTx(ctx, tx, c.tenantID, application.ID, message.EventType)
	if err != nil {
		return err
	}
	for _, endpoint := range endpoints {
		deliveryID, err := herald.NewID()
		if err != nil {
			return err
		}
		delivery, err := s.store.CreateDeliveryTx(ctx, tx, herald.Delivery{
			ID:         deliveryID,
			TenantID:   c.tenantID,
			MessageID:  message.ID,
			EndpointID: endpoint.ID,
		})
		if err != nil {
			return err
		}
		if _, err := s.queue.InsertTx(ctx, tx, deliver.Args{DeliveryID: delivery.ID}); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}

	s.writeJSON(w, http.StatusAccepted, acceptedMessageResponse{
		ID:        message.ID,
		EventType: message.EventType,
		CreatedAt: message.CreatedAt,
	})
	return nil
}
