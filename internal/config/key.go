package config

import (
	"encoding/base64"
	"fmt"
)

func decodeKey(s string) ([]byte, error) {
	if s == "" {
		return nil, fmt.Errorf("TOKEN_ENCRYPTION_KEY is required; generate one with:\n  openssl rand -base64 32")
	}
	key, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("TOKEN_ENCRYPTION_KEY must be base64: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("TOKEN_ENCRYPTION_KEY must decode to 32 bytes, got %d", len(key))
	}
	return key, nil
}
