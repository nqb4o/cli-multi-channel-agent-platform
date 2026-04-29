package crypto

import (
	"bytes"
	"crypto/rand"
	"errors"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	key := make([]byte, KeyLen)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	plain := []byte("hello, world — channel secret!")

	blob, err := Encrypt(key, plain)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	got, err := Decrypt(key, blob)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("round-trip mismatch: got %q want %q", got, plain)
	}
}

func TestWireFormat(t *testing.T) {
	key := make([]byte, KeyLen)
	plain := []byte("payload")

	blob, err := Encrypt(key, plain)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if blob[0] != VersionAESGCMV1 {
		t.Fatalf("version byte: got 0x%02x want 0x%02x", blob[0], VersionAESGCMV1)
	}
	// 1 (version) + 12 (nonce) + len(plain) + 16 (tag)
	want := 1 + NonceLen + len(plain) + tagLen
	if len(blob) != want {
		t.Fatalf("blob length: got %d want %d", len(blob), want)
	}
}

func TestEncryptUsesFreshNonce(t *testing.T) {
	key := make([]byte, KeyLen)
	plain := []byte("same plaintext, different nonce")

	a, err := Encrypt(key, plain)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Encrypt(key, plain)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a, b) {
		t.Fatal("two encryptions produced identical blobs — nonce is not random")
	}
	// Nonces differ:
	if bytes.Equal(a[1:1+NonceLen], b[1:1+NonceLen]) {
		t.Fatal("nonces match across two encryptions")
	}
}

func TestDecryptRejectsTampering(t *testing.T) {
	key := make([]byte, KeyLen)
	plain := []byte("don't tamper with me")
	blob, err := Encrypt(key, plain)
	if err != nil {
		t.Fatal(err)
	}

	// Flip one bit in the ciphertext (post-nonce).
	tampered := make([]byte, len(blob))
	copy(tampered, blob)
	tampered[len(tampered)-1] ^= 0x01

	if _, err := Decrypt(key, tampered); err == nil {
		t.Fatal("decrypt accepted tampered ciphertext")
	} else if !errors.Is(err, ErrCrypto) {
		t.Fatalf("expected ErrCrypto, got %v", err)
	}
}

func TestDecryptRejectsBadVersion(t *testing.T) {
	key := make([]byte, KeyLen)
	blob, err := Encrypt(key, []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	blob[0] = 0x02 // bump version
	if _, err := Decrypt(key, blob); err == nil {
		t.Fatal("decrypt accepted unsupported version")
	}
}

func TestDecryptRejectsShortBlob(t *testing.T) {
	key := make([]byte, KeyLen)
	if _, err := Decrypt(key, []byte{0x01, 0x00}); err == nil {
		t.Fatal("decrypt accepted absurdly short blob")
	}
}

func TestKeyLengthValidation(t *testing.T) {
	short := make([]byte, KeyLen-1)
	if _, err := Encrypt(short, []byte("x")); err == nil {
		t.Fatal("encrypt accepted short key")
	}
	if _, err := Decrypt(short, make([]byte, 64)); err == nil {
		t.Fatal("decrypt accepted short key")
	}
}

func TestDecryptRejectsBadKey(t *testing.T) {
	key1 := make([]byte, KeyLen)
	key2 := make([]byte, KeyLen)
	key2[0] = 0x42
	blob, err := Encrypt(key1, []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decrypt(key2, blob); err == nil {
		t.Fatal("decrypt accepted wrong key")
	}
}

func TestDecryptZeroKey(t *testing.T) {
	// Encrypt with all-zero key; round-trip stays valid (this is the
	// fixture key shape used by the cross-language test).
	key := make([]byte, KeyLen)
	plain := []byte("hello from go")
	blob, err := Encrypt(key, plain)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decrypt(key, blob)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("zero-key round-trip mismatch")
	}
}
