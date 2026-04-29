package storage_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openclaw/agent-platform/internal/registry/storage"
)

// ---------------------------------------------------------------------------
// InMemoryBlobStore tests.
// ---------------------------------------------------------------------------

func TestInMemoryBlobStore_PutGet(t *testing.T) {
	s := storage.NewInMemoryBlobStore()
	payload := []byte("hello, blob store")
	digest, err := s.Put(payload)
	require.NoError(t, err)
	assert.Equal(t, storage.SHA256Hex(payload), digest)

	got, err := s.Get(digest)
	require.NoError(t, err)
	assert.Equal(t, payload, got)
}

func TestInMemoryBlobStore_PutIdempotent(t *testing.T) {
	s := storage.NewInMemoryBlobStore()
	payload := []byte("idempotent content")
	d1, err := s.Put(payload)
	require.NoError(t, err)
	d2, err := s.Put(payload)
	require.NoError(t, err)
	assert.Equal(t, d1, d2)
	assert.True(t, s.Has(d1))
}

func TestInMemoryBlobStore_GetMissing(t *testing.T) {
	s := storage.NewInMemoryBlobStore()
	_, err := s.Get("aaaa" + string(make([]byte, 60)))
	assert.Error(t, err)
}

func TestInMemoryBlobStore_Has(t *testing.T) {
	s := storage.NewInMemoryBlobStore()
	assert.False(t, s.Has(storage.SHA256Hex([]byte("missing"))))
	d, _ := s.Put([]byte("present"))
	assert.True(t, s.Has(d))
}

func TestInMemoryBlobStore_IsolatesReturned(t *testing.T) {
	s := storage.NewInMemoryBlobStore()
	payload := []byte("original")
	digest, _ := s.Put(payload)
	got, _ := s.Get(digest)
	got[0] = 'X' // mutate returned slice
	got2, _ := s.Get(digest)
	assert.Equal(t, payload, got2, "mutation of returned slice must not affect stored bytes")
}

// ---------------------------------------------------------------------------
// FilesystemBlobStore tests.
// ---------------------------------------------------------------------------

func TestFilesystemBlobStore_PutGet(t *testing.T) {
	dir := t.TempDir()
	s, err := storage.NewFilesystemBlobStore(dir)
	require.NoError(t, err)

	payload := []byte("filesystem blob content")
	digest, err := s.Put(payload)
	require.NoError(t, err)
	assert.Len(t, digest, 64)

	got, err := s.Get(digest)
	require.NoError(t, err)
	assert.Equal(t, payload, got)
}

func TestFilesystemBlobStore_PutIdempotent(t *testing.T) {
	dir := t.TempDir()
	s, err := storage.NewFilesystemBlobStore(dir)
	require.NoError(t, err)

	payload := []byte("repeat")
	d1, err := s.Put(payload)
	require.NoError(t, err)
	d2, err := s.Put(payload)
	require.NoError(t, err)
	assert.Equal(t, d1, d2)
}

func TestFilesystemBlobStore_GetMissing(t *testing.T) {
	dir := t.TempDir()
	s, err := storage.NewFilesystemBlobStore(dir)
	require.NoError(t, err)
	// 64 lowercase hex chars
	_, getErr := s.Get("abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890")
	assert.Error(t, getErr)
	assert.True(t, os.IsNotExist(getErr) || getErr != nil)
}

func TestFilesystemBlobStore_ShardLayout(t *testing.T) {
	dir := t.TempDir()
	s, err := storage.NewFilesystemBlobStore(dir)
	require.NoError(t, err)

	payload := []byte("layout test")
	digest, _ := s.Put(payload)

	expectedPath := filepath.Join(s.Root(), "sha256", digest[:2], digest)
	_, statErr := os.Stat(expectedPath)
	assert.NoError(t, statErr, "blob should be stored at sha256/<2char>/<digest>")
}

func TestFilesystemBlobStore_Has(t *testing.T) {
	dir := t.TempDir()
	s, err := storage.NewFilesystemBlobStore(dir)
	require.NoError(t, err)

	d, _ := s.Put([]byte("presence check"))
	assert.True(t, s.Has(d))
	assert.False(t, s.Has("abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"))
}

func TestFilesystemBlobStore_InvalidRoot(t *testing.T) {
	_, err := storage.NewFilesystemBlobStore("/proc/nonexistent/path/that/cannot/exist")
	assert.Error(t, err)
}

// ---------------------------------------------------------------------------
// SHA256Hex.
// ---------------------------------------------------------------------------

func TestSHA256Hex(t *testing.T) {
	// Known SHA-256 of empty string.
	got := storage.SHA256Hex([]byte{})
	assert.Equal(t, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", got)
}
