package gateway

import (
	"encoding/hex"
	"errors"
	"fmt"

	pkgcrypto "github.com/openclaw/agent-platform/pkg/crypto"
)

// EncryptChannelConfig encrypts the JSON-serialized channel config blob with
// the platform AES-GCM key. The wire format (FROZEN — 0x01 || nonce || ct+tag)
// is shared with the Python persistence.crypto helpers.
func EncryptChannelConfig(key, plaintext []byte) ([]byte, error) {
	return pkgcrypto.Encrypt(key, plaintext)
}

// DecryptChannelConfig is the inverse of EncryptChannelConfig.
func DecryptChannelConfig(key, blob []byte) ([]byte, error) {
	return pkgcrypto.Decrypt(key, blob)
}

// ParseEncryptionKey decodes a hex-encoded AES-GCM key. Returns a typed error
// the caller can map to a 500 with a clear "DB_ENCRYPTION_KEY ..." message.
type EncryptionKeyError struct{ msg string }

func (e *EncryptionKeyError) Error() string { return e.msg }

// IsEncryptionKeyError reports whether err is an EncryptionKeyError (or wraps one).
func IsEncryptionKeyError(err error) bool {
	var t *EncryptionKeyError
	return errors.As(err, &t)
}

// ParseEncryptionKey decodes the hex-encoded 32-byte AES key from cfg, or
// returns an EncryptionKeyError describing the misconfiguration.
func ParseEncryptionKey(hexKey string) ([]byte, error) {
	if hexKey == "" {
		return nil, &EncryptionKeyError{
			msg: "DB_ENCRYPTION_KEY is not configured — channel config cannot be encrypted",
		}
	}
	key, err := hex.DecodeString(hexKey)
	if err != nil {
		return nil, &EncryptionKeyError{
			msg: "DB_ENCRYPTION_KEY is not valid hex",
		}
	}
	if len(key) != pkgcrypto.KeyLen {
		return nil, &EncryptionKeyError{
			msg: fmt.Sprintf("DB_ENCRYPTION_KEY must decode to %d bytes (got %d)", pkgcrypto.KeyLen, len(key)),
		}
	}
	return key, nil
}
