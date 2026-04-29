package routes

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/openclaw/agent-platform/internal/gateway"
)

type provisionSandboxReq struct {
	UserID string `json:"user_id"`
}

type registerChannelAdminReq struct {
	UserID      string         `json:"user_id"`
	AgentID     string         `json:"agent_id"`
	ChannelType string         `json:"channel_type"`
	ExtID       string         `json:"ext_id"`
	Config      map[string]any `json:"config"`
}

func mountAdmin(r chi.Router, app *gateway.App) {
	r.Route("/admin", func(ar chi.Router) {
		ar.Use(gateway.AdminAuthMiddleware(app.Config.AdminToken))

		ar.Post("/sandboxes", func(w http.ResponseWriter, r *http.Request) {
			var body provisionSandboxReq
			if code, msg, err := decodeJSON(r, &body); err != nil {
				writeError(w, code, msg)
				return
			}
			if !nonEmpty(body.UserID) {
				writeError(w, http.StatusUnprocessableEntity, "user_id is required")
				return
			}
			ctx, cancel := requestCtxWithTimeout(r, 5*time.Second)
			defer cancel()
			view, err := app.Orchestrator.ProvisionSandbox(ctx, body.UserID)
			if err != nil {
				var statusErr *gateway.OrchestratorStatusError
				var transportErr *gateway.OrchestratorTransportError
				switch {
				case errors.As(err, &statusErr):
					writeError(w, http.StatusBadGateway, "orchestrator error")
				case errors.As(err, &transportErr):
					writeError(w, http.StatusBadGateway, "orchestrator unreachable")
				default:
					writeError(w, http.StatusBadGateway, "orchestrator failed: "+err.Error())
				}
				return
			}
			writeJSON(w, http.StatusOK, map[string]string{
				"id":      view.ID,
				"user_id": view.UserID,
				"state":   view.State,
			})
		})

		ar.Post("/channels", func(w http.ResponseWriter, r *http.Request) {
			var body registerChannelAdminReq
			if code, msg, err := decodeJSON(r, &body); err != nil {
				writeError(w, code, msg)
				return
			}
			if !nonEmpty(body.UserID) || !nonEmpty(body.AgentID) ||
				!nonEmpty(body.ChannelType) || !nonEmpty(body.ExtID) {
				writeError(w, http.StatusUnprocessableEntity,
					"user_id, agent_id, channel_type, ext_id are required")
				return
			}
			cfg := body.Config
			if cfg == nil {
				cfg = map[string]any{}
			}
			// Mirror Python: pass JSON-serialized bytes through. F12 would
			// AES-GCM-encrypt; the in-memory stub stores verbatim.
			plain, err := gateway.EncodeMessagePayload(cfg)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "encode config: "+err.Error())
				return
			}
			ctx, cancel := requestCtxWithTimeout(r, 5*time.Second)
			defer cancel()
			rec, err := app.ChannelsRepo.Register(
				ctx,
				body.UserID,
				body.ChannelType,
				body.ExtID,
				[]byte(plain),
				body.AgentID,
			)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "channel registration failed")
				return
			}
			writeJSON(w, http.StatusCreated, map[string]string{
				"channel_id":   rec.ChannelID,
				"user_id":      rec.UserID,
				"agent_id":     rec.AgentID,
				"channel_type": body.ChannelType,
				"ext_id":       body.ExtID,
			})
		})
	})
}
