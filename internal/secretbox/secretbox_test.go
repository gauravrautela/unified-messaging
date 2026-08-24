package secretbox

import (
	"crypto/rand"
	"testing"
)

func TestSealOpenRoundTrip(t *testing.T) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	const secret = "0.AXkA-refresh-token-value"

	sealed, err := Seal(key, secret)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if sealed == secret {
		t.Fatal("sealed value is plaintext")
	}

	got, err := Open(key, sealed)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got != secret {
		t.Fatalf("round trip = %q, want %q", got, secret)
	}
}

func TestSealIsNondeterministic(t *testing.T) {
	key := make([]byte, 32)
	a, _ := Seal(key, "same")
	b, _ := Seal(key, "same")
	if a == b {
		t.Fatal("identical ciphertexts: nonce is not random")
	}
}

func TestOpenWithWrongKeyFails(t *testing.T) {
	good := make([]byte, 32)
	if _, err := rand.Read(good); err != nil {
		t.Fatal(err)
	}
	sealed, err := Seal(good, "secret")
	if err != nil {
		t.Fatal(err)
	}
	bad := make([]byte, 32)
	if _, err := Open(bad, sealed); err == nil {
		t.Fatal("decrypted with the wrong key")
	}
}
