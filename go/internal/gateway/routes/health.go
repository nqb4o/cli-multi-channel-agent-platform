package routes

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/openclaw/agent-platform/internal/gateway"
)

func mountHealth(r chi.Router, app *gateway.App) {
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	r.Get("/readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := requestCtxWithTimeout(r, 2*time.Second)
		defer cancel()

		redisOK := false
		if cmd := app.Redis.Ping(ctx); cmd != nil && cmd.Err() == nil {
			redisOK = true
		}
		dbOK := false
		if app.DBHealth != nil {
			dbOK = app.DBHealth.Ping(ctx)
		}

		body := map[string]any{
			"redis": redisOK,
			"db":    dbOK,
		}
		if redisOK && dbOK {
			body["status"] = "ready"
			writeJSON(w, http.StatusOK, body)
			return
		}
		body["status"] = "not_ready"
		writeJSON(w, http.StatusServiceUnavailable, body)
	})
}
