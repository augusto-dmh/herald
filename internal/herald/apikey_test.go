package herald_test

import (
	"crypto/sha256"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/augusto-dmh/herald/internal/herald"
)

func TestNewAPIKeyIssuesARecognizablePlaintextKey(t *testing.T) {
	t.Parallel()

	generated, err := herald.NewAPIKey(uuid.Must(uuid.NewV7()), herald.ScopeFull)
	if err != nil {
		t.Fatalf("NewAPIKey: %v", err)
	}

	if !strings.HasPrefix(generated.Plaintext, herald.APIKeyPrefix) {
		t.Errorf("plaintext key %q does not start with %q", generated.Plaintext, herald.APIKeyPrefix)
	}
	secret := strings.TrimPrefix(generated.Plaintext, herald.APIKeyPrefix)
	if len(secret) < 32 {
		t.Errorf("secret part is %d characters, too few to resist guessing", len(secret))
	}
}

func TestNewAPIKeyIssuesADistinctKeyEveryTime(t *testing.T) {
	t.Parallel()

	tenantID := uuid.Must(uuid.NewV7())
	seen := make(map[string]bool)
	for range 100 {
		generated, err := herald.NewAPIKey(tenantID, herald.ScopeIngest)
		if err != nil {
			t.Fatalf("NewAPIKey: %v", err)
		}
		if seen[generated.Plaintext] {
			t.Fatalf("NewAPIKey repeated the key %q", generated.Plaintext)
		}
		seen[generated.Plaintext] = true
	}
}

// The record a caller is handed to persist must carry no trace of the
// plaintext, whatever fields it grows: every string and byte field is
// searched for the secret.
func TestNewAPIKeyRecordCarriesNoPlaintext(t *testing.T) {
	t.Parallel()

	generated, err := herald.NewAPIKey(uuid.Must(uuid.NewV7()), herald.ScopeFull)
	if err != nil {
		t.Fatalf("NewAPIKey: %v", err)
	}
	secret := strings.TrimPrefix(generated.Plaintext, herald.APIKeyPrefix)

	value := reflect.ValueOf(generated.Key)
	for i := range value.NumField() {
		field := value.Type().Field(i)
		var content string
		switch value.Field(i).Interface().(type) {
		case string:
			content = value.Field(i).String()
		case []byte:
			content = string(value.Field(i).Bytes())
		default:
			continue
		}
		if strings.Contains(content, secret) {
			t.Errorf("stored field %s holds the key secret", field.Name)
		}
	}
}

func TestNewAPIKeyRecordHashesThePlaintext(t *testing.T) {
	t.Parallel()

	generated, err := herald.NewAPIKey(uuid.Must(uuid.NewV7()), herald.ScopeFull)
	if err != nil {
		t.Fatalf("NewAPIKey: %v", err)
	}

	want := sha256.Sum256([]byte(generated.Plaintext))
	if !herald.EqualHash(generated.Key.Hash, want[:]) {
		t.Errorf("stored hash does not match SHA-256 of the issued key")
	}
	if !herald.EqualHash(generated.Key.Hash, herald.HashAPIKey(generated.Plaintext)) {
		t.Errorf("authenticating the issued key would not find its stored hash")
	}
}

func TestAPIKeyDisplayPrefixShowsOnlyAFragmentOfTheKey(t *testing.T) {
	t.Parallel()

	generated, err := herald.NewAPIKey(uuid.Must(uuid.NewV7()), herald.ScopeFull)
	if err != nil {
		t.Fatalf("NewAPIKey: %v", err)
	}

	prefix := generated.Key.Prefix
	if !strings.HasPrefix(generated.Plaintext, prefix) {
		t.Errorf("display prefix %q does not identify key %q", prefix, generated.Plaintext)
	}
	if prefix == generated.Plaintext {
		t.Fatalf("display prefix is the whole key")
	}
	revealed := len(prefix) - len(herald.APIKeyPrefix)
	if revealed > 12 {
		t.Errorf("display prefix reveals %d characters of the secret, too many", revealed)
	}
}

func TestNewAPIKeyRejectsAnUnknownScope(t *testing.T) {
	t.Parallel()

	if _, err := herald.NewAPIKey(uuid.Must(uuid.NewV7()), herald.Scope("admin")); err == nil {
		t.Errorf("NewAPIKey with an unknown scope returned no error")
	}
}

func TestHashAPIKeySeparatesDifferentKeys(t *testing.T) {
	t.Parallel()

	if herald.EqualHash(herald.HashAPIKey("hrld_live_a"), herald.HashAPIKey("hrld_live_b")) {
		t.Errorf("two different keys hash to the same value")
	}
}
