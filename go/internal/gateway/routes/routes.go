// Package routes wires up the F06 gateway HTTP surface using chi.
//
// Layout matches the Python FastAPI routers:
//
//	GET   /healthz            (health.go)
//	GET   /readyz             (health.go)
//	POST  /channels/{type}/webhook    (webhooks.go)
//	POST  /admin/sandboxes    (admin.go)   — admin-token gated
//	POST  /admin/channels     (admin.go)   — admin-token gated
//	POST  /auth/signup        (auth.go)
//	POST  /auth/login         (auth.go)
//	GET   /auth/me            (auth.go)    — bearer JWT
//	POST  /agents             (agents.go)  — bearer JWT
//	GET   /agents             (agents.go)  — bearer JWT
//	GET   /agents/{id}        (agents.go)  — bearer JWT
//	PATCH /agents/{id}        (agents.go)  — bearer JWT
//	DELETE /agents/{id}       (agents.go)  — bearer JWT
//	POST  /channels           (channels.go) — bearer JWT
//	GET   /channels           (channels.go) — bearer JWT
//	DELETE /channels/{id}     (channels.go) — bearer JWT
package routes

import (
	"github.com/go-chi/chi/v5"

	"github.com/openclaw/agent-platform/internal/gateway"
)

// NewRouter composes the full gateway HTTP router.
//
// Note on the /channels prefix: the user-facing `/channels` POST/GET/DELETE
// share the prefix with `/channels/{type}/webhook`. chi's trie router
// disambiguates by full path; webhooks are mounted on a sub-router that only
// responds at the deeper path (`/{type}/webhook`), so the bearer-auth on the
// user routes never gates the public webhook path.
func NewRouter(app *gateway.App) *chi.Mux {
	r := chi.NewRouter()

	mountHealth(r, app)
	mountWebhooks(r, app)
	mountAdmin(r, app)
	mountAuth(r, app)
	mountAgents(r, app)
	mountUserChannels(r, app)

	return r
}
