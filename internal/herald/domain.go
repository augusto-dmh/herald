// Package herald holds the domain types shared by every layer of the
// service: the tenant-owned entity set of ADR-0002 and the validation
// rules that guard it. It has no dependency on storage or transport.
package herald

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ValidationError reports input that a domain rule rejects. Field names
// the offending input so a transport layer can tell the caller which
// part of the request to fix.
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("%s: %s", e.Field, e.Message)
}

func invalid(field, message string) error {
	return &ValidationError{Field: field, Message: message}
}

// NewID returns a fresh UUIDv7: time-ordered, so identifiers cluster by
// creation time in B-tree indexes and double as pagination cursors.
func NewID() (uuid.UUID, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.Nil, fmt.Errorf("herald: generate id: %w", err)
	}
	return id, nil
}

// Scope is the authority granted to an API key.
type Scope string

const (
	// ScopeFull may perform every operation on its tenant.
	ScopeFull Scope = "full"
	// ScopeIngest may only submit messages.
	ScopeIngest Scope = "ingest"
)

// Valid reports whether s is a scope herald recognizes.
func (s Scope) Valid() bool {
	return s == ScopeFull || s == ScopeIngest
}

// Permits reports whether a key holding s may perform an operation that
// requires the want scope. A full key covers every scope; an ingest key
// covers only ingest.
func (s Scope) Permits(want Scope) bool {
	if !s.Valid() || !want.Valid() {
		return false
	}
	return s == ScopeFull || s == want
}

// DeliveryStatus is the API-facing state of a single delivery. It is
// herald's own projection: the queue's job state is never exposed.
type DeliveryStatus string

const (
	// DeliveryPending has not yet been attempted, or is between attempts.
	DeliveryPending DeliveryStatus = "pending"
	// DeliveryDelivered got a 2xx response.
	DeliveryDelivered DeliveryStatus = "delivered"
	// DeliveryFailed exhausted its chances without a 2xx response.
	DeliveryFailed DeliveryStatus = "failed"
)

// Valid reports whether s is a delivery status herald recognizes.
func (s DeliveryStatus) Valid() bool {
	return s == DeliveryPending || s == DeliveryDelivered || s == DeliveryFailed
}

// Tenant is the isolation boundary: every other entity belongs to
// exactly one tenant and is never visible outside it.
type Tenant struct {
	ID        uuid.UUID
	Name      string
	CreatedAt time.Time
}

// Validate checks a tenant before it is stored.
func (t Tenant) Validate() error {
	name := strings.TrimSpace(t.Name)
	if name == "" {
		return invalid("name", "must not be empty")
	}
	if len(name) > maxNameLength {
		return invalid("name", fmt.Sprintf("must be at most %d characters", maxNameLength))
	}
	return nil
}

// APIKey is the stored half of a credential. The plaintext key exists
// only in the response that created it: what persists is its SHA-256
// hash, which authentication looks up, and Prefix, a leading fragment
// kept solely so humans can tell their keys apart.
type APIKey struct {
	ID         uuid.UUID
	TenantID   uuid.UUID
	Hash       []byte
	Prefix     string
	Scope      Scope
	LastUsedAt *time.Time
	CreatedAt  time.Time
}

// Application is a tenant's addressable stream of messages, named by a
// caller-chosen uid that is unique within the tenant.
type Application struct {
	ID        uuid.UUID
	TenantID  uuid.UUID
	UID       string
	Name      string
	CreatedAt time.Time
}

// Validate checks an application before it is stored.
func (a Application) Validate() error {
	if err := ValidateApplicationUID(a.UID); err != nil {
		return err
	}
	name := strings.TrimSpace(a.Name)
	if name == "" {
		return invalid("name", "must not be empty")
	}
	if len(name) > maxNameLength {
		return invalid("name", fmt.Sprintf("must be at most %d characters", maxNameLength))
	}
	return nil
}

// Endpoint is a URL registered under an application. A nil or empty
// FilterTypes means the endpoint wants every event type; a populated
// one restricts it to the listed types. A disabled endpoint receives
// nothing.
type Endpoint struct {
	ID            uuid.UUID
	TenantID      uuid.UUID
	ApplicationID uuid.UUID
	URL           string
	Description   string
	FilterTypes   []string
	Disabled      bool
	CreatedAt     time.Time
}

// Validate checks an endpoint before it is stored.
func (e Endpoint) Validate() error {
	if err := ValidateEndpointURL(e.URL); err != nil {
		return err
	}
	for _, eventType := range e.FilterTypes {
		var verr *ValidationError
		if errors.As(ValidateEventType(eventType), &verr) {
			return invalid("filter_types", verr.Message)
		}
	}
	return nil
}

// Message is one event submitted by an application, fanned out to the
// endpoints that match its event type.
type Message struct {
	ID            uuid.UUID
	TenantID      uuid.UUID
	ApplicationID uuid.UUID
	EventType     string
	Payload       json.RawMessage
	CreatedAt     time.Time
}

// Validate checks a message before it is stored.
func (m Message) Validate() error {
	if err := ValidateEventType(m.EventType); err != nil {
		return err
	}
	if len(m.Payload) == 0 {
		return invalid("payload", "must not be empty")
	}
	if !json.Valid(m.Payload) {
		return invalid("payload", "must be valid JSON")
	}
	return nil
}

// Delivery is the intent to deliver one message to one endpoint, and
// the record of how that turned out. It is the unit of work behind a
// queue job and the unit the API reports on.
type Delivery struct {
	ID         uuid.UUID
	TenantID   uuid.UUID
	MessageID  uuid.UUID
	EndpointID uuid.UUID
	Status     DeliveryStatus
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// DeliveryAttempt records one execution of a delivery. Attempts are
// append-only and are never deduplicated by attempt number: a re-run of
// the same attempt is recorded as the separate execution it was.
type DeliveryAttempt struct {
	ID              uuid.UUID
	TenantID        uuid.UUID
	DeliveryID      uuid.UUID
	AttemptNumber   int
	StatusCode      *int
	Success         bool
	Error           string
	ResponseSnippet string
	Duration        time.Duration
	AttemptedAt     time.Time
}

const (
	maxNameLength      = 200
	maxEventTypeLength = 255
	maxUIDLength       = 256
	maxURLLength       = 2048
)

// ValidateEventType checks the string an endpoint's filters match on.
func ValidateEventType(eventType string) error {
	switch {
	case eventType == "":
		return invalid("event_type", "must not be empty")
	case len(eventType) > maxEventTypeLength:
		return invalid("event_type", fmt.Sprintf("must be at most %d characters", maxEventTypeLength))
	case strings.TrimSpace(eventType) != eventType:
		return invalid("event_type", "must not have leading or trailing whitespace")
	}
	return nil
}

// ValidateApplicationUID checks a caller-chosen application identifier.
// It travels in URL paths, so it is restricted to characters that need
// no escaping.
func ValidateApplicationUID(uid string) error {
	if uid == "" {
		return invalid("uid", "must not be empty")
	}
	if len(uid) > maxUIDLength {
		return invalid("uid", fmt.Sprintf("must be at most %d characters", maxUIDLength))
	}
	for _, r := range uid {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return invalid("uid", "may contain only letters, digits, '-', '_' and '.'")
		}
	}
	return nil
}

// ValidateEndpointURL checks a destination herald is willing to POST
// to: an absolute http or https URL with a host.
func ValidateEndpointURL(raw string) error {
	if raw == "" {
		return invalid("url", "must not be empty")
	}
	if len(raw) > maxURLLength {
		return invalid("url", fmt.Sprintf("must be at most %d characters", maxURLLength))
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return invalid("url", "must be a valid URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return invalid("url", "must use the http or https scheme")
	}
	if parsed.Host == "" {
		return invalid("url", "must include a host")
	}
	if parsed.User != nil {
		return invalid("url", "must not embed credentials")
	}
	return nil
}
