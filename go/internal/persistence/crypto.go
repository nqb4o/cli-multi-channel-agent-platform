package persistence

// AES-GCM encrypt/decrypt for channel config blobs.
//
// Wire format (single concatenated bytestring stored in
// channels.config_encrypted):
//
//	| 1 byte version=0x01 | 12 bytes nonce | ciphertext+tag |
//
//   - AES-256-GCM (32-byte key).
//   - Random 96-bit nonce per encryption (NIST SP 800-38D recommended size).
//   - Versioned framing leaves the door open to migrate to a different
//     AEAD without breaking already-encrypted rows.
//
// MUST stay byte-identical to packages/persistence/src/persistence/crypto.py.

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
)

// VersionAESGCMV1 is the leading byte of every blob produced by the v1
// encrypter. Decrypt rejects any other value.
const VersionAESGCMV1 byte = 0x01

// NonceLen is the GCM nonce length (96 bits, NIST recommended).
const NonceLen = 12

// KeyLen is the AES-256 key length.
const KeyLen = 32

// TagLen is the AES-GCM auth tag length appended to ciphertext.
const TagLen = 16

// CryptoError signals a key/format/auth failure during encrypt or decrypt.
type CryptoError struct {
	Msg string
	Err error
}

func (e *CryptoError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %v", e.Msg, e.Err)
	}
	return e.Msg
}

func (e *CryptoError) Unwrap() error { return e.Err }

func cryptoErr(msg string) error               { return &CryptoError{Msg: msg} }
func cryptoErrWrap(msg string, e error) error  { return &CryptoError{Msg: msg, Err: e} }

// Encrypt encrypts plaintext under key (32 bytes) and returns wire-format bytes.
func Encrypt(key, plaintext []byte) ([]byte, error) {
	if err := checkKey(key); err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, cryptoErrWrap("aes.NewCipher failed", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, cryptoErrWrap("cipher.NewGCM failed", err)
	}
	nonce := make([]byte, NonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return nil, cryptoErrWrap("nonce generation failed", err)
	}
	// Seal appends ciphertext+tag; we prepend the version byte then the nonce
	// to match the Python wire format exactly.
	out := make([]byte, 0, 1+NonceLen+len(plaintext)+TagLen)
	out = append(out, VersionAESGCMV1)
	out = append(out, nonce...)
	out = gcm.Seal(out, nonce, plaintext, nil)
	return out, nil
}

// Decrypt parses wire-format bytes produced by Encrypt (or by the Python
// implementation) and returns the original plaintext.
func Decrypt(key, blob []byte) ([]byte, error) {
	if err := checkKey(key); err != nil {
		return nil, err
	}
	if len(blob) < 1+NonceLen+TagLen {
		return nil, cryptoErr(fmt.Sprintf("ciphertext too short (%d bytes)", len(blob)))
	}
	version := blob[0]
	if version != VersionAESGCMV1 {
		return nil, cryptoErr(fmt.Sprintf("unsupported crypto version 0x%02x", version))
	}
	nonce := blob[1 : 1+NonceLen]
	ct := blob[1+NonceLen:]
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, cryptoErrWrap("aes.NewCipher failed", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, cryptoErrWrap("cipher.NewGCM failed", err)
	}
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, cryptoErrWrap("decryption failed (bad key or tampered ciphertext)", err)
	}
	return pt, nil
}

func checkKey(key []byte) error {
	if len(key) != KeyLen {
		return cryptoErr(fmt.Sprintf("AES-GCM key must be %d bytes, got %d", KeyLen, len(key)))
	}
	return nil
}

// IsCryptoError is a small helper for callers that want to discriminate.
func IsCryptoError(err error) bool {
	var ce *CryptoError
	return errors.As(err, &ce)
}
