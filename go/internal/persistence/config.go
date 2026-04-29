// Package persistence — F12 Postgres + AES-GCM + migrations.
//
// Two pieces of config:
//   - DB_DSN — Postgres connection string (pgx-compatible).
//   - DB_ENCRYPTION_KEY — 32-byte key (64 hex chars) for AES-GCM encryption
//     of channels.config_encrypted.
//
// Both are read from environment variables. For prod, DB_ENCRYPTION_KEY
// should come from a KMS-issued secret; for dev, it lives in .env / shell env.
package persistence

import (
	"encoding/hex"
	"fmt"
	"os"
)

// DefaultDSN matches the Python default in persistence.config.
const DefaultDSN = "postgres://postgres:postgres@localhost:5432/postgres"

// Config is the resolved persistence settings (pgx pool DSN + crypto key).
type Config struct {
	DSN              string
	EncryptionKeyHex string
}

// EncryptionKey decodes the hex-encoded key into raw bytes; it errors when
// the value does not decode to exactly 32 bytes.
func (c Config) EncryptionKey() ([]byte, error) {
	raw, err := hex.DecodeString(c.EncryptionKeyHex)
	if err != nil {
		return nil, fmt.Errorf("DB_ENCRYPTION_KEY is not valid hex: %w", err)
	}
	if len(raw) != KeyLen {
		return nil, fmt.Errorf(
			"DB_ENCRYPTION_KEY must decode to exactly %d bytes (got %d bytes from %d hex chars)",
			KeyLen, len(raw), len(c.EncryptionKeyHex),
		)
	}
	return raw, nil
}

// NewFromEnv builds a Config from the process environment. Equivalent to
// the Python from_env() helper.
func NewFromEnv() (Config, error) {
	return NewFromMap(envMap())
}

// NewFromMap builds a Config from an arbitrary string map (for tests).
func NewFromMap(env map[string]string) (Config, error) {
	dsn := env["DB_DSN"]
	if dsn == "" {
		dsn = DefaultDSN
	}
	key := env["DB_ENCRYPTION_KEY"]
	if key == "" {
		return Config{}, fmt.Errorf(
			"DB_ENCRYPTION_KEY is required (32-byte hex string). " +
				"For dev, set it in your shell or .env: " +
				"`export DB_ENCRYPTION_KEY=$(openssl rand -hex 32)`",
		)
	}
	cfg := Config{DSN: dsn, EncryptionKeyHex: key}
	if _, err := cfg.EncryptionKey(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func envMap() map[string]string {
	out := make(map[string]string, 2)
	for _, k := range []string{"DB_DSN", "DB_ENCRYPTION_KEY"} {
		if v, ok := os.LookupEnv(k); ok {
			out[k] = v
		}
	}
	return out
}
