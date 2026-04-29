package registry

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
)

// SignatureVerifier is the swappable signature-verification backend.
//
// All implementations must be safe to call concurrently from multiple
// goroutines. The method returns false (not an error) for an invalid
// signature so callers can produce a uniform 400 response regardless of
// which verifier is in use. An error return is reserved for unexpected
// infrastructure failures.
type SignatureVerifier interface {
	Verify(payload, signature []byte, publicKeyPEM string) (bool, error)
}

// MakeVerifier constructs the verifier named by kind.
//
// kind is one of "inprocess", "cosign", or "always-accept".
// The always-accept verifier requires allowInsecure=true or it panics —
// the production entry point enforces this so it can never be wired
// silently in a deployed binary.
func MakeVerifier(kind string, allowInsecure bool) (SignatureVerifier, string, error) {
	switch kind {
	case "inprocess", "":
		return &InProcessVerifier{}, "ed25519", nil
	case "cosign":
		return &CosignSubprocessVerifier{}, "sigstore-cosign", nil
	case "always-accept":
		if !allowInsecure {
			return nil, "", fmt.Errorf(
				"REGISTRY_VERIFIER=always-accept requires REGISTRY_ALLOW_INSECURE_VERIFIER=1",
			)
		}
		return &AlwaysAcceptVerifier{}, "always-accept", nil
	default:
		return nil, "", fmt.Errorf("unknown REGISTRY_VERIFIER %q", kind)
	}
}

// ---------------------------------------------------------------------------
// InProcessVerifier — Ed25519, no subprocess.
// ---------------------------------------------------------------------------

// InProcessVerifier verifies Ed25519 detached signatures using the Go stdlib.
// It is the default for tests and bootstrap publishers.
type InProcessVerifier struct{}

// Verify returns true iff signature is a valid Ed25519 sig over payload
// produced by the private key paired with publicKeyPEM.
func (v *InProcessVerifier) Verify(payload, signature []byte, publicKeyPEM string) (bool, error) {
	pub, err := parseEd25519PEM(publicKeyPEM)
	if err != nil {
		log.Printf("InProcessVerifier: bad PEM: %v", err)
		return false, nil // malformed key → not valid, not a server error
	}
	ok := ed25519.Verify(pub, payload, signature)
	return ok, nil
}

func parseEd25519PEM(pem_text string) (ed25519.PublicKey, error) {
	block, _ := pem.Decode([]byte(pem_text))
	if block == nil {
		return nil, fmt.Errorf("no PEM block found")
	}
	raw, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("ParsePKIXPublicKey: %w", err)
	}
	pub, ok := raw.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("key is not Ed25519 (got %T)", raw)
	}
	return pub, nil
}

// ---------------------------------------------------------------------------
// CosignSubprocessVerifier — production stub.
// ---------------------------------------------------------------------------

// CosignSubprocessVerifier shells out to "cosign verify-blob". It is a
// deliberate stub — the swap from InProcessVerifier to cosign is a single
// config flip when Sigstore support is fully wired.
//
// If the cosign binary is not on PATH every call returns false and a one-shot
// warning is emitted. Tests cover the happy + missing-binary paths.
type CosignSubprocessVerifier struct {
	CosignBin string // defaults to "cosign" via PATH

	warnOnce sync.Once
}

// Verify writes tmpfiles for the blob, signature, and key, then runs cosign.
func (v *CosignSubprocessVerifier) Verify(payload, signature []byte, publicKeyPEM string) (bool, error) {
	bin := v.CosignBin
	if bin == "" {
		bin = os.Getenv("COSIGN_BIN")
	}
	if bin == "" {
		bin = "cosign"
	}
	if _, err := exec.LookPath(bin); err != nil {
		v.warnOnce.Do(func() {
			log.Printf("CosignSubprocessVerifier: %q not found on PATH", bin)
		})
		return false, nil
	}

	tmp, err := os.MkdirTemp("", "cosign-verify-")
	if err != nil {
		return false, fmt.Errorf("cosign: mkdirtemp: %w", err)
	}
	defer os.RemoveAll(tmp)

	blobPath := filepath.Join(tmp, "blob")
	sigPath := filepath.Join(tmp, "blob.sig")
	keyPath := filepath.Join(tmp, "cosign.pub")
	if err := os.WriteFile(blobPath, payload, 0o600); err != nil {
		return false, err
	}
	if err := os.WriteFile(sigPath, signature, 0o600); err != nil {
		return false, err
	}
	if err := os.WriteFile(keyPath, []byte(publicKeyPEM), 0o600); err != nil {
		return false, err
	}

	cmd := exec.Command(bin, "verify-blob", "--key", keyPath, "--signature", sigPath, blobPath)
	if err := cmd.Run(); err != nil {
		return false, nil // cosign rejected — not a server error
	}
	return true, nil
}

// ---------------------------------------------------------------------------
// AlwaysAcceptVerifier — test fixture only.
// ---------------------------------------------------------------------------

// AlwaysAcceptVerifier accepts every signature. It must never be wired in
// production without REGISTRY_ALLOW_INSECURE_VERIFIER=1.
type AlwaysAcceptVerifier struct{}

// Verify always returns true.
func (v *AlwaysAcceptVerifier) Verify(_, _ []byte, _ string) (bool, error) {
	return true, nil
}
