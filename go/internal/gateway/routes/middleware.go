package routes

import (
	"context"
	"net/http"
	"time"
)

// requestCtxWithTimeout derives a child context from r.Context() with a hard
// deadline. Used by handlers that talk to external services (Redis, DB, F01).
func requestCtxWithTimeout(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		return context.WithCancel(r.Context())
	}
	return context.WithTimeout(r.Context(), d)
}
