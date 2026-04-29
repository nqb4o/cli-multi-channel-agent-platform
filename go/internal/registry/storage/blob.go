// Package storage implements the content-addressed blob store (filesystem +
// in-memory) and the Postgres-backed metadata catalog for the F13 registry.
package storage

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// BlobStore is the content-addressed tarball store.
//
// Both Put and Get are synchronous; the HTTP layer runs them on goroutines that
// handle one request at a time, so the cost of a blocking FS write is bounded.
// Going async-native would force tests to manage an event loop for a single
// file write — not worth it.
type BlobStore interface {
	// Put stores payload and returns the sha256 hex digest. Idempotent.
	Put(payload []byte) (digest string, err error)
	// Get retrieves the blob identified by digestHex.
	// Returns os.ErrNotExist if missing.
	Get(digestHex string) ([]byte, error)
	// Has reports whether a blob with that digest exists.
	Has(digestHex string) bool
}

// SHA256Hex returns the lowercase hex-encoded SHA-256 digest of payload.
func SHA256Hex(payload []byte) string {
	sum := sha256.Sum256(payload)
	return fmt.Sprintf("%x", sum)
}

// ---------------------------------------------------------------------------
// In-memory store — tests + laptop dev mode.
// ---------------------------------------------------------------------------

// InMemoryBlobStore is a thread-safe map-backed BlobStore.
type InMemoryBlobStore struct {
	mu    sync.Mutex
	blobs map[string][]byte
}

// NewInMemoryBlobStore constructs an empty in-memory store.
func NewInMemoryBlobStore() *InMemoryBlobStore {
	return &InMemoryBlobStore{blobs: make(map[string][]byte)}
}

// Put stores payload. Duplicate digests are silently skipped.
func (s *InMemoryBlobStore) Put(payload []byte) (string, error) {
	digest := SHA256Hex(payload)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.blobs[digest]; !ok {
		s.blobs[digest] = append([]byte(nil), payload...) // defensive copy
	}
	return digest, nil
}

// Get retrieves a blob by digest. Returns os.ErrNotExist if not found.
func (s *InMemoryBlobStore) Get(digestHex string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.blobs[digestHex]
	if !ok {
		return nil, fmt.Errorf("blob %s: %w", digestHex, os.ErrNotExist)
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out, nil
}

// Has reports whether the digest is present.
func (s *InMemoryBlobStore) Has(digestHex string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.blobs[digestHex]
	return ok
}

// ---------------------------------------------------------------------------
// Filesystem store — production default.
// ---------------------------------------------------------------------------

// FilesystemBlobStore writes tarballs under:
//
//	<root>/sha256/<2-char-prefix>/<full-hex-digest>
//
// The two-char prefix sharding caps directory fan-out at 256 entries — the
// same trick git uses for its object store.
type FilesystemBlobStore struct {
	root string // absolute path, resolved at construction time
}

// NewFilesystemBlobStore constructs a FilesystemBlobStore rooted at dir.
// The sha256 subdirectory is created eagerly so config errors surface at
// startup rather than on the first publish.
func NewFilesystemBlobStore(dir string) (*FilesystemBlobStore, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("FilesystemBlobStore: resolve path: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(abs, "sha256"), 0o755); err != nil {
		return nil, fmt.Errorf("FilesystemBlobStore: mkdir: %w", err)
	}
	return &FilesystemBlobStore{root: abs}, nil
}

// Root returns the absolute root path.
func (s *FilesystemBlobStore) Root() string { return s.root }

func (s *FilesystemBlobStore) pathFor(digestHex string) (string, error) {
	if len(digestHex) != 64 {
		return "", fmt.Errorf("invalid sha256 hex digest length: %q", digestHex)
	}
	for _, c := range digestHex {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return "", fmt.Errorf("invalid sha256 hex digest: %q", digestHex)
		}
	}
	return filepath.Join(s.root, "sha256", digestHex[:2], digestHex), nil
}

// Put stores payload atomically via a .part temp-file rename. Idempotent.
func (s *FilesystemBlobStore) Put(payload []byte) (string, error) {
	digest := SHA256Hex(payload)
	target, err := s.pathFor(digest)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(target); err == nil {
		return digest, nil // already exists
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return "", fmt.Errorf("FilesystemBlobStore.Put mkdir: %w", err)
	}
	tmp := target + ".part"
	if err := os.WriteFile(tmp, payload, 0o644); err != nil {
		return "", fmt.Errorf("FilesystemBlobStore.Put write: %w", err)
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("FilesystemBlobStore.Put rename: %w", err)
	}
	return digest, nil
}

// Get reads the blob identified by digestHex.
func (s *FilesystemBlobStore) Get(digestHex string) ([]byte, error) {
	target, err := s.pathFor(digestHex)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(target)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("blob %s: %w", digestHex, os.ErrNotExist)
		}
		return nil, fmt.Errorf("FilesystemBlobStore.Get: %w", err)
	}
	return data, nil
}

// Has reports whether the blob exists.
func (s *FilesystemBlobStore) Has(digestHex string) bool {
	target, err := s.pathFor(digestHex)
	if err != nil {
		return false
	}
	_, err = os.Stat(target)
	return err == nil
}
