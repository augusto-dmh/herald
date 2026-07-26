package store

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/augusto-dmh/herald/internal/herald"
)

const messageColumns = `id, tenant_id, application_id, event_type, payload, created_at`

func scanMessage(row pgx.Row) (herald.Message, error) {
	var m herald.Message
	var payload []byte
	err := row.Scan(&m.ID, &m.TenantID, &m.ApplicationID, &m.EventType, &payload, &m.CreatedAt)
	m.Payload = payload
	return m, err
}

// CreateMessageTx inserts an accepted message in the caller's
// transaction. There is deliberately no variant that opens its own: a
// message is only ever written together with the deliveries and queue
// jobs that carry it onward, so an accepted message can never exist
// with nothing scheduled to deliver it.
func (s *Store) CreateMessageTx(
	ctx context.Context, tx pgx.Tx, m herald.Message,
) (herald.Message, error) {
	out, err := scanMessage(tx.QueryRow(ctx, `
		INSERT INTO messages (id, tenant_id, application_id, event_type, payload)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING `+messageColumns,
		m.ID, m.TenantID, m.ApplicationID, m.EventType, m.Payload))
	if err != nil {
		return herald.Message{}, wrap("create message", err)
	}
	return out, nil
}

// MessageByID returns one message of a tenant's application. A message
// of another tenant, or of another application, is reported as missing.
func (s *Store) MessageByID(
	ctx context.Context, tenantID, applicationID, messageID uuid.UUID,
) (herald.Message, error) {
	out, err := scanMessage(s.pool.QueryRow(ctx, `
		SELECT `+messageColumns+`
		FROM messages
		WHERE tenant_id = $1 AND application_id = $2 AND id = $3`,
		tenantID, applicationID, messageID))
	if err != nil {
		return herald.Message{}, wrap("read message", err)
	}
	return out, nil
}

const deliveryColumns = `id, tenant_id, message_id, endpoint_id, status, created_at, updated_at`

func scanDelivery(row pgx.Row) (herald.Delivery, error) {
	var d herald.Delivery
	err := row.Scan(&d.ID, &d.TenantID, &d.MessageID, &d.EndpointID, &d.Status,
		&d.CreatedAt, &d.UpdatedAt)
	return d, err
}

// CreateDeliveryTx inserts one delivery in the caller's transaction,
// alongside its message and its queue job.
func (s *Store) CreateDeliveryTx(
	ctx context.Context, tx pgx.Tx, d herald.Delivery,
) (herald.Delivery, error) {
	status := d.Status
	if status == "" {
		status = herald.DeliveryPending
	}
	out, err := scanDelivery(tx.QueryRow(ctx, `
		INSERT INTO deliveries (id, tenant_id, message_id, endpoint_id, status)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING `+deliveryColumns,
		d.ID, d.TenantID, d.MessageID, d.EndpointID, status))
	if err != nil {
		return herald.Delivery{}, wrap("create delivery", err)
	}
	return out, nil
}

// DeliveriesByMessage returns a message's deliveries, oldest first.
func (s *Store) DeliveriesByMessage(
	ctx context.Context, tenantID, messageID uuid.UUID,
) ([]herald.Delivery, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+deliveryColumns+`
		FROM deliveries
		WHERE tenant_id = $1 AND message_id = $2
		ORDER BY id`, tenantID, messageID)
	if err != nil {
		return nil, wrap("read deliveries", err)
	}
	defer rows.Close()

	var deliveries []herald.Delivery
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, wrap("read deliveries", err)
		}
		deliveries = append(deliveries, d)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("read deliveries", err)
	}
	return deliveries, nil
}

// UpdateDeliveryStatus moves a delivery to its outcome. The delivery is
// identified by id alone because the worker that calls this learned the
// id from its job, not from a tenant-scoped request.
func (s *Store) UpdateDeliveryStatus(
	ctx context.Context, deliveryID uuid.UUID, status herald.DeliveryStatus,
) error {
	if !status.Valid() {
		return wrap("update delivery status", &herald.ValidationError{
			Field: "status", Message: "unknown delivery status",
		})
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE deliveries SET status = $2, updated_at = now() WHERE id = $1`,
		deliveryID, status)
	if err != nil {
		return wrap("update delivery status", err)
	}
	if tag.RowsAffected() == 0 {
		return wrap("update delivery status", pgx.ErrNoRows)
	}
	return nil
}

const (
	attemptColumns = `id, tenant_id, delivery_id, attempt_number, status_code, success, ` +
		`error, response_snippet, duration_ms, attempted_at`
	// The same columns for a query that joins deliveries, where several
	// of these names exist on both tables.
	attemptColumnsQualified = `a.id, a.tenant_id, a.delivery_id, a.attempt_number, ` +
		`a.status_code, a.success, a.error, a.response_snippet, a.duration_ms, a.attempted_at`
)

func scanAttempt(row pgx.Row) (herald.DeliveryAttempt, error) {
	var a herald.DeliveryAttempt
	var durationMS int
	err := row.Scan(&a.ID, &a.TenantID, &a.DeliveryID, &a.AttemptNumber, &a.StatusCode,
		&a.Success, &a.Error, &a.ResponseSnippet, &durationMS, &a.AttemptedAt)
	a.Duration = time.Duration(durationMS) * time.Millisecond
	return a, err
}

// CreateAttempt appends what one execution of a delivery did. Attempts
// are never updated and never replaced: a re-run of the same attempt
// number is a second row.
func (s *Store) CreateAttempt(
	ctx context.Context, a herald.DeliveryAttempt,
) (herald.DeliveryAttempt, error) {
	out, err := scanAttempt(s.pool.QueryRow(ctx, `
		INSERT INTO delivery_attempts
			(id, tenant_id, delivery_id, attempt_number, status_code, success,
			 error, response_snippet, duration_ms)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING `+attemptColumns,
		a.ID, a.TenantID, a.DeliveryID, a.AttemptNumber, a.StatusCode, a.Success,
		a.Error, a.ResponseSnippet, a.Duration.Milliseconds()))
	if err != nil {
		return herald.DeliveryAttempt{}, wrap("create delivery attempt", err)
	}
	return out, nil
}

// AttemptsByMessage returns every attempt made for a message, across
// all of its deliveries, in the order they happened.
func (s *Store) AttemptsByMessage(
	ctx context.Context, tenantID, messageID uuid.UUID,
) ([]herald.DeliveryAttempt, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+attemptColumnsQualified+`
		FROM delivery_attempts a
		JOIN deliveries d ON d.id = a.delivery_id
		WHERE a.tenant_id = $1 AND d.message_id = $2
		ORDER BY a.attempted_at, a.id`, tenantID, messageID)
	if err != nil {
		return nil, wrap("read delivery attempts", err)
	}
	defer rows.Close()

	var attempts []herald.DeliveryAttempt
	for rows.Next() {
		a, err := scanAttempt(rows)
		if err != nil {
			return nil, wrap("read delivery attempts", err)
		}
		attempts = append(attempts, a)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("read delivery attempts", err)
	}
	return attempts, nil
}
