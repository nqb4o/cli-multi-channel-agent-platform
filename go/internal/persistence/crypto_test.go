package persistence

import (
	"bytes"
	"encoding/hex"
	"errors"
	"testing"
)

// fakeKey returns 32 zero bytes — same convention as the Python conftest's
// fake_key fixture, used so ciphertext layout (length, nonce position) is
// trivially testable.
func fakeKey() []byte { return make([]byte, KeyLen) }

// ---------------------------------------------------------------------------
// Round-trip

func TestEncryptDecryptRoundTripShort(t *testing.T) {
	pt := []byte(`{"webhook_secret": "abc"}`)
	blob, err := Encrypt(fakeKey(), pt)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	got, err := Decrypt(fakeKey(), blob)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if !bytes.Equal(got, pt) {
		t.Fatalf("plaintext mismatch: got %q want %q", got, pt)
	}
}

func TestEncryptDecryptRoundTripEmpty(t *testing.T) {
	blob, err := Encrypt(fakeKey(), []byte{})
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	got, err := Decrypt(fakeKey(), blob)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty plaintext, got %d bytes", len(got))
	}
}

func TestEncryptDecryptRoundTripLarge(t *testing.T) {
	pt := bytes.Repeat([]byte{'x'}, 4096)
	blob, err := Encrypt(fakeKey(), pt)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	got, err := Decrypt(fakeKey(), blob)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if !bytes.Equal(got, pt) {
		t.Fatal("plaintext mismatch on 4KiB payload")
	}
}

// ---------------------------------------------------------------------------
// Wire format

func TestWireFormatVersionByte(t *testing.T) {
	blob, err := Encrypt(fakeKey(), []byte("hello"))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if blob[0] != VersionAESGCMV1 {
		t.Fatalf("version byte = 0x%02x, want 0x%02x", blob[0], VersionAESGCMV1)
	}
}

func TestWireFormatLength(t *testing.T) {
	pt := []byte("hello")
	blob, err := Encrypt(fakeKey(), pt)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	want := 1 + NonceLen + len(pt) + TagLen
	if len(blob) != want {
		t.Fatalf("blob len = %d, want %d", len(blob), want)
	}
}

func TestUniqueNoncePerEncryption(t *testing.T) {
	a, err := Encrypt(fakeKey(), []byte("identical"))
	if err != nil {
		t.Fatalf("encrypt a: %v", err)
	}
	b, err := Encrypt(fakeKey(), []byte("identical"))
	if err != nil {
		t.Fatalf("encrypt b: %v", err)
	}
	if a[0] != b[0] {
		t.Fatalf("version bytes differ: %x vs %x", a[0], b[0])
	}
	if bytes.Equal(a[1:], b[1:]) {
		t.Fatal("nonce/ciphertext identical across two encryptions — randomness broken")
	}
}

// ---------------------------------------------------------------------------
// Error paths

func TestKeyLengthValidationShort(t *testing.T) {
	_, err := Encrypt(make([]byte, 16), []byte("hi"))
	mustCryptoErr(t, err)
}

func TestKeyLengthValidationLong(t *testing.T) {
	_, err := Encrypt(make([]byte, 64), []byte("hi"))
	mustCryptoErr(t, err)
}

func TestUnknownVersionRejected(t *testing.T) {
	blob, err := Encrypt(fakeKey(), []byte("hi"))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	tampered := append([]byte{0x99}, blob[1:]...)
	_, err = Decrypt(fakeKey(), tampered)
	mustCryptoErr(t, err)
}

func TestShortBlobRejected(t *testing.T) {
	_, err := Decrypt(fakeKey(), append([]byte{0x01}, make([]byte, 5)...))
	mustCryptoErr(t, err)
}

func TestTamperedCiphertextFailsAuth(t *testing.T) {
	blob, err := Encrypt(fakeKey(), []byte("original"))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	t1 := append([]byte(nil), blob...)
	t1[len(t1)-1] ^= 0x01
	_, err = Decrypt(fakeKey(), t1)
	mustCryptoErr(t, err)
}

func TestWrongKeyFailsDecryption(t *testing.T) {
	blob, err := Encrypt(fakeKey(), []byte("secret"))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	other := bytes.Repeat([]byte{0x01}, KeyLen)
	_, err = Decrypt(other, blob)
	mustCryptoErr(t, err)
}

func TestDecryptValidatesKeyLength(t *testing.T) {
	blob, err := Encrypt(fakeKey(), []byte("x"))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	_, err = Decrypt(make([]byte, 16), blob)
	mustCryptoErr(t, err)
}

// ---------------------------------------------------------------------------
// Cross-language fixture: a blob produced by Python persistence.encrypt with
// 32 zero bytes as the key. Captured once via:
//
//   .venv/bin/python -c "from persistence import encrypt; ..."
//
// If the wire format ever drifts, this test fails immediately.
const (
	pyFixtureKeyHex  = "0000000000000000000000000000000000000000000000000000000000000000"
	pyFixturePTHex   = "63726f73732d6c616e672d666978747572652d7061796c6f61642d7631" // "cross-lang-fixture-payload-v1"
	pyFixtureBlobHex = "017584b56059a36e56cfab91c34ed5af6c707c31e519990a40ade98ac68e58ec14980ac22a062e04e28a2b462f29bc7a108aaf86266fc0bb0a3e"
)

func TestDecryptPythonProducedBlob(t *testing.T) {
	key, _ := hex.DecodeString(pyFixtureKeyHex)
	wantPT, _ := hex.DecodeString(pyFixturePTHex)
	blob, _ := hex.DecodeString(pyFixtureBlobHex)

	got, err := Decrypt(key, blob)
	if err != nil {
		t.Fatalf("decrypt python blob: %v", err)
	}
	if !bytes.Equal(got, wantPT) {
		t.Fatalf("plaintext mismatch:\n got = %q\n want = %q", got, wantPT)
	}
}


// ---------------------------------------------------------------------------
// helpers

func mustCryptoErr(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected CryptoError, got nil")
	}
	var ce *CryptoError
	if !errors.As(err, &ce) {
		t.Fatalf("expected *CryptoError, got %T: %v", err, err)
	}
}

