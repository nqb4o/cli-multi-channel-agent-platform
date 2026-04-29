package gateway

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

const bearerPrefix = "Bearer "

// AdminAuthMiddleware enforces an Authorization: Bearer <ADMIN_TOKEN> header.
//
// Behaviour mirrors the Python admin auth dependency:
//   - 503 fail-closed if expectedToken is empty (no admin token configured).
//   - 401 if the Authorization header is missing or not "Bearer <token>".
//   - 403 if the token does not match (constant-time compare).
func AdminAuthMiddleware(expectedToken string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if expectedToken == "" {
				writeJSONError(w, http.StatusServiceUnavailable, "admin token not configured")
				return
			}
			provided, ok := extractBearer(r.Header.Get("Authorization"))
			if !ok {
				w.Header().Set("WWW-Authenticate", "Bearer")
				writeJSONError(w, http.StatusUnauthorized, "missing bearer token")
				return
			}
			if subtle.ConstantTimeCompare([]byte(provided), []byte(expectedToken)) != 1 {
				writeJSONError(w, http.StatusForbidden, "invalid admin token")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// extractBearer parses an Authorization header, returning the token and
// whether the header was a well-formed "Bearer <token>" with a non-empty token.
func extractBearer(authz string) (string, bool) {
	if authz == "" {
		return "", false
	}
	if !strings.HasPrefix(authz, bearerPrefix) {
		return "", false
	}
	token := strings.TrimSpace(authz[len(bearerPrefix):])
	if token == "" {
		return "", false
	}
	return token, true
}
