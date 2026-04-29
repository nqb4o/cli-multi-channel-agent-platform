package gateway

import (
	"strings"
	"testing"
)

func TestParseEncryptionKey_HappyPath(t *testing.T) {
	hexKey := strings.Repeat("a", 64)
	key, err := ParseEncryptionKey(hexKey)
	if err != nil {
		t.Fatal(err)
	}
	if len(key) != 32 {
		t.Fatalf("got %d bytes", len(key))
	}
}

func TestParseEncryptionKey_EmptyIsTypedError(t *testing.T) {
	_, err := ParseEncryptionKey("")
	if err == nil || !IsEncryptionKeyError(err) {
		t.Fatalf("expected EncryptionKeyError, got %v", err)
	}
	if !strings.Contains(err.Error(), "DB_ENCRYPTION_KEY") {
		t.Fatalf("missing helpful message: %v", err)
	}
}

func TestParseEncryptionKey_BadHexIsTypedError(t *testing.T) {
	_, err := ParseEncryptionKey("zz" + strings.Repeat("a", 62))
	if err == nil || !IsEncryptionKeyError(err) {
		t.Fatalf("expected EncryptionKeyError, got %v", err)
	}
}

func TestParseEncryptionKey_WrongLengthIsTypedError(t *testing.T) {
	short := strings.Repeat("ab", 16) // 16 bytes
	_, err := ParseEncryptionKey(short)
	if err == nil || !IsEncryptionKeyError(err) {
		t.Fatalf("expected EncryptionKeyError, got %v", err)
	}
}

func TestEncryptDecryptChannelConfig_RoundTrip(t *testing.T) {
	key, _ := ParseEncryptionKey(strings.Repeat("a", 64))
	plain := []byte(`{"bot_token":"abc"}`)
	ct, err := EncryptChannelConfig(key, plain)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if ct[0] != 0x01 {
		t.Fatalf("expected version byte 0x01, got 0x%02x", ct[0])
	}
	out, err := DecryptChannelConfig(key, ct)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if string(out) != string(plain) {
		t.Fatalf("round-trip mismatch: %q", out)
	}
}
