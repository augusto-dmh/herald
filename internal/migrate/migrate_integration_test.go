//go:build integration

package migrate_test

import (
	"bytes"
	"context"
	"os"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/augusto-dmh/herald/internal/migrate"
	"github.com/augusto-dmh/herald/internal/testdb"
)

func TestMain(m *testing.M) { os.Exit(testdb.RunMain(m)) }

// latestVersion is the highest embedded migration; the schema is only
// complete once every one of them has been applied.
const latestVersion = 1

func migrated(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return testdb.NewDB(t, migrate.Migrate)
}

// tableNames returns every table herald owns in the database.
func tableNames(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT table_name FROM information_schema.tables
		WHERE table_schema = 'public' AND table_type = 'BASE TABLE'`)
	if err != nil {
		t.Fatalf("read tables: %v", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan table name: %v", err)
		}
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func columnNames(t *testing.T, pool *pgxpool.Pool, table string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT column_name FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = $1`, table)
	if err != nil {
		t.Fatalf("read columns of %s: %v", table, err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan column name: %v", err)
		}
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// seed inserts one tenant, application, endpoint, message and delivery
// so constraint behavior can be exercised on real rows.
type seeded struct {
	tenantID      uuid.UUID
	applicationID uuid.UUID
	endpointID    uuid.UUID
	messageID     uuid.UUID
	deliveryID    uuid.UUID
}

func seed(t *testing.T, pool *pgxpool.Pool) seeded {
	t.Helper()
	ctx := context.Background()
	s := seeded{
		tenantID:      uuid.Must(uuid.NewV7()),
		applicationID: uuid.Must(uuid.NewV7()),
		endpointID:    uuid.Must(uuid.NewV7()),
		messageID:     uuid.Must(uuid.NewV7()),
		deliveryID:    uuid.Must(uuid.NewV7()),
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed %s: %v", sql, err)
		}
	}
	exec(`INSERT INTO tenants (id, name) VALUES ($1, 'acme')`, s.tenantID)
	exec(`INSERT INTO applications (id, tenant_id, uid, name) VALUES ($1, $2, 'billing', 'Billing')`,
		s.applicationID, s.tenantID)
	exec(`INSERT INTO endpoints (id, tenant_id, application_id, url)
	      VALUES ($1, $2, $3, 'https://example.com/hooks')`,
		s.endpointID, s.tenantID, s.applicationID)
	exec(`INSERT INTO messages (id, tenant_id, application_id, event_type, payload)
	      VALUES ($1, $2, $3, 'invoice.paid', $4)`,
		s.messageID, s.tenantID, s.applicationID, []byte(`{}`))
	exec(`INSERT INTO deliveries (id, tenant_id, message_id, endpoint_id)
	      VALUES ($1, $2, $3, $4)`,
		s.deliveryID, s.tenantID, s.messageID, s.endpointID)
	return s
}

func TestMigrateCreatesTheWholeSchemaOnAnEmptyDatabase(t *testing.T) {
	pool := migrated(t)
	ctx := context.Background()

	want := []string{
		"api_keys", "applications", "deliveries", "delivery_attempts",
		"endpoint_secrets", "endpoints", "herald_migrations", "messages", "tenants",
	}
	if got := tableNames(t, pool); !slices.Equal(got, want) {
		t.Errorf("tables = %v, want %v", got, want)
	}

	var version int
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(MAX(version), 0) FROM herald_migrations`).Scan(&version); err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	if version != latestVersion {
		t.Errorf("schema version = %d, want %d", version, latestVersion)
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	pool := migrated(t)
	ctx := context.Background()

	before := tableNames(t, pool)
	if err := migrate.Migrate(ctx, pool); err != nil {
		t.Fatalf("second Migrate returned %v, want nil", err)
	}
	if after := tableNames(t, pool); !slices.Equal(after, before) {
		t.Errorf("tables after a second Migrate = %v, want %v", after, before)
	}

	var applied int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM herald_migrations`).Scan(&applied); err != nil {
		t.Fatalf("count applied migrations: %v", err)
	}
	if applied != latestVersion {
		t.Errorf("recorded migrations = %d, want %d (nothing re-applied)", applied, latestVersion)
	}
}

// Tenant isolation has to be expressible as a predicate on whichever
// table is being read, so every tenant-owned row carries its tenant
// even when the owner could be reached through a join.
func TestEveryTenantOwnedTableCarriesTheTenantColumn(t *testing.T) {
	pool := migrated(t)

	for _, table := range tableNames(t, pool) {
		if table == "tenants" || table == "herald_migrations" {
			continue
		}
		if !slices.Contains(columnNames(t, pool, table), "tenant_id") {
			t.Errorf("table %s has no tenant_id column, so reads of it cannot be tenant-scoped", table)
		}
	}
}

// The schema must give a plaintext key nowhere to live.
func TestAPIKeysHaveNowhereToStoreAPlaintextKey(t *testing.T) {
	pool := migrated(t)

	want := []string{"created_at", "id", "key_hash", "last_used_at", "prefix", "scope", "tenant_id"}
	if got := columnNames(t, pool, "api_keys"); !slices.Equal(got, want) {
		t.Errorf("api_keys columns = %v, want exactly %v", got, want)
	}

	var dataType string
	if err := pool.QueryRow(context.Background(), `
		SELECT data_type FROM information_schema.columns
		WHERE table_name = 'api_keys' AND column_name = 'key_hash'`).Scan(&dataType); err != nil {
		t.Fatalf("read key_hash type: %v", err)
	}
	if dataType != "bytea" {
		t.Errorf("key_hash is %s, want bytea", dataType)
	}

	// Authentication looks a key up by its hash, which needs the hash to
	// be both indexed and unambiguous.
	s := seed(t, pool)
	insert := func(id uuid.UUID) error {
		_, err := pool.Exec(context.Background(),
			`INSERT INTO api_keys (id, tenant_id, key_hash, prefix, scope)
			 VALUES ($1, $2, $3, 'hrld_live_abc', 'full')`,
			id, s.tenantID, []byte("the-same-hash"))
		return err
	}
	if err := insert(uuid.Must(uuid.NewV7())); err != nil {
		t.Fatalf("insert api key: %v", err)
	}
	if err := insert(uuid.Must(uuid.NewV7())); err == nil {
		t.Errorf("two keys with the same hash were accepted; authentication would be ambiguous")
	}
}

// The bytes a tenant submitted are what herald delivers, hands back and
// will one day sign. A json or jsonb column would parse and
// re-serialize them — sorting keys, discarding duplicates, rewriting
// whitespace — so the document that came back would not be the document
// that came in.
func TestAMessagePayloadIsKeptAsTheBytesThatWereSubmitted(t *testing.T) {
	pool := migrated(t)
	ctx := context.Background()

	var dataType string
	if err := pool.QueryRow(ctx, `
		SELECT data_type FROM information_schema.columns
		WHERE table_name = 'messages' AND column_name = 'payload'`).Scan(&dataType); err != nil {
		t.Fatalf("read payload type: %v", err)
	}
	if dataType != "bytea" {
		t.Errorf("payload is %s, want bytea so the submitted bytes survive", dataType)
	}

	// A document a JSON column would not have returned unchanged: keys
	// out of order, a repeated key, and whitespace that means nothing to
	// a parser and everything to a signature.
	submitted := []byte(`{"zebra":1,  "alpha":2,"zebra":3}`)
	s := seed(t, pool)
	if _, err := pool.Exec(ctx,
		`UPDATE messages SET payload = $2 WHERE id = $1`, s.messageID, submitted); err != nil {
		t.Fatalf("store the submitted payload: %v", err)
	}

	var stored []byte
	if err := pool.QueryRow(ctx,
		`SELECT payload FROM messages WHERE id = $1`, s.messageID).Scan(&stored); err != nil {
		t.Fatalf("read the stored payload: %v", err)
	}
	if !bytes.Equal(stored, submitted) {
		t.Errorf("payload came back as %s, want %s", stored, submitted)
	}
}

// A NULL filter list is the match-everything subscription; an empty one
// would be a subscription to nothing, which is always a mistake.
func TestEndpointFiltersSeparateMatchAllFromASubscriptionToNothing(t *testing.T) {
	pool := migrated(t)
	ctx := context.Background()
	s := seed(t, pool)

	insertEndpoint := func(filters any) error {
		_, err := pool.Exec(ctx,
			`INSERT INTO endpoints (id, tenant_id, application_id, url, filter_types)
			 VALUES ($1, $2, $3, 'https://example.com/hooks', $4)`,
			uuid.Must(uuid.NewV7()), s.tenantID, s.applicationID, filters)
		return err
	}

	if err := insertEndpoint(nil); err != nil {
		t.Errorf("an endpoint with no filters was rejected: %v", err)
	}
	if err := insertEndpoint([]string{"invoice.paid"}); err != nil {
		t.Errorf("an endpoint filtered to one event type was rejected: %v", err)
	}
	if err := insertEndpoint([]string{}); err == nil {
		t.Errorf("an endpoint subscribed to no event types was accepted")
	}

	// The default is to receive everything, and to be enabled.
	var filters []string
	var disabled bool
	if err := pool.QueryRow(ctx,
		`SELECT filter_types, disabled FROM endpoints WHERE id = $1`, s.endpointID).
		Scan(&filters, &disabled); err != nil {
		t.Fatalf("read seeded endpoint: %v", err)
	}
	if filters != nil {
		t.Errorf("filter_types defaults to %v, want NULL so the endpoint receives every event", filters)
	}
	if disabled {
		t.Errorf("a newly created endpoint is disabled, want enabled")
	}
}

func TestOneDeliveryPerMessageAndEndpointWhileAttemptsAccumulate(t *testing.T) {
	pool := migrated(t)
	ctx := context.Background()
	s := seed(t, pool)

	_, err := pool.Exec(ctx,
		`INSERT INTO deliveries (id, tenant_id, message_id, endpoint_id) VALUES ($1, $2, $3, $4)`,
		uuid.Must(uuid.NewV7()), s.tenantID, s.messageID, s.endpointID)
	if err == nil {
		t.Errorf("a message was fanned out to the same endpoint twice")
	}

	// A re-run of the same attempt is a second execution and is recorded
	// as one, so attempt numbers are deliberately not unique.
	insertAttempt := func() error {
		_, err := pool.Exec(ctx,
			`INSERT INTO delivery_attempts
			   (id, tenant_id, delivery_id, attempt_number, status_code, success, duration_ms)
			 VALUES ($1, $2, $3, 1, 500, false, 12)`,
			uuid.Must(uuid.NewV7()), s.tenantID, s.deliveryID)
		return err
	}
	if err := insertAttempt(); err != nil {
		t.Fatalf("insert attempt: %v", err)
	}
	if err := insertAttempt(); err != nil {
		t.Errorf("a re-run of attempt 1 could not be recorded: %v", err)
	}

	// No response at all is distinguishable from an error status.
	_, err = pool.Exec(ctx,
		`INSERT INTO delivery_attempts
		   (id, tenant_id, delivery_id, attempt_number, success, error, duration_ms)
		 VALUES ($1, $2, $3, 2, false, 'connection refused', 3)`,
		uuid.Must(uuid.NewV7()), s.tenantID, s.deliveryID)
	if err != nil {
		t.Errorf("an attempt that got no response could not be recorded: %v", err)
	}
}

func TestScopeAndStatusColumnsRejectUndocumentedValues(t *testing.T) {
	pool := migrated(t)
	ctx := context.Background()
	s := seed(t, pool)

	for _, scope := range []string{"full", "ingest"} {
		_, err := pool.Exec(ctx,
			`INSERT INTO api_keys (id, tenant_id, key_hash, prefix, scope)
			 VALUES ($1, $2, $3, 'hrld_live_abc', $4)`,
			uuid.Must(uuid.NewV7()), s.tenantID, []byte(scope), scope)
		if err != nil {
			t.Errorf("documented scope %q was rejected: %v", scope, err)
		}
	}
	_, err := pool.Exec(ctx,
		`INSERT INTO api_keys (id, tenant_id, key_hash, prefix, scope)
		 VALUES ($1, $2, $3, 'hrld_live_abc', 'admin')`,
		uuid.Must(uuid.NewV7()), s.tenantID, []byte("admin"))
	if err == nil {
		t.Errorf("an undocumented key scope was accepted")
	}

	var status string
	if err := pool.QueryRow(ctx,
		`SELECT status FROM deliveries WHERE id = $1`, s.deliveryID).Scan(&status); err != nil {
		t.Fatalf("read seeded delivery: %v", err)
	}
	if status != "pending" {
		t.Errorf("a new delivery starts as %q, want pending", status)
	}
	for _, want := range []string{"delivered", "failed", "pending"} {
		if _, err := pool.Exec(ctx,
			`UPDATE deliveries SET status = $1 WHERE id = $2`, want, s.deliveryID); err != nil {
			t.Errorf("documented delivery status %q was rejected: %v", want, err)
		}
	}
	if _, err := pool.Exec(ctx,
		`UPDATE deliveries SET status = 'sending' WHERE id = $1`, s.deliveryID); err == nil {
		t.Errorf("an undocumented delivery status was accepted")
	}
}
