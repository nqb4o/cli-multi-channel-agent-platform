package storage_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openclaw/agent-platform/internal/registry/storage"
)

func newTestMeta() *storage.InMemoryMetadataStore {
	return storage.NewInMemoryMetadataStore()
}

func seedPublisher(t *testing.T, meta *storage.InMemoryMetadataStore, handle string) *storage.PublisherRow {
	t.Helper()
	row, err := meta.UpsertPublisher(context.Background(), handle, "Test Publisher", "PEM_PLACEHOLDER", "token-hash-"+handle)
	require.NoError(t, err)
	return row
}

func TestInMemoryMetadataStore_UpsertAndGetPublisher(t *testing.T) {
	meta := newTestMeta()
	ctx := context.Background()

	row, err := meta.UpsertPublisher(ctx, "acme", "Acme Corp", "PEMDATA", "sha256tok")
	require.NoError(t, err)
	assert.Equal(t, "acme", row.Handle)
	assert.Equal(t, "Acme Corp", row.DisplayName)
	assert.Equal(t, "PEMDATA", row.PublicKeyPEM)

	byHandle, err := meta.GetPublisherByHandle(ctx, "acme")
	require.NoError(t, err)
	assert.Equal(t, row.ID, byHandle.ID)

	byToken, err := meta.GetPublisherByTokenHash(ctx, "sha256tok")
	require.NoError(t, err)
	assert.Equal(t, row.ID, byToken.ID)
}

func TestInMemoryMetadataStore_UpsertPublisherRotatesToken(t *testing.T) {
	meta := newTestMeta()
	ctx := context.Background()
	_, err := meta.UpsertPublisher(ctx, "acme", "", "PEM", "old-token")
	require.NoError(t, err)

	// Rotate token.
	_, err = meta.UpsertPublisher(ctx, "acme", "", "PEM", "new-token")
	require.NoError(t, err)

	// Old token must no longer work.
	old, _ := meta.GetPublisherByTokenHash(ctx, "old-token")
	assert.Nil(t, old)

	// New token works.
	fresh, _ := meta.GetPublisherByTokenHash(ctx, "new-token")
	require.NotNil(t, fresh)
}

func TestInMemoryMetadataStore_GetPublisherMissing(t *testing.T) {
	meta := newTestMeta()
	ctx := context.Background()
	row, err := meta.GetPublisherByHandle(ctx, "ghost")
	require.NoError(t, err)
	assert.Nil(t, row)
}

func TestInMemoryMetadataStore_InsertAndGetRelease(t *testing.T) {
	meta := newTestMeta()
	ctx := context.Background()
	seedPublisher(t, meta, "platform")

	p := storage.InsertReleaseParams{
		Slug:               "web-search",
		PublisherHandle:    "platform",
		Version:            "0.1.0",
		ManifestDigest:     "aaaa1234" + fmt.Sprintf("%056x", 0),
		BlobSizeBytes:      512,
		ManifestJSON:       map[string]any{"name": "web-search"},
		Description:        "Web search skill",
		Summary:            "Search the web",
		Tags:               []string{"web", "search"},
		Changelog:          "Initial release",
		SignatureB64:       "c2lnbmF0dXJl",
		SignatureKeyID:     "platform",
		SignatureAlgorithm: "ed25519",
	}
	rel, err := meta.InsertRelease(ctx, p)
	require.NoError(t, err)
	assert.Equal(t, "web-search", rel.Slug)
	assert.Equal(t, "0.1.0", rel.Version)
	assert.Equal(t, "c2lnbmF0dXJl", rel.SignatureB64)

	got, err := meta.GetRelease(ctx, "web-search", "0.1.0")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, rel.ID, got.ID)
}

func TestInMemoryMetadataStore_InsertRelease_ConflictVersion(t *testing.T) {
	meta := newTestMeta()
	ctx := context.Background()
	seedPublisher(t, meta, "platform")

	p := storage.InsertReleaseParams{
		Slug: "dup-skill", PublisherHandle: "platform", Version: "1.0.0",
		ManifestDigest: "deadbeef" + fmt.Sprintf("%056x", 0),
		BlobSizeBytes:  1, ManifestJSON: map[string]any{},
		SignatureB64: "sig", SignatureKeyID: "platform", SignatureAlgorithm: "ed25519",
	}
	_, err := meta.InsertRelease(ctx, p)
	require.NoError(t, err)

	_, err = meta.InsertRelease(ctx, p)
	assert.Error(t, err, "duplicate version should fail")
	assert.Contains(t, err.Error(), "already exists")
}

func TestInMemoryMetadataStore_InsertRelease_WrongPublisher(t *testing.T) {
	meta := newTestMeta()
	ctx := context.Background()
	seedPublisher(t, meta, "alice")
	seedPublisher(t, meta, "bob")

	p := storage.InsertReleaseParams{
		Slug: "contested", PublisherHandle: "alice", Version: "1.0.0",
		ManifestDigest: "cafebabe" + fmt.Sprintf("%056x", 0),
		BlobSizeBytes:  1, ManifestJSON: map[string]any{},
		SignatureB64: "sig", SignatureKeyID: "alice", SignatureAlgorithm: "ed25519",
	}
	_, err := meta.InsertRelease(ctx, p)
	require.NoError(t, err)

	// Bob tries to claim the same slug.
	p.PublisherHandle = "bob"
	p.Version = "2.0.0"
	_, err = meta.InsertRelease(ctx, p)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "different publisher")
}

func TestInMemoryMetadataStore_InsertRelease_UnknownPublisher(t *testing.T) {
	meta := newTestMeta()
	ctx := context.Background()

	p := storage.InsertReleaseParams{
		Slug: "orphan", PublisherHandle: "ghost", Version: "1.0.0",
		ManifestDigest: fmt.Sprintf("%064x", 0),
		BlobSizeBytes:  1, ManifestJSON: map[string]any{},
		SignatureB64: "sig", SignatureKeyID: "ghost", SignatureAlgorithm: "ed25519",
	}
	_, err := meta.InsertRelease(ctx, p)
	assert.Error(t, err)
}

func TestInMemoryMetadataStore_SearchPackages(t *testing.T) {
	meta := newTestMeta()
	ctx := context.Background()
	seedPublisher(t, meta, "platform")

	for idx, slug := range []string{"trend-analysis", "web-search", "summarize-url"} {
		_, err := meta.InsertRelease(ctx, storage.InsertReleaseParams{
			Slug:            slug,
			PublisherHandle: "platform",
			Version:         "0.1.0",
			ManifestDigest:  fmt.Sprintf("%064x", idx+100),
			BlobSizeBytes:   1,
			ManifestJSON:    map[string]any{},
			Description:     slug + " description",
			Summary:         slug + " summary",
			SignatureB64:    "sig", SignatureKeyID: "platform", SignatureAlgorithm: "ed25519",
		})
		require.NoError(t, err)
	}

	// Empty query → all results.
	results, err := meta.SearchPackages(ctx, "", 100)
	require.NoError(t, err)
	assert.Len(t, results, 3)

	// Prefix query.
	results, err = meta.SearchPackages(ctx, "trend", 100)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "trend-analysis", results[0].Slug)

	// Summary substring match.
	results, err = meta.SearchPackages(ctx, "summarize", 100)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "summarize-url", results[0].Slug)

	// No match.
	results, err = meta.SearchPackages(ctx, "nonexistent", 100)
	require.NoError(t, err)
	assert.Len(t, results, 0)
}

func TestInMemoryMetadataStore_SearchRespectLimit(t *testing.T) {
	meta := newTestMeta()
	ctx := context.Background()
	seedPublisher(t, meta, "platform")

	for i := 0; i < 5; i++ {
		slug := fmt.Sprintf("skill-%c", rune('a'+i))
		digest := fmt.Sprintf("%064x", i+1) // valid 64-char hex
		_, _ = meta.InsertRelease(ctx, storage.InsertReleaseParams{
			Slug:            slug,
			PublisherHandle: "platform",
			Version:         "1.0.0",
			ManifestDigest:  digest,
			BlobSizeBytes:   1,
			ManifestJSON:    map[string]any{},
			SignatureB64:    "sig", SignatureKeyID: "platform", SignatureAlgorithm: "ed25519",
		})
	}

	results, err := meta.SearchPackages(ctx, "", 3)
	require.NoError(t, err)
	assert.Len(t, results, 3)
}

func TestInMemoryMetadataStore_ListReleases(t *testing.T) {
	meta := newTestMeta()
	ctx := context.Background()
	seedPublisher(t, meta, "platform")

	for _, v := range []string{"0.1.0", "0.2.0", "1.0.0"} {
		_, err := meta.InsertRelease(ctx, storage.InsertReleaseParams{
			Slug: "multi-version", PublisherHandle: "platform", Version: v,
			ManifestDigest: fmt.Sprintf("%064x", len(v)+200),
			BlobSizeBytes:  1, ManifestJSON: map[string]any{},
			SignatureB64: "sig", SignatureKeyID: "platform", SignatureAlgorithm: "ed25519",
		})
		require.NoError(t, err)
	}

	releases, err := meta.ListReleases(ctx, "multi-version")
	require.NoError(t, err)
	assert.Len(t, releases, 3)
}

func TestInMemoryMetadataStore_GetPackage_LatestVersion(t *testing.T) {
	meta := newTestMeta()
	ctx := context.Background()
	seedPublisher(t, meta, "platform")

	for _, v := range []string{"0.1.0", "0.2.0"} {
		_, _ = meta.InsertRelease(ctx, storage.InsertReleaseParams{
			Slug: "versioned", PublisherHandle: "platform", Version: v,
			ManifestDigest: fmt.Sprintf("%064x", len(v)+300),
			BlobSizeBytes:  1, ManifestJSON: map[string]any{},
			SignatureB64: "sig", SignatureKeyID: "platform", SignatureAlgorithm: "ed25519",
		})
	}

	pkg, err := meta.GetPackage(ctx, "versioned")
	require.NoError(t, err)
	require.NotNil(t, pkg)
	assert.Equal(t, "0.2.0", pkg.LatestVersion)
}

func TestInMemoryMetadataStore_GetPackageMissing(t *testing.T) {
	meta := newTestMeta()
	ctx := context.Background()
	pkg, err := meta.GetPackage(ctx, "noexist")
	require.NoError(t, err)
	assert.Nil(t, pkg)
}

func TestInMemoryMetadataStore_GetReleaseMissing(t *testing.T) {
	meta := newTestMeta()
	ctx := context.Background()
	rel, err := meta.GetRelease(ctx, "ghost", "1.0.0")
	require.NoError(t, err)
	assert.Nil(t, rel)
}
