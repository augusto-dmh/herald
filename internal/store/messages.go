package store

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/augusto-dmh/herald/internal/herald"
)

const (
	messageColumns = `id, tenant_id, application_id, event_type, payload, created_at`
	// The same columns for a query that joins messages to deliveries,
	// where id, tenant_id and created_at exist on both tables.
	messageColumnsQualified = `m.id, m.tenant_id, m.application_id, m.event_type, ` +
		`m.payload, m.created_at`
)

// A payload is written and read as plain bytes, never as a JSON type:
// the column stores what the tenant sent and this layer must not be
// the thing that changes it. The document was checked for validity
// before it got here.

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
		m.ID, m.TenantID, m.ApplicationID, m.EventType, []byte(m.Payload)))
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

const (
	deliveryColumns = `id, tenant_id, message_id, endpoint_id, status, created_at, updated_at`
	// The same columns for a query that joins deliveries to the endpoint
	// and message they name.
	deliveryColumnsQualified = `d.id, d.tenant_id, d.message_id, d.endpoint_id, d.status, ` +
		`d.created_at, d.updated_at`
)

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

// DeliveryWork is everything one execution of a delivery needs: the
// delivery itself, the endpoint it is addressed to, and the message it
// carries.
type DeliveryWork struct {
	Delivery herald.Delivery
	Endpoint herald.Endpoint
	Message  herald.Message
}

// DeliveryWorkForJob reads a delivery and what it takes to run it, by
// delivery id alone.
//
// It is the one read in this package that takes no tenant, and it is
// unscoped by necessity rather than by convenience: a queue job carries
// a delivery id and nothing else, so the tenant is not something the
// worker could be asked to pass — it is something this read returns.
// The rest of the rule still holds, because every row the worker then
// writes is keyed to the tenant this read handed back, and nothing on
// this path is ever reached by a request. Anything the API can reach
// keeps its tenant-scoped variant.
//
// The state is read now rather than carried in the job, so a URL edited
// between enqueue and execution is the URL herald POSTs to.
func (s *Store) DeliveryWorkForJob(
	ctx context.Context, deliveryID uuid.UUID,
) (DeliveryWork, error) {
	out, err := scanDeliveryWork(s.pool.QueryRow(ctx, `
		SELECT `+deliveryColumnsQualified+`, `+endpointColumnsQualified+`, `+messageColumnsQualified+`
		FROM deliveries d
		JOIN endpoints e ON e.id = d.endpoint_id
		JOIN messages m ON m.id = d.message_id
		WHERE d.id = $1`, deliveryID))
	if err != nil {
		return DeliveryWork{}, wrap("read delivery work", err)
	}
	return out, nil
}

func scanDeliveryWork(row pgx.Row) (DeliveryWork, error) {
	var w DeliveryWork
	var payload []byte
	err := row.Scan(
		&w.Delivery.ID, &w.Delivery.TenantID, &w.Delivery.MessageID, &w.Delivery.EndpointID,
		&w.Delivery.Status, &w.Delivery.CreatedAt, &w.Delivery.UpdatedAt,
		&w.Endpoint.ID, &w.Endpoint.TenantID, &w.Endpoint.ApplicationID, &w.Endpoint.URL,
		&w.Endpoint.Description, &w.Endpoint.FilterTypes, &w.Endpoint.Disabled,
		&w.Endpoint.CreatedAt,
		&w.Message.ID, &w.Message.TenantID, &w.Message.ApplicationID, &w.Message.EventType,
		&payload, &w.Message.CreatedAt,
	)
	w.Message.Payload = payload
	return w, err
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
