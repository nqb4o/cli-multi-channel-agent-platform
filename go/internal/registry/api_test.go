package registry_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openclaw/agent-platform/internal/registry"
	"github.com/openclaw/agent-platform/internal/registry/storage"
)

// ---------------------------------------------------------------------------
// Test fixtures.
// ---------------------------------------------------------------------------

type testServer struct {
	meta     *storage.InMemoryMetadataStore
	blobs    *storage.InMemoryBlobStore
	verifier registry.SignatureVerifier
	server   *httptest.Server
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()
	meta := storage.NewInMemoryMetadataStore()
	blobs := storage.NewInMemoryBlobStore()
	deps := &registry.Deps{
		Metadata:           meta,
		Blobs:              blobs,
		Verifier:           &registry.AlwaysAcceptVerifier{},
		SignatureAlgorithm: "always-accept",
	}
	srv := httptest.NewServer(registry.NewRouter(deps))
	t.Cleanup(srv.Close)
	return &testServer{meta: meta, blobs: blobs, verifier: &registry.AlwaysAcceptVerifier{}, server: srv}
}

func (ts *testServer) url(path string) string { return ts.server.URL + path }

func (ts *testServer) seedPublisher(t *testing.T, handle, token string) {
	t.Helper()
	hash := registry.HashToken(token)
	_, err := ts.meta.UpsertPublisher(context.Background(), handle, handle+" Publisher", "PEM", hash)
	require.NoError(t, err)
}

func (ts *testServer) publishSkill(t *testing.T, token, slug, version string) {
	t.Helper()
	tarball := makeTarball(t, slug, version)
	sig := []byte("fake-signature")

	body := &bytes.Buffer{}
	w := multipart.NewWriter(body)
	part, _ := w.CreateFormFile("tarball", "skill.tar.gz")
	_, _ = part.Write(tarball)
	sigPart, _ := w.CreateFormFile("signature", "skill.sig")
	_, _ = sigPart.Write(sig)
	_ = w.WriteField("metadata", `{"changelog": "test release"}`)
	w.Close()

	req, err := http.NewRequest("POST", ts.url("/skills/"+slug+"/versions"), body)
	require.NoError(t, err)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode, "publish should return 201")
}

// makeTarball creates a minimal gzip tarball with a valid SKILL.md for the given slug/version.
func makeTarball(t *testing.T, slug, version string) []byte {
	t.Helper()
	skillMD := fmt.Sprintf(`---
name: %s
version: %s
description: Test skill for %s
when_to_use: Use for testing.
---
# %s

Test skill body.
`, slug, version, slug, slug)

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	hdr := &tar.Header{
		Name:     "SKILL.md",
		Mode:     0o644,
		Size:     int64(len(skillMD)),
		Typeflag: tar.TypeReg,
	}
	require.NoError(t, tw.WriteHeader(hdr))
	_, err := io.WriteString(tw, skillMD)
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	require.NoError(t, gw.Close())
	return buf.Bytes()
}

func decodeJSONBody(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	var body map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	return body
}

// ---------------------------------------------------------------------------
// GET /healthz.
// ---------------------------------------------------------------------------

func TestAPI_Healthz(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Get(ts.url("/healthz"))
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	body := decodeJSONBody(t, resp)
	assert.Equal(t, "ok", body["status"])
}

// ---------------------------------------------------------------------------
// GET /skills — search.
// ---------------------------------------------------------------------------

func TestAPI_Search_EmptyRegistry(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Get(ts.url("/skills?q=trend"))
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	body := decodeJSONBody(t, resp)
	results, ok := body["results"].([]any)
	require.True(t, ok)
	assert.Len(t, results, 0)
}

func TestAPI_Search_ReturnsMatchingSkills(t *testing.T) {
	ts := newTestServer(t)
	ts.seedPublisher(t, "platform", "tok")
	ts.publishSkill(t, "tok", "trend-analysis", "0.1.0")
	ts.publishSkill(t, "tok", "web-search", "0.1.0")

	resp, err := http.Get(ts.url("/skills?q=trend"))
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	body := decodeJSONBody(t, resp)
	results := body["results"].([]any)
	require.Len(t, results, 1)
	r := results[0].(map[string]any)
	assert.Equal(t, "trend-analysis", r["slug"])
}

func TestAPI_Search_LimitCapped(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Get(ts.url("/skills?q=&limit=200"))
	require.NoError(t, err)
	body := decodeJSONBody(t, resp)
	assert.EqualValues(t, 100, body["limit"])
}

// ---------------------------------------------------------------------------
// GET /skills/{slug}.
// ---------------------------------------------------------------------------

func TestAPI_PackageDetail_NotFound(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Get(ts.url("/skills/ghost"))
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestAPI_PackageDetail_Found(t *testing.T) {
	ts := newTestServer(t)
	ts.seedPublisher(t, "platform", "tok")
	ts.publishSkill(t, "tok", "web-search", "0.1.0")

	resp, err := http.Get(ts.url("/skills/web-search"))
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	body := decodeJSONBody(t, resp)
	pkg := body["package"].(map[string]any)
	assert.Equal(t, "web-search", pkg["slug"])
	versions := body["versions"].([]any)
	assert.Len(t, versions, 1)
}

// ---------------------------------------------------------------------------
// GET /skills/{slug}/versions/{version}.
// ---------------------------------------------------------------------------

func TestAPI_ReleaseDetail_NotFound(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Get(ts.url("/skills/ghost/versions/0.1.0"))
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestAPI_ReleaseDetail_Found(t *testing.T) {
	ts := newTestServer(t)
	ts.seedPublisher(t, "platform", "tok")
	ts.publishSkill(t, "tok", "web-search", "0.1.0")

	resp, err := http.Get(ts.url("/skills/web-search/versions/0.1.0"))
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	body := decodeJSONBody(t, resp)
	rel := body["release"].(map[string]any)
	assert.Equal(t, "0.1.0", rel["version"])
}

// ---------------------------------------------------------------------------
// GET /skills/{slug}/versions/{version}/tarball.
// ---------------------------------------------------------------------------

func TestAPI_DownloadTarball_OK(t *testing.T) {
	ts := newTestServer(t)
	ts.seedPublisher(t, "platform", "tok")
	ts.publishSkill(t, "tok", "web-search", "0.1.0")

	resp, err := http.Get(ts.url("/skills/web-search/versions/0.1.0/tarball"))
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	defer resp.Body.Close()

	// Verify Docker-Content-Digest header.
	digestHeader := resp.Header.Get("Docker-Content-Digest")
	assert.True(t, len(digestHeader) > 7 && digestHeader[:7] == "sha256:", "should have sha256 digest header")

	data, _ := io.ReadAll(resp.Body)
	assert.NotEmpty(t, data)
}

func TestAPI_DownloadTarball_NotFound(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Get(ts.url("/skills/ghost/versions/9.9.9/tarball"))
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// ---------------------------------------------------------------------------
// GET /skills/{slug}/versions/{version}/sig.
// ---------------------------------------------------------------------------

func TestAPI_Signature_OK(t *testing.T) {
	ts := newTestServer(t)
	ts.seedPublisher(t, "platform", "tok")
	ts.publishSkill(t, "tok", "web-search", "0.1.0")

	resp, err := http.Get(ts.url("/skills/web-search/versions/0.1.0/sig"))
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	body := decodeJSONBody(t, resp)
	assert.NotEmpty(t, body["signature_b64"])
	assert.NotEmpty(t, body["key_id"])
}

func TestAPI_Signature_NotFound(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Get(ts.url("/skills/ghost/versions/1.0.0/sig"))
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// ---------------------------------------------------------------------------
// GET /keys/{publisher}.
// ---------------------------------------------------------------------------

func TestAPI_PublisherKey_Found(t *testing.T) {
	ts := newTestServer(t)
	ts.seedPublisher(t, "acme", "tok")

	resp, err := http.Get(ts.url("/keys/acme"))
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	body := decodeJSONBody(t, resp)
	assert.Equal(t, "acme", body["handle"])
	assert.NotEmpty(t, body["public_key_pem"])
}

func TestAPI_PublisherKey_NotFound(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Get(ts.url("/keys/ghost"))
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// ---------------------------------------------------------------------------
// POST /skills/{slug}/versions — publish.
// ---------------------------------------------------------------------------

func TestAPI_Publish_OK(t *testing.T) {
	ts := newTestServer(t)
	ts.seedPublisher(t, "platform", "my-token")
	ts.publishSkill(t, "my-token", "trend-analysis", "0.1.0")

	// Verify the release is now queryable.
	resp, err := http.Get(ts.url("/skills/trend-analysis/versions/0.1.0"))
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestAPI_Publish_Unauthenticated(t *testing.T) {
	ts := newTestServer(t)
	tarball := makeTarball(t, "my-skill", "1.0.0")

	body := &bytes.Buffer{}
	w := multipart.NewWriter(body)
	part, _ := w.CreateFormFile("tarball", "skill.tar.gz")
	_, _ = part.Write(tarball)
	w.Close()

	req, _ := http.NewRequest("POST", ts.url("/skills/my-skill/versions"), body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	// No Authorization header.

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestAPI_Publish_WrongToken(t *testing.T) {
	ts := newTestServer(t)
	ts.seedPublisher(t, "platform", "real-token")
	tarball := makeTarball(t, "test-skill", "1.0.0")

	body := &bytes.Buffer{}
	w := multipart.NewWriter(body)
	part, _ := w.CreateFormFile("tarball", "skill.tar.gz")
	_, _ = part.Write(tarball)
	sigPart, _ := w.CreateFormFile("signature", "skill.sig")
	_, _ = sigPart.Write([]byte("fake-sig"))
	w.Close()

	req, _ := http.NewRequest("POST", ts.url("/skills/test-skill/versions"), body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Authorization", "Bearer wrong-token")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestAPI_Publish_MissingTarball(t *testing.T) {
	ts := newTestServer(t)
	ts.seedPublisher(t, "platform", "tok")

	body := &bytes.Buffer{}
	w := multipart.NewWriter(body)
	_ = w.WriteField("signature", "c2ln")
	w.Close()

	req, _ := http.NewRequest("POST", ts.url("/skills/my-skill/versions"), body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Authorization", "Bearer tok")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestAPI_Publish_SlugMismatch(t *testing.T) {
	ts := newTestServer(t)
	ts.seedPublisher(t, "platform", "tok")

	// Tarball says "web-search" but URL slug is "other-skill".
	tarball := makeTarball(t, "web-search", "1.0.0")

	body := &bytes.Buffer{}
	w := multipart.NewWriter(body)
	part, _ := w.CreateFormFile("tarball", "skill.tar.gz")
	_, _ = part.Write(tarball)
	sigPart, _ := w.CreateFormFile("signature", "skill.sig")
	_, _ = sigPart.Write([]byte("sig"))
	w.Close()

	req, _ := http.NewRequest("POST", ts.url("/skills/other-skill/versions"), body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Authorization", "Bearer tok")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	respBody := decodeJSONBody(t, resp)
	assert.Equal(t, "slug_mismatch", respBody["error"])
}

func TestAPI_Publish_DuplicateVersion(t *testing.T) {
	ts := newTestServer(t)
	ts.seedPublisher(t, "platform", "tok")
	ts.publishSkill(t, "tok", "web-search", "0.1.0")

	// Publish again — same version.
	tarball := makeTarball(t, "web-search", "0.1.0")
	body := &bytes.Buffer{}
	w := multipart.NewWriter(body)
	part, _ := w.CreateFormFile("tarball", "skill.tar.gz")
	_, _ = part.Write(tarball)
	sigPart, _ := w.CreateFormFile("signature", "skill.sig")
	_, _ = sigPart.Write([]byte("sig"))
	w.Close()

	req, _ := http.NewRequest("POST", ts.url("/skills/web-search/versions"), body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Authorization", "Bearer tok")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	assert.Equal(t, http.StatusConflict, resp.StatusCode)
}

func TestAPI_Publish_InvalidSignature_InProcessVerifier(t *testing.T) {
	// Use a real Ed25519 verifier.
	meta := storage.NewInMemoryMetadataStore()
	blobs := storage.NewInMemoryBlobStore()

	pub, priv, pubPEM := generateEd25519Key(t)
	_ = pub
	token := "real-token"
	hash := registry.HashToken(token)
	_, err := meta.UpsertPublisher(context.Background(), "platform", "Platform", pubPEM, hash)
	require.NoError(t, err)

	deps := &registry.Deps{
		Metadata:           meta,
		Blobs:              blobs,
		Verifier:           &registry.InProcessVerifier{},
		SignatureAlgorithm: "ed25519",
	}
	srv := httptest.NewServer(registry.NewRouter(deps))
	defer srv.Close()

	tarball := makeTarball(t, "real-skill", "1.0.0")
	// Wrong signature — sign different data; b64-encode for wire format.
	wrongRawSig := ed25519.Sign(priv, []byte("not the tarball"))
	wrongSigB64 := base64.StdEncoding.EncodeToString(wrongRawSig)

	body := &bytes.Buffer{}
	w := multipart.NewWriter(body)
	part, _ := w.CreateFormFile("tarball", "skill.tar.gz")
	_, _ = part.Write(tarball)
	sigPart, _ := w.CreateFormFile("signature", "skill.sig")
	_, _ = io.WriteString(sigPart, wrongSigB64)
	w.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/skills/real-skill/versions", body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err2 := http.DefaultClient.Do(req)
	require.NoError(t, err2)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	respBody := decodeJSONBody(t, resp)
	assert.Equal(t, "invalid_signature", respBody["error"])
}

func TestAPI_Publish_ValidSignature_InProcessVerifier(t *testing.T) {
	meta := storage.NewInMemoryMetadataStore()
	blobs := storage.NewInMemoryBlobStore()

	_, priv, pubPEM := generateEd25519Key(t)
	token := "valid-token"
	hash := registry.HashToken(token)
	_, err := meta.UpsertPublisher(context.Background(), "platform", "Platform", pubPEM, hash)
	require.NoError(t, err)

	deps := &registry.Deps{
		Metadata:           meta,
		Blobs:              blobs,
		Verifier:           &registry.InProcessVerifier{},
		SignatureAlgorithm: "ed25519",
	}
	srv := httptest.NewServer(registry.NewRouter(deps))
	defer srv.Close()

	tarball := makeTarball(t, "signed-skill", "1.0.0")
	rawSig := ed25519.Sign(priv, tarball)
	// The Python CLI uploads base64(raw_sig) as the signature part (octet-stream).
	// DecodeSignatureField decodes it back to raw bytes.
	sigB64 := base64.StdEncoding.EncodeToString(rawSig)

	body := &bytes.Buffer{}
	w := multipart.NewWriter(body)
	part, _ := w.CreateFormFile("tarball", "skill.tar.gz")
	_, _ = part.Write(tarball)
	sigPart, _ := w.CreateFormFile("signature", "skill.sig")
	_, _ = io.WriteString(sigPart, sigB64)
	w.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/skills/signed-skill/versions", body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err2 := http.DefaultClient.Do(req)
	require.NoError(t, err2)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusCreated, resp.StatusCode, "valid Ed25519 signature should be accepted")
}

// generateEd25519Key is also used here; declared in signing_test.go (same package).
// rand is already imported above.
var _ = rand.Reader
