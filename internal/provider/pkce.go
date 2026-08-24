package provider

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
)

// PKCE is one connect attempt's proof key. Nothing about it is provider
// specific — it is plain RFC 7636 — so it lives with the contracts rather than
// inside any one implementation.
type PKCE struct {
	Verifier  string
	Challenge string
}

func NewPKCE() (PKCE, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return PKCE{}, err
	}
	verifier := base64.RawURLEncoding.EncodeToString(b)
	return PKCE{Verifier: verifier, Challenge: ChallengeFor(verifier)}, nil
}

// ChallengeFor recomputes the S256 challenge from a stored verifier, so a
// pending connect attempt only ever persists one secret.
func ChallengeFor(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func RandomString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
