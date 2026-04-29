// Package registry is the F13 Skill Registry — chi router, auth, publish pipeline,
// signing, and storage. The factory pattern (New / NewFromEnv) mirrors the Python
// create_app / run split so tests pass in-memory fakes without touching the network.
package registry

import (
	"os"
	"strconv"
)

// Config holds all registry runtime knobs.
//
// All fields have sensible zero-value defaults: in-memory stores are
// selected automatically when the DSN / blob dir are empty.
type Config struct {
	// DSN for the Postgres-backed metadata store.
	// Empty → InMemoryMetadataStore.
	DSN string

	// BlobDir is the filesystem root for the content-addressed blob store.
	// Empty → InMemoryBlobStore.
	BlobDir string

	// VerifierKind selects the SignatureVerifier: "inprocess" (default),
	// "cosign", or "always-accept" (requires AllowInsecureVerifier=true).
	VerifierKind string

	// AllowInsecureVerifier gates the "always-accept" verifier. Must be
	// explicitly set to true in non-test callers.
	AllowInsecureVerifier bool

	// Host / Port for the HTTP listener (production only).
	Host string
	Port int
}

// LoadConfigFromEnv reads registry config from environment variables.
//
//	REGISTRY_DSN              → Config.DSN
//	REGISTRY_BLOB_DIR         → Config.BlobDir
//	REGISTRY_VERIFIER         → Config.VerifierKind (default: "inprocess")
//	REGISTRY_ALLOW_INSECURE_VERIFIER → "1" enables always-accept
//	REGISTRY_HOST             → Config.Host       (default: "127.0.0.1")
//	REGISTRY_PORT             → Config.Port       (default: 8090)
func LoadConfigFromEnv() Config {
	port := 8090
	if v := os.Getenv("REGISTRY_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			port = p
		}
	}
	host := os.Getenv("REGISTRY_HOST")
	if host == "" {
		host = "127.0.0.1"
	}
	verifierKind := os.Getenv("REGISTRY_VERIFIER")
	if verifierKind == "" {
		verifierKind = "inprocess"
	}
	return Config{
		DSN:                   os.Getenv("REGISTRY_DSN"),
		BlobDir:               os.Getenv("REGISTRY_BLOB_DIR"),
		VerifierKind:          verifierKind,
		AllowInsecureVerifier: os.Getenv("REGISTRY_ALLOW_INSECURE_VERIFIER") == "1",
		Host:                  host,
		Port:                  port,
	}
}
