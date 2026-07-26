package store

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/augusto-dmh/herald/internal/herald"
)

const applicationColumns = `id, tenant_id, uid, name, created_at`

func scanApplication(row pgx.Row) (herald.Application, error) {
	var a herald.Application
	err := row.Scan(&a.ID, &a.TenantID, &a.UID, &a.Name, &a.CreatedAt)
	return a, err
}

// CreateApplication inserts an application. A uid already taken in the
// same tenant is a conflict; the same uid in another tenant is not.
func (s *Store) CreateApplication(ctx context.Context, a herald.Application) (herald.Application, error) {
	out, err := scanApplication(s.pool.QueryRow(ctx, `
		INSERT INTO applications (id, tenant_id, uid, name)
		VALUES ($1, $2, $3, $4)
		RETURNING `+applicationColumns,
		a.ID, a.TenantID, a.UID, a.Name))
	if err != nil {
		return herald.Application{}, wrap("create application", err)
	}
	return out, nil
}

// ApplicationByUID resolves the uid in a request path to an
// application of the given tenant. Another tenant's application is
// reported as missing, not as forbidden, so the API cannot be used to
// discover which uids exist elsewhere.
func (s *Store) ApplicationByUID(
	ctx context.Context, tenantID uuid.UUID, uid string,
) (herald.Application, error) {
	out, err := scanApplication(s.pool.QueryRow(ctx, `
		SELECT `+applicationColumns+`
		FROM applications
		WHERE tenant_id = $1 AND uid = $2`, tenantID, uid))
	if err != nil {
		return herald.Application{}, wrap("read application", err)
	}
	return out, nil
}

const endpointColumns = `id, tenant_id, application_id, url, description, filter_types, disabled, created_at`

func scanEndpoint(row pgx.Row) (herald.Endpoint, error) {
	var e herald.Endpoint
	err := row.Scan(&e.ID, &e.TenantID, &e.ApplicationID, &e.URL, &e.Description,
		&e.FilterTypes, &e.Disabled, &e.CreatedAt)
	return e, err
}

// CreateEndpoint registers a destination under an application. An
// endpoint with no filter types is stored with none at all rather than
// with an empty list, which is what makes it receive every event type.
func (s *Store) CreateEndpoint(ctx context.Context, e herald.Endpoint) (herald.Endpoint, error) {
	var filters []string
	if len(e.FilterTypes) > 0 {
		filters = e.FilterTypes
	}
	out, err := scanEndpoint(s.pool.QueryRow(ctx, `
		INSERT INTO endpoints (id, tenant_id, application_id, url, description, filter_types, disabled)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING `+endpointColumns,
		e.ID, e.TenantID, e.ApplicationID, e.URL, e.Description, filters, e.Disabled))
	if err != nil {
		return herald.Endpoint{}, wrap("create endpoint", err)
	}
	return out, nil
}

// EndpointByID returns one endpoint of a tenant.
func (s *Store) EndpointByID(
	ctx context.Context, tenantID, endpointID uuid.UUID,
) (herald.Endpoint, error) {
	out, err := scanEndpoint(s.pool.QueryRow(ctx, `
		SELECT `+endpointColumns+`
		FROM endpoints
		WHERE tenant_id = $1 AND id = $2`, tenantID, endpointID))
	if err != nil {
		return herald.Endpoint{}, wrap("read endpoint", err)
	}
	return out, nil
}

// EndpointsForEvent returns the endpoints an event of the given type
// must be delivered to: the application's enabled endpoints that either
// filter for this event type or filter for nothing at all.
func (s *Store) EndpointsForEvent(
	ctx context.Context, tenantID, applicationID uuid.UUID, eventType string,
) ([]herald.Endpoint, error) {
	return endpointsForEvent(ctx, s.pool, tenantID, applicationID, eventType)
}

// EndpointsForEventTx is EndpointsForEvent inside the caller's
// transaction, so the fan-out is decided on the same snapshot that the
// resulting deliveries are written to.
func (s *Store) EndpointsForEventTx(
	ctx context.Context, tx pgx.Tx, tenantID, applicationID uuid.UUID, eventType string,
) ([]herald.Endpoint, error) {
	return endpointsForEvent(ctx, tx, tenantID, applicationID, eventType)
}

func endpointsForEvent(
	ctx context.Context, q querier, tenantID, applicationID uuid.UUID, eventType string,
) ([]herald.Endpoint, error) {
	rows, err := q.Query(ctx, `
		SELECT `+endpointColumns+`
		FROM endpoints
		WHERE tenant_id = $1
		  AND application_id = $2
		  AND NOT disabled
		  AND (filter_types IS NULL OR $3 = ANY (filter_types))
		ORDER BY id`, tenantID, applicationID, eventType)
	if err != nil {
		return nil, wrap("read endpoints for event", err)
	}
	defer rows.Close()

	var endpoints []herald.Endpoint
	for rows.Next() {
		e, err := scanEndpoint(rows)
		if err != nil {
			return nil, wrap("read endpoints for event", err)
		}
		endpoints = append(endpoints, e)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("read endpoints for event", err)
	}
	return endpoints, nil
}
