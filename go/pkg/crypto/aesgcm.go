// Package crypto provides AES-256-GCM encrypt/decrypt for channel config
// blobs and any other small secrets persisted by the platform.
//
// Wire format (single concatenated bytestring):
//
//	| 1 byte version=0x01 | 12 bytes nonce | ciphertext+tag |
//
//   - AES-256-GCM (32-byte key)
//   - Random 96-bit nonce per encryption (NIST SP 800-38D recommended)
//   - The Go standard library's crypto/cipher GCM mode appends the auth tag
//     to the ciphertext, matching cryptography.hazmat's AESGCM in Python.
//
// The version byte is intentional so we can migrate to a different AEAD
// without breaking already-encrypted rows.
//
// This is the FROZEN shared wrapper for both Go services and the Python
// implementation in packages/persistence/src/persistence/crypto.py — the
// two MUST stay byte-compatible.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
)

// VersionAESGCMV1 is the leading wire-format byte for v1 blobs.
const VersionAESGCMV1 byte = 0x01

// NonceLen is the AES-GCM nonce length (96 bits).
const NonceLen = 12

// KeyLen is the AES-256 key length.
const KeyLen = 32

// tagLen is the AES-GCM authentication tag length (128 bits).
const tagLen = 16

// ErrCrypto is returned for any encrypt/decrypt failure (bad key length,
// short blob, unsupported version, failed authentication).
var ErrCrypto = errors.New("crypto error")

// Encrypt encrypts plaintext under key (32 bytes) and returns the wire-format
// blob `0x01 || nonce(12) || ciphertext+tag`.
//
// A fresh random 12-byte nonce is generated per call from crypto/rand.
func Encrypt(key, plaintext []byte) ([]byte, error) {
	if err := checkKey(key); err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("%w: aes.NewCipher: %v", ErrCrypto, err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("%w: cipher.NewGCM: %v", ErrCrypto, err)
	}
	nonce := make([]byte, NonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("%w: rand.Read: %v", ErrCrypto, err)
	}
	// Allocate the full output up front: 1 + 12 + len(plaintext) + 16.
	out := make([]byte, 0, 1+NonceLen+len(plaintext)+tagLen)
	out = append(out, VersionAESGCMV1)
	out = append(out, nonce...)
	// gcm.Seal appends ciphertext+tag to its first arg.
	out = gcm.Seal(out, nonce, plaintext, nil)
	return out, nil
}

// Decrypt validates the version byte, splits nonce / ciphertext+tag, and
// returns the original plaintext. Returns ErrCrypto on any failure.
func Decrypt(key, blob []byte) ([]byte, error) {
	if err := checkKey(key); err != nil {
		return nil, err
	}
	if len(blob) < 1+NonceLen+tagLen {
		return nil, fmt.Errorf("%w: ciphertext too short (%d bytes)", ErrCrypto, len(blob))
	}
	if blob[0] != VersionAESGCMV1 {
		return nil, fmt.Errorf("%w: unsupported crypto version 0x%02x", ErrCrypto, blob[0])
	}
	nonce := blob[1 : 1+NonceLen]
	ct := blob[1+NonceLen:]
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("%w: aes.NewCipher: %v", ErrCrypto, err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("%w: cipher.NewGCM: %v", ErrCrypto, err)
	}
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: decryption failed (bad key or tampered ciphertext)", ErrCrypto)
	}
	return pt, nil
}

func checkKey(key []byte) error {
	if len(key) != KeyLen {
		return fmt.Errorf("%w: AES-GCM key must be %d bytes, got %d", ErrCrypto, KeyLen, len(key))
	}
	return nil
}
