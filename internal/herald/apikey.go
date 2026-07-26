package herald

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"

	"github.com/google/uuid"
)

const (
	// APIKeyPrefix marks a string as a herald API key. It is part of the
	// plaintext so leaked keys are recognizable to secret scanners.
	APIKeyPrefix = "hrld_live_" //nolint:gosec // G101: a public marker, not a credential

	// apiKeySecretBytes is the entropy behind a key. 256 random bits are
	// far beyond guessing range, which is why keys are hashed with a
	// plain SHA-256 rather than a password KDF.
	apiKeySecretBytes = 32

	// displayFragmentLength is how many characters of the secret are
	// kept alongside the prefix. Enough for an operator to tell two keys
	// apart, far too few to narrow down a guess.
	displayFragmentLength = 8
)

// GeneratedAPIKey is a freshly minted credential: the plaintext to hand
// the caller exactly once, and the record to persist in its place.
type GeneratedAPIKey struct {
	// Plaintext is the only copy of the key that will ever exist. It is
	// deliberately absent from Key.
	Plaintext string
	Key       APIKey
}

// NewAPIKey mints a credential for a tenant. The returned Key carries
// only the hash and display prefix, so storing it cannot leak the
// secret: whatever the caller does with the record, the plaintext lives
// solely in the value returned here.
func NewAPIKey(tenantID uuid.UUID, scope Scope) (GeneratedAPIKey, error) {
	if !scope.Valid() {
		return GeneratedAPIKey{}, invalid("scope", "must be one of full, ingest")
	}

	secret := make([]byte, apiKeySecretBytes)
	if _, err := rand.Read(secret); err != nil {
		return GeneratedAPIKey{}, fmt.Errorf("herald: read random bytes for api key: %w", err)
	}
	plaintext := APIKeyPrefix + base64.RawURLEncoding.EncodeToString(secret)

	id, err := NewID()
	if err != nil {
		return GeneratedAPIKey{}, err
	}

	return GeneratedAPIKey{
		Plaintext: plaintext,
		Key: APIKey{
			ID:       id,
			TenantID: tenantID,
			Hash:     HashAPIKey(plaintext),
			Prefix:   APIKeyDisplayPrefix(plaintext),
			Scope:    scope,
		},
	}, nil
}

// HashAPIKey returns the value stored in place of a plaintext key and
// looked up on every authenticated request.
func HashAPIKey(plaintext string) []byte {
	sum := sha256.Sum256([]byte(plaintext))
	return sum[:]
}

// APIKeyDisplayPrefix returns the fragment of a key that is safe to
// show and store: its scannable prefix plus a few characters of the
// secret.
func APIKeyDisplayPrefix(plaintext string) string {
	cut := len(APIKeyPrefix) + displayFragmentLength
	if len(plaintext) < cut {
		return plaintext
	}
	return plaintext[:cut]
}

// EqualHash compares two key hashes without leaking, through timing,
// how much of them matched.
func EqualHash(a, b []byte) bool {
	return subtle.ConstantTimeCompare(a, b) == 1
}
