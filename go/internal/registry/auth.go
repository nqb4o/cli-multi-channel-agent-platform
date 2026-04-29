package registry

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"

	"github.com/openclaw/agent-platform/internal/registry/storage"
)

// AuthError is returned when the bearer token is missing, malformed, or
// does not match any known publisher. The API layer maps it to a 401 with
// a fixed body so callers cannot distinguish "no token" from "wrong token".
type AuthError struct{ msg string }

func (e *AuthError) Error() string { return e.msg }

// Publisher is the authenticated publisher view used by the publish handler.
type Publisher struct {
	Handle       string
	PublicKeyPEM string
}

// HashToken returns sha256(token.strip()).hexdigest() — what the registry
// stores and compares. Whitespace is stripped because clients sometimes
// append a newline.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return fmt.Sprintf("%x", sum)
}

// RequirePublisher resolves a publisher from a Bearer Authorization header.
// It always returns the same error string for "no token" and "wrong token"
// so brute-forcing the publisher list is no easier than iterating /skills.
func RequirePublisher(ctx context.Context, authorizationHeader string, meta storage.MetadataStore) (*Publisher, error) {
	token, err := extractBearer(authorizationHeader)
	if err != nil {
		return nil, err
	}
	digest := HashToken(token)
	row, err := meta.GetPublisherByTokenHash(ctx, digest)
	if err != nil {
		return nil, fmt.Errorf("auth: store lookup: %w", err)
	}
	if row == nil {
		return nil, &AuthError{"invalid bearer token"}
	}
	return &Publisher{Handle: row.Handle, PublicKeyPEM: row.PublicKeyPEM}, nil
}

func extractBearer(h string) (string, error) {
	if h == "" {
		return "", &AuthError{"missing Authorization header"}
	}
	parts := strings.SplitN(strings.TrimSpace(h), " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") {
		return "", &AuthError{"Authorization header must use Bearer scheme"}
	}
	token := strings.TrimSpace(parts[1])
	if token == "" {
		return "", &AuthError{"empty bearer token"}
	}
	return token, nil
}
