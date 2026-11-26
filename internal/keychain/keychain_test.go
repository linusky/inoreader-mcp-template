package keychain

import (
	"bytes"
	"errors"
	"testing"
)

func TestMemoryStoreRoundTrip(t *testing.T) {
	store := NewMemoryStore()
	secret := []byte(`{"access_token":"tok-1","refresh_token":"ref-1"}`)

	if _, err := store.Get(ServiceName, DefaultAccount); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}

	if err := store.Set(ServiceName, DefaultAccount, secret); err != nil {
		t.Fatalf("Set failed: %v", err)
	}
	// Mutating original slice must not affect stored bytes.
	secret[0] = 'X'

	got, err := store.Get(ServiceName, DefaultAccount)
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if !bytes.Equal(got, []byte(`{"access_token":"tok-1","refresh_token":"ref-1"}`)) {
		t.Fatalf("unexpected secret: %q", string(got))
	}

	if err := store.Delete(ServiceName, DefaultAccount); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
	if _, err := store.Get(ServiceName, DefaultAccount); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
}

func TestValidateKeyRejectsUnsafeIdentifiers(t *testing.T) {
	bad := []string{
		"",
		"..",
		"../etc",
		"foo..bar",
		".hidden",
		"-flag",
		"has space",
		"semi;colon",
		"quote\"here",
		"new\nline",
		"$(whoami)",
	}
	store := NewMemoryStore()
	for _, id := range bad {
		if err := store.Set(id, DefaultAccount, []byte("x")); err == nil {
			t.Errorf("expected error for unsafe service %q", id)
		}
		if err := store.Set(ServiceName, id, []byte("x")); err == nil {
			t.Errorf("expected error for unsafe account %q", id)
		}
	}
}

func TestEncodeDecodeSecretNeverStartsWithDash(t *testing.T) {
	// 0xF8..0xFB would start with '-' under RawURLEncoding; RawStdEncoding uses '+' instead.
	orig := []byte{0xF8, 0xFF, 0xFE, '{', 'a', '}'}
	enc := encodeSecret(orig)
	if len(enc) == 0 || enc[0] == '-' {
		t.Fatalf("encoded secret must not start with '-': %q", enc)
	}
	dec, err := decodeSecret(enc + "\n")
	if err != nil {
		t.Fatalf("decodeSecret failed: %v", err)
	}
	if !bytes.Equal(dec, orig) {
		t.Fatalf("roundtrip mismatch: got %x", dec)
	}
}
