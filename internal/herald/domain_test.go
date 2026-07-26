package herald_test

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/augusto-dmh/herald/internal/herald"
)

func TestFullScopeCoversEveryOperationAndIngestCoversOnlyIngest(t *testing.T) {
	t.Parallel()

	cases := []struct {
		held herald.Scope
		want herald.Scope
		ok   bool
	}{
		{herald.ScopeFull, herald.ScopeFull, true},
		{herald.ScopeFull, herald.ScopeIngest, true},
		{herald.ScopeIngest, herald.ScopeIngest, true},
		{herald.ScopeIngest, herald.ScopeFull, false},
		{herald.Scope("admin"), herald.ScopeIngest, false},
		{herald.ScopeFull, herald.Scope("admin"), false},
	}
	for _, c := range cases {
		if got := c.held.Permits(c.want); got != c.ok {
			t.Errorf("Scope(%q).Permits(%q) = %v, want %v", c.held, c.want, got, c.ok)
		}
	}
}

func TestScopeAndDeliveryStatusAcceptOnlyDocumentedValues(t *testing.T) {
	t.Parallel()

	for _, scope := range []herald.Scope{herald.ScopeFull, herald.ScopeIngest} {
		if !scope.Valid() {
			t.Errorf("documented scope %q rejected", scope)
		}
	}
	for _, scope := range []herald.Scope{"", "admin", "FULL"} {
		if scope.Valid() {
			t.Errorf("undocumented scope %q accepted", scope)
		}
	}

	for _, status := range []herald.DeliveryStatus{
		herald.DeliveryPending, herald.DeliveryDelivered, herald.DeliveryFailed,
	} {
		if !status.Valid() {
			t.Errorf("documented delivery status %q rejected", status)
		}
	}
	for _, status := range []herald.DeliveryStatus{"", "sending", "DELIVERED"} {
		if status.Valid() {
			t.Errorf("undocumented delivery status %q accepted", status)
		}
	}
}

func TestNewIDReturnsTimeOrderedUniqueIdentifiers(t *testing.T) {
	t.Parallel()

	const count = 200
	ids := make([]uuid.UUID, 0, count)
	seen := make(map[uuid.UUID]bool, count)
	for range count {
		id, err := herald.NewID()
		if err != nil {
			t.Fatalf("NewID: %v", err)
		}
		if seen[id] {
			t.Fatalf("NewID repeated %s", id)
		}
		seen[id] = true
		if id.Version() != 7 {
			t.Fatalf("NewID returned a version %d UUID, want version 7", id.Version())
		}
		ids = append(ids, id)
	}
	for i := 1; i < len(ids); i++ {
		if ids[i].String() < ids[i-1].String() {
			t.Errorf("identifier %s sorts before its predecessor %s; ids are not time-ordered",
				ids[i], ids[i-1])
		}
	}
}

func TestEndpointURLMustBeAnAbsoluteHTTPTarget(t *testing.T) {
	t.Parallel()

	valid := []string{
		"https://example.com/hooks",
		"http://example.com",
		"https://example.com:8443/hooks?tenant=1",
		"http://127.0.0.1:9000/receive",
	}
	for _, raw := range valid {
		if err := herald.ValidateEndpointURL(raw); err != nil {
			t.Errorf("ValidateEndpointURL(%q) = %v, want nil", raw, err)
		}
	}

	invalid := []string{
		"",
		"example.com/hooks",
		"ftp://example.com/hooks",
		"file:///etc/passwd",
		"https://",
		"http://user:pass@example.com/hooks",
		"://example.com",
	}
	for _, raw := range invalid {
		if err := herald.ValidateEndpointURL(raw); err == nil {
			t.Errorf("ValidateEndpointURL(%q) = nil, want an error", raw)
		}
	}
}

func TestValidationErrorsNameTheOffendingField(t *testing.T) {
	t.Parallel()

	cases := map[string]error{
		"url":        herald.Endpoint{URL: "nonsense"}.Validate(),
		"event_type": herald.Message{EventType: "", Payload: []byte(`{}`)}.Validate(),
		"payload":    herald.Message{EventType: "user.created", Payload: []byte(`{`)}.Validate(),
		"uid":        herald.Application{UID: "has spaces", Name: "app"}.Validate(),
		"name":       herald.Tenant{Name: "  "}.Validate(),
	}
	for field, err := range cases {
		var verr *herald.ValidationError
		if !errors.As(err, &verr) {
			t.Errorf("error for %s is %v, want a *ValidationError", field, err)
			continue
		}
		if verr.Field != field {
			t.Errorf("validation error names field %q, want %q", verr.Field, field)
		}
		if verr.Message == "" {
			t.Errorf("validation error for %q carries no message", field)
		}
	}
}

func TestMessageRequiresAnEventTypeAndJSONPayload(t *testing.T) {
	t.Parallel()

	good := herald.Message{EventType: "invoice.paid", Payload: []byte(`{"id":1}`)}
	if err := good.Validate(); err != nil {
		t.Errorf("Validate on a well-formed message = %v, want nil", err)
	}

	bad := []herald.Message{
		{EventType: "", Payload: []byte(`{}`)},
		{EventType: " invoice.paid", Payload: []byte(`{}`)},
		{EventType: "invoice.paid"},
		{EventType: "invoice.paid", Payload: []byte(`{"id":`)},
		{EventType: "invoice.paid", Payload: []byte(`not json`)},
	}
	for _, m := range bad {
		if err := m.Validate(); err == nil {
			t.Errorf("Validate on message %+v = nil, want an error", m)
		}
	}
}

func TestEndpointFilterTypesMustBeUsableEventTypes(t *testing.T) {
	t.Parallel()

	matchAll := herald.Endpoint{URL: "https://example.com/hooks"}
	if err := matchAll.Validate(); err != nil {
		t.Errorf("an endpoint with no filters is valid, got %v", err)
	}

	filtered := herald.Endpoint{
		URL:         "https://example.com/hooks",
		FilterTypes: []string{"invoice.paid", "user.created"},
	}
	if err := filtered.Validate(); err != nil {
		t.Errorf("Validate on filtered endpoint = %v, want nil", err)
	}

	empty := herald.Endpoint{
		URL:         "https://example.com/hooks",
		FilterTypes: []string{"invoice.paid", ""},
	}
	var verr *herald.ValidationError
	if !errors.As(empty.Validate(), &verr) || verr.Field != "filter_types" {
		t.Errorf("an empty filter entry was not reported against filter_types: %v", empty.Validate())
	}
}

func TestApplicationUIDIsRestrictedToURLSafeCharacters(t *testing.T) {
	t.Parallel()

	for _, uid := range []string{"acme", "acme-prod", "acme_prod.eu", "app1"} {
		if err := herald.ValidateApplicationUID(uid); err != nil {
			t.Errorf("ValidateApplicationUID(%q) = %v, want nil", uid, err)
		}
	}
	for _, uid := range []string{"", "has spaces", "slash/es", "question?", "hash#", "%2e%2e"} {
		if err := herald.ValidateApplicationUID(uid); err == nil {
			t.Errorf("ValidateApplicationUID(%q) = nil, want an error", uid)
		}
	}
}
