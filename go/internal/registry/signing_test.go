package registry_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openclaw/agent-platform/internal/registry"
)

// generateEd25519Key creates a fresh key pair for tests.
func generateEd25519Key(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey, string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	pubDER, err := x509.MarshalPKIXPublicKey(pub)
	require.NoError(t, err)
	pubPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}))
	return pub, priv, pubPEM
}

func signPayload(t *testing.T, priv ed25519.PrivateKey, payload []byte) []byte {
	t.Helper()
	sig := ed25519.Sign(priv, payload)
	return sig
}

// ---------------------------------------------------------------------------
// InProcessVerifier tests.
// ---------------------------------------------------------------------------

func TestInProcessVerifier_ValidSignature(t *testing.T) {
	_, priv, pubPEM := generateEd25519Key(t)
	payload := []byte("the tarball bytes")
	sig := signPayload(t, priv, payload)

	v := &registry.InProcessVerifier{}
	ok, err := v.Verify(payload, sig, pubPEM)
	require.NoError(t, err)
	assert.True(t, ok, "valid signature should verify")
}

func TestInProcessVerifier_TamperedPayload(t *testing.T) {
	_, priv, pubPEM := generateEd25519Key(t)
	payload := []byte("original")
	sig := signPayload(t, priv, payload)

	v := &registry.InProcessVerifier{}
	ok, err := v.Verify([]byte("tampered"), sig, pubPEM)
	require.NoError(t, err)
	assert.False(t, ok, "signature over tampered payload must not verify")
}

func TestInProcessVerifier_WrongKey(t *testing.T) {
	_, priv, _ := generateEd25519Key(t)
	_, _, wrongPEM := generateEd25519Key(t) // different key pair

	payload := []byte("payload")
	sig := signPayload(t, priv, payload)

	v := &registry.InProcessVerifier{}
	ok, err := v.Verify(payload, sig, wrongPEM)
	require.NoError(t, err)
	assert.False(t, ok, "signature verified against wrong key must fail")
}

func TestInProcessVerifier_MalformedPEM(t *testing.T) {
	v := &registry.InProcessVerifier{}
	ok, err := v.Verify([]byte("data"), []byte("sig"), "not-a-pem")
	require.NoError(t, err) // implementation returns false, not error
	assert.False(t, ok)
}

func TestInProcessVerifier_EmptySignature(t *testing.T) {
	_, _, pubPEM := generateEd25519Key(t)
	v := &registry.InProcessVerifier{}
	ok, err := v.Verify([]byte("payload"), []byte{}, pubPEM)
	require.NoError(t, err)
	assert.False(t, ok)
}

// ---------------------------------------------------------------------------
// AlwaysAcceptVerifier tests.
// ---------------------------------------------------------------------------

func TestAlwaysAcceptVerifier(t *testing.T) {
	v := &registry.AlwaysAcceptVerifier{}
	ok, err := v.Verify(nil, nil, "")
	require.NoError(t, err)
	assert.True(t, ok)
}

// ---------------------------------------------------------------------------
// MakeVerifier tests.
// ---------------------------------------------------------------------------

func TestMakeVerifier_InProcess(t *testing.T) {
	v, algo, err := registry.MakeVerifier("inprocess", false)
	require.NoError(t, err)
	assert.Equal(t, "ed25519", algo)
	assert.IsType(t, &registry.InProcessVerifier{}, v)
}

func TestMakeVerifier_AlwaysAccept_RequiresFlag(t *testing.T) {
	_, _, err := registry.MakeVerifier("always-accept", false)
	assert.Error(t, err)
}

func TestMakeVerifier_AlwaysAccept_Allowed(t *testing.T) {
	v, algo, err := registry.MakeVerifier("always-accept", true)
	require.NoError(t, err)
	assert.Equal(t, "always-accept", algo)
	assert.IsType(t, &registry.AlwaysAcceptVerifier{}, v)
}

func TestMakeVerifier_Cosign(t *testing.T) {
	v, algo, err := registry.MakeVerifier("cosign", false)
	require.NoError(t, err)
	assert.Equal(t, "sigstore-cosign", algo)
	assert.IsType(t, &registry.CosignSubprocessVerifier{}, v)
}

func TestMakeVerifier_Unknown(t *testing.T) {
	_, _, err := registry.MakeVerifier("banana", false)
	assert.Error(t, err)
}
