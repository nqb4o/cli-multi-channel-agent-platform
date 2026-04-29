package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openclaw/agent-platform/internal/registry"
	"github.com/openclaw/agent-platform/internal/registry/storage"
)

// ---------------------------------------------------------------------------
// Test server helper — reuses internal/registry without a binary.
// ---------------------------------------------------------------------------

func newCLITestServer(t *testing.T) *httptest.Server {
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

	// Seed a publisher.
	_, err := meta.UpsertPublisher(context.Background(), "platform", "Platform Publisher", "PEM", registry.HashToken("test-token"))
	require.NoError(t, err)

	// Seed some skills.
	for _, slug := range []string{"trend-analysis", "web-search", "summarize-url"} {
		publishSkillToServer(t, srv.URL, slug, "0.1.0", "test-token")
	}
	// Also publish a second version of trend-analysis.
	publishSkillToServer(t, srv.URL, "trend-analysis", "0.2.0", "test-token")

	return srv
}

func publishSkillToServer(t *testing.T, serverURL, slug, version, token string) {
	t.Helper()
	tarball := buildTarball(t, slug, version)

	body := &bytes.Buffer{}
	w := multipart.NewWriter(body)
	part, _ := w.CreateFormFile("tarball", "skill.tar.gz")
	_, _ = part.Write(tarball)
	sigPart, _ := w.CreateFormFile("signature", "skill.sig")
	_, _ = sigPart.Write([]byte("fake-sig"))
	_ = w.WriteField("metadata", `{"changelog": "test"}`)
	w.Close()

	req, _ := http.NewRequest("POST", serverURL+"/skills/"+slug+"/versions", body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode,
		"seed publish should succeed for %s@%s", slug, version)
}

func buildTarball(t *testing.T, slug, version string) []byte {
	t.Helper()
	skillMD := fmt.Sprintf(`---
name: %s
version: %s
description: Test skill %s
when_to_use: Use for testing.
---
# %s
`, slug, version, slug, slug)

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	hdr := &tar.Header{Name: "SKILL.md", Mode: 0o644, Size: int64(len(skillMD)), Typeflag: tar.TypeReg}
	require.NoError(t, tw.WriteHeader(hdr))
	_, _ = io.WriteString(tw, skillMD)
	require.NoError(t, tw.Close())
	require.NoError(t, gw.Close())
	return buf.Bytes()
}

func setCLIEnv(t *testing.T, serverURL, skillsDir string) {
	t.Helper()
	t.Setenv("PLATFORM_REGISTRY_URL", serverURL)
	t.Setenv("PLATFORM_SKILLS_DIR", skillsDir)
	t.Setenv("PLATFORM_VERIFIER", "always-accept")
	t.Setenv("PLATFORM_TOKEN", "test-token")
}

// ---------------------------------------------------------------------------
// loadCLIConfig tests.
// ---------------------------------------------------------------------------

func TestLoadCLIConfig_Defaults(t *testing.T) {
	t.Setenv("PLATFORM_REGISTRY_URL", "")
	t.Setenv("PLATFORM_TOKEN", "")
	t.Setenv("PLATFORM_SKILLS_DIR", "")
	t.Setenv("PLATFORM_VERIFIER", "")
	cfg := loadCLIConfig()
	assert.Equal(t, "http://127.0.0.1:8090", cfg.RegistryURL)
	assert.Equal(t, "inprocess", cfg.VerifierKind)
	assert.NotEmpty(t, cfg.SkillsDir)
}

func TestLoadCLIConfig_EnvOverride(t *testing.T) {
	t.Setenv("PLATFORM_REGISTRY_URL", "http://example.com/")
	cfg := loadCLIConfig()
	assert.Equal(t, "http://example.com", cfg.RegistryURL) // trailing slash stripped
}

// ---------------------------------------------------------------------------
// registryClient tests.
// ---------------------------------------------------------------------------

func TestRegistryClient_Search(t *testing.T) {
	srv := newCLITestServer(t)
	skillsDir := t.TempDir()
	setCLIEnv(t, srv.URL, skillsDir)

	cfg := loadCLIConfig()
	client := newRegistryClient(cfg)

	results, err := client.search("trend", 25)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "trend-analysis", results[0]["slug"])
}

func TestRegistryClient_ListSkills(t *testing.T) {
	srv := newCLITestServer(t)
	setCLIEnv(t, srv.URL, t.TempDir())

	cfg := loadCLIConfig()
	client := newRegistryClient(cfg)

	results, err := client.listSkills(100)
	require.NoError(t, err)
	assert.Len(t, results, 3) // trend-analysis, web-search, summarize-url
}

func TestRegistryClient_GetPackage_Found(t *testing.T) {
	srv := newCLITestServer(t)
	setCLIEnv(t, srv.URL, t.TempDir())

	cfg := loadCLIConfig()
	client := newRegistryClient(cfg)

	body, err := client.getPackage("trend-analysis")
	require.NoError(t, err)
	pkg := body["package"].(map[string]any)
	assert.Equal(t, "trend-analysis", pkg["slug"])
	assert.Equal(t, "0.2.0", pkg["latest_version"])
}

func TestRegistryClient_GetPackage_NotFound(t *testing.T) {
	srv := newCLITestServer(t)
	setCLIEnv(t, srv.URL, t.TempDir())

	cfg := loadCLIConfig()
	client := newRegistryClient(cfg)

	_, err := client.getPackage("nonexistent-skill")
	require.Error(t, err)
	var rce *registryClientError
	require.ErrorAs(t, err, &rce)
	assert.Equal(t, 404, rce.Status)
}

func TestRegistryClient_GetRelease(t *testing.T) {
	srv := newCLITestServer(t)
	setCLIEnv(t, srv.URL, t.TempDir())

	cfg := loadCLIConfig()
	client := newRegistryClient(cfg)

	rel, err := client.getRelease("trend-analysis", "0.1.0")
	require.NoError(t, err)
	assert.Equal(t, "0.1.0", rel["version"])
}

func TestRegistryClient_GetSignature(t *testing.T) {
	srv := newCLITestServer(t)
	setCLIEnv(t, srv.URL, t.TempDir())

	cfg := loadCLIConfig()
	client := newRegistryClient(cfg)

	sig, err := client.getSignature("trend-analysis", "0.1.0")
	require.NoError(t, err)
	assert.NotEmpty(t, sig["signature_b64"])
}

func TestRegistryClient_DownloadTarball(t *testing.T) {
	srv := newCLITestServer(t)
	setCLIEnv(t, srv.URL, t.TempDir())

	cfg := loadCLIConfig()
	client := newRegistryClient(cfg)

	data, digest, err := client.downloadTarball("web-search", "0.1.0")
	require.NoError(t, err)
	assert.NotEmpty(t, data)
	assert.Len(t, digest, 64)
}

// ---------------------------------------------------------------------------
// installSkill tests.
// ---------------------------------------------------------------------------

func TestInstallSkill_OK(t *testing.T) {
	srv := newCLITestServer(t)
	skillsDir := t.TempDir()
	setCLIEnv(t, srv.URL, skillsDir)

	cfg := loadCLIConfig()
	client := newRegistryClient(cfg)

	skillDir, receipt, err := installSkill("web-search", client, cfg, "", false)
	require.NoError(t, err)
	assert.Equal(t, "web-search", receipt.Slug)
	assert.Equal(t, "0.1.0", receipt.Version)
	assert.DirExists(t, skillDir)

	// Receipt file must exist.
	receiptPath := filepath.Join(skillDir, receiptFilename)
	assert.FileExists(t, receiptPath)
}

func TestInstallSkill_PinnedVersion(t *testing.T) {
	srv := newCLITestServer(t)
	skillsDir := t.TempDir()
	setCLIEnv(t, srv.URL, skillsDir)

	cfg := loadCLIConfig()
	client := newRegistryClient(cfg)

	_, receipt, err := installSkill("trend-analysis", client, cfg, "0.1.0", false)
	require.NoError(t, err)
	assert.Equal(t, "0.1.0", receipt.Version)
}

func TestInstallSkill_LatestVersion(t *testing.T) {
	srv := newCLITestServer(t)
	skillsDir := t.TempDir()
	setCLIEnv(t, srv.URL, skillsDir)

	cfg := loadCLIConfig()
	client := newRegistryClient(cfg)

	// No version pin → should install 0.2.0.
	_, receipt, err := installSkill("trend-analysis", client, cfg, "", false)
	require.NoError(t, err)
	assert.Equal(t, "0.2.0", receipt.Version)
}

func TestInstallSkill_AlreadyInstalled_NoForce(t *testing.T) {
	srv := newCLITestServer(t)
	skillsDir := t.TempDir()
	setCLIEnv(t, srv.URL, skillsDir)

	cfg := loadCLIConfig()
	client := newRegistryClient(cfg)

	_, _, err := installSkill("web-search", client, cfg, "", false)
	require.NoError(t, err)

	_, _, err = installSkill("web-search", client, cfg, "", false)
	require.Error(t, err)
	var ie *installerError
	require.ErrorAs(t, err, &ie)
	assert.Equal(t, "already_installed", ie.Code)
}

func TestInstallSkill_Force(t *testing.T) {
	srv := newCLITestServer(t)
	skillsDir := t.TempDir()
	setCLIEnv(t, srv.URL, skillsDir)

	cfg := loadCLIConfig()
	client := newRegistryClient(cfg)

	_, _, err := installSkill("web-search", client, cfg, "", false)
	require.NoError(t, err)

	// Force re-install — must succeed.
	_, receipt, err := installSkill("web-search", client, cfg, "", true)
	require.NoError(t, err)
	assert.Equal(t, "web-search", receipt.Slug)
}

func TestInstallSkill_NotFound(t *testing.T) {
	srv := newCLITestServer(t)
	skillsDir := t.TempDir()
	setCLIEnv(t, srv.URL, skillsDir)

	cfg := loadCLIConfig()
	client := newRegistryClient(cfg)

	_, _, err := installSkill("ghost-skill", client, cfg, "", false)
	require.Error(t, err)
	var ie *installerError
	require.ErrorAs(t, err, &ie)
	assert.Equal(t, "not_found", ie.Code)
}

func TestInstallSkill_ReceiptContents(t *testing.T) {
	srv := newCLITestServer(t)
	skillsDir := t.TempDir()
	setCLIEnv(t, srv.URL, skillsDir)

	cfg := loadCLIConfig()
	client := newRegistryClient(cfg)

	skillDir, _, err := installSkill("web-search", client, cfg, "", false)
	require.NoError(t, err)

	raw, err := os.ReadFile(filepath.Join(skillDir, receiptFilename))
	require.NoError(t, err)
	var receipt map[string]any
	require.NoError(t, json.Unmarshal(raw, &receipt))

	assert.Equal(t, "web-search", receipt["slug"])
	assert.NotEmpty(t, receipt["version"])
	assert.NotEmpty(t, receipt["manifest_digest"])
	assert.EqualValues(t, 1, receipt["receipt_version"])
	assert.Equal(t, srv.URL, receipt["registry_url"])
}
