package config

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"

	"golang.org/x/crypto/bcrypt"
)

const (
	bcryptCost   = 12
	apiKeyPrefix = "pgc_"
)

// HashPassword returns the bcrypt hash of a password.
func HashPassword(password string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return "", err
	}
	return string(h), nil
}

// CheckPassword reports whether password matches the bcrypt hash.
func CheckPassword(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// NewAPIKey generates a key ("pgc_" + 32 random bytes in base64url) and the
// SHA-256 hex digest to store. The clear key is shown once and never kept.
func NewAPIKey() (clear, hash string) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		panic("config: no randomness available: " + err.Error())
	}
	clear = apiKeyPrefix + base64.RawURLEncoding.EncodeToString(buf)
	return clear, HashAPIKey(clear)
}

// HashAPIKey returns the SHA-256 hex digest of a clear API key.
func HashAPIKey(clear string) string {
	sum := sha256.Sum256([]byte(clear))
	return hex.EncodeToString(sum[:])
}

// MatchAPIKey finds the configured key matching the clear value.
func MatchAPIKey(cfg *Config, clear string) (*APIKey, bool) {
	if cfg == nil || clear == "" {
		return nil, false
	}
	h := []byte(HashAPIKey(clear))
	var found *APIKey
	for i := range cfg.APIKeys {
		if subtle.ConstantTimeCompare(h, []byte(cfg.APIKeys[i].KeyHash)) == 1 && found == nil {
			found = &cfg.APIKeys[i]
		}
	}
	return found, found != nil
}
