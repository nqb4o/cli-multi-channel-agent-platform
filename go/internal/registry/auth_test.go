package registry_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openclaw/agent-platform/internal/registry"
	"github.com/openclaw/agent-platform/internal/registry/storage"
)

func TestHashToken(t *testing.T) {
	// sha256("hello") = 2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824
	got := registry.HashToken("hello")
	assert.Equal(t, "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824", got)
}

func TestHashToken_StripsWhitespace(t *testing.T) {
	got := registry.HashToken("  hello  ")
	assert.Equal(t, registry.HashToken("hello"), got)
}

func TestRequirePublisher_OK(t *testing.T) {
	meta := storage.NewInMemoryMetadataStore()
	ctx := context.Background()

	token := "my-secret-token"
	hash := registry.HashToken(token)
	_, err := meta.UpsertPublisher(ctx, "acme", "Acme", "PEM", hash)
	require.NoError(t, err)

	pub, err := registry.RequirePublisher(ctx, "Bearer "+token, meta)
	require.NoError(t, err)
	assert.Equal(t, "acme", pub.Handle)
}

func TestRequirePublisher_MissingHeader(t *testing.T) {
	meta := storage.NewInMemoryMetadataStore()
	_, err := registry.RequirePublisher(context.Background(), "", meta)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "missing Authorization")
}

func TestRequirePublisher_WrongScheme(t *testing.T) {
	meta := storage.NewInMemoryMetadataStore()
	_, err := registry.RequirePublisher(context.Background(), "Basic dXNlcjpwYXNz", meta)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "Bearer")
}

func TestRequirePublisher_EmptyToken(t *testing.T) {
	meta := storage.NewInMemoryMetadataStore()
	_, err := registry.RequirePublisher(context.Background(), "Bearer ", meta)
	assert.Error(t, err)
}

func TestRequirePublisher_UnknownToken(t *testing.T) {
	meta := storage.NewInMemoryMetadataStore()
	_, err := registry.RequirePublisher(context.Background(), "Bearer no-such-token", meta)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid bearer token")
}

func TestRequirePublisher_TrailingNewlineToken(t *testing.T) {
	meta := storage.NewInMemoryMetadataStore()
	ctx := context.Background()
	token := "trimmed-token"
	hash := registry.HashToken(token)
	_, _ = meta.UpsertPublisher(ctx, "pub", "Pub", "PEM", hash)

	// Client sends token with trailing newline.
	pub, err := registry.RequirePublisher(ctx, "Bearer "+token+"\n", meta)
	require.NoError(t, err)
	assert.Equal(t, "pub", pub.Handle)
}
