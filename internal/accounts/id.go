package accounts

import (
	"crypto/rand"
	"encoding/hex"
)

func newID(prefix string) (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return prefix + "_" + hex.EncodeToString(b), nil
}

// NewID is exported for callers that need the same ID shape (webhooks, etc).
func NewID(prefix string) (string, error) { return newID(prefix) }
