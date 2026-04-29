package registry_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openclaw/agent-platform/internal/registry"
	"github.com/openclaw/agent-platform/internal/registry/storage"
)

func setupPublishDeps(t *testing.T) (*storage.InMemoryMetadataStore, *storage.InMemoryBlobStore, *registry.Publisher) {
	t.Helper()
	meta := storage.NewInMemoryMetadataStore()
	blobs := storage.NewInMemoryBlobStore()
	_, err := meta.UpsertPublisher(context.Background(), "platform", "Platform", "PEM", "tok-hash")
	require.NoError(t, err)
	pub := &registry.Publisher{Handle: "platform", PublicKeyPEM: "PEM"}
	return meta, blobs, pub
}

func TestPublishRelease_OK(t *testing.T) {
	meta, blobs, pub := setupPublishDeps(t)
	tarball := makeTarball(t, "web-search", "0.1.0")
	req := &registry.PublishRequest{
		Slug:           "web-search",
		Publisher:      pub,
		TarballBytes:   tarball,
		SignatureBytes: []byte("sig"),
		Changelog:      "first release",
	}
	result, err := registry.PublishRelease(context.Background(), req, meta, blobs, &registry.AlwaysAcceptVerifier{}, "always-accept")
	require.NoError(t, err)
	assert.NotNil(t, result.Release)
	assert.Equal(t, "web-search", result.Release.Slug)
	assert.Equal(t, "0.1.0", result.Release.Version)
	assert.Len(t, result.ManifestDigest, 64)
	assert.True(t, blobs.Has(result.ManifestDigest), "blob should be stored after publish")
}

func TestPublishRelease_EmptyTarball(t *testing.T) {
	meta, blobs, pub := setupPublishDeps(t)
	req := &registry.PublishRequest{
		Slug:           "x",
		Publisher:      pub,
		TarballBytes:   []byte{},
		SignatureBytes: []byte("sig"),
	}
	_, err := registry.PublishRelease(context.Background(), req, meta, blobs, &registry.AlwaysAcceptVerifier{}, "always-accept")
	require.Error(t, err)
	var pe *registry.PublishError
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, "invalid_tarball", pe.Code)
}

func TestPublishRelease_TarballTooLarge(t *testing.T) {
	meta, blobs, pub := setupPublishDeps(t)
	big := make([]byte, registry.MaxTarballBytes+1)
	req := &registry.PublishRequest{
		Slug:           "x",
		Publisher:      pub,
		TarballBytes:   big,
		SignatureBytes: []byte("sig"),
	}
	_, err := registry.PublishRelease(context.Background(), req, meta, blobs, &registry.AlwaysAcceptVerifier{}, "always-accept")
	require.Error(t, err)
	var pe *registry.PublishError
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, "invalid_tarball", pe.Code)
}

func TestPublishRelease_InvalidSlug(t *testing.T) {
	meta, blobs, pub := setupPublishDeps(t)
	tarball := makeTarball(t, "web-search", "0.1.0")
	req := &registry.PublishRequest{
		Slug:           "INVALID SLUG!",
		Publisher:      pub,
		TarballBytes:   tarball,
		SignatureBytes: []byte("sig"),
	}
	_, err := registry.PublishRelease(context.Background(), req, meta, blobs, &registry.AlwaysAcceptVerifier{}, "always-accept")
	require.Error(t, err)
	var pe *registry.PublishError
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, "invalid_slug", pe.Code)
}

func TestPublishRelease_SlugMismatch(t *testing.T) {
	meta, blobs, pub := setupPublishDeps(t)
	tarball := makeTarball(t, "web-search", "0.1.0")
	req := &registry.PublishRequest{
		Slug:           "other-slug",
		Publisher:      pub,
		TarballBytes:   tarball,
		SignatureBytes: []byte("sig"),
	}
	_, err := registry.PublishRelease(context.Background(), req, meta, blobs, &registry.AlwaysAcceptVerifier{}, "always-accept")
	require.Error(t, err)
	var pe *registry.PublishError
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, "slug_mismatch", pe.Code)
}

func TestPublishRelease_InvalidSignature(t *testing.T) {
	meta, blobs, pub := setupPublishDeps(t)
	tarball := makeTarball(t, "web-search", "0.1.0")
	req := &registry.PublishRequest{
		Slug:           "web-search",
		Publisher:      pub,
		TarballBytes:   tarball,
		SignatureBytes: []byte("sig"),
	}
	// Use InProcessVerifier which will reject any signature against "PEM".
	_, err := registry.PublishRelease(context.Background(), req, meta, blobs, &registry.InProcessVerifier{}, "ed25519")
	require.Error(t, err)
	var pe *registry.PublishError
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, "invalid_signature", pe.Code)
}

func TestPublishRelease_ConflictVersion(t *testing.T) {
	meta, blobs, pub := setupPublishDeps(t)
	tarball := makeTarball(t, "web-search", "0.1.0")
	req := &registry.PublishRequest{
		Slug:           "web-search",
		Publisher:      pub,
		TarballBytes:   tarball,
		SignatureBytes: []byte("sig"),
	}
	_, err := registry.PublishRelease(context.Background(), req, meta, blobs, &registry.AlwaysAcceptVerifier{}, "always-accept")
	require.NoError(t, err)

	// Second publish of same version.
	_, err = registry.PublishRelease(context.Background(), req, meta, blobs, &registry.AlwaysAcceptVerifier{}, "always-accept")
	require.Error(t, err)
	var pe *registry.PublishError
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, "conflict", pe.Code)
}

func TestPublishRelease_NoSKILLmd(t *testing.T) {
	meta, blobs, pub := setupPublishDeps(t)
	tarball := makeEmptyTarball(t)
	req := &registry.PublishRequest{
		Slug:           "missing-md",
		Publisher:      pub,
		TarballBytes:   tarball,
		SignatureBytes: []byte("sig"),
	}
	_, err := registry.PublishRelease(context.Background(), req, meta, blobs, &registry.AlwaysAcceptVerifier{}, "always-accept")
	require.Error(t, err)
	var pe *registry.PublishError
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, "invalid_tarball", pe.Code)
}

func TestDecodeSignatureField_Hex(t *testing.T) {
	hexSig := "deadbeef"
	got := registry.DecodeSignatureField(hexSig)
	assert.Equal(t, []byte{0xde, 0xad, 0xbe, 0xef}, got)
}

func TestDecodeSignatureField_Base64(t *testing.T) {
	// "hello" base64-encoded = "aGVsbG8="
	got := registry.DecodeSignatureField("aGVsbG8=")
	assert.Equal(t, []byte("hello"), got)
}

func TestDecodeSignatureField_Fallback(t *testing.T) {
	// Not valid hex or base64 → return UTF-8 bytes.
	got := registry.DecodeSignatureField("???not_valid???")
	assert.Equal(t, []byte("???not_valid???"), got)
}

// makeEmptyTarball creates a valid but empty gzipped tarball (no files).
func makeEmptyTarball(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	require.NoError(t, tw.Close())
	require.NoError(t, gw.Close())
	return buf.Bytes()
}
