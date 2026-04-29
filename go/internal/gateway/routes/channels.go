package routes

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/openclaw/agent-platform/internal/gateway"
)

type registerChannelUserReq struct {
	Type    string         `json:"type"`
	ExtID   string         `json:"ext_id"`
	AgentID string         `json:"agent_id"`
	Config  map[string]any `json:"config"`
}

func channelView(row *gateway.ChannelRow) map[string]any {
	return map[string]any{
		"channel_id": row.ChannelID,
		"user_id":    row.UserID,
		"agent_id":   row.AgentID,
		"type":       row.ChannelType,
		"ext_id":     row.ExtID,
	}
}

func mountUserChannels(r chi.Router, app *gateway.App) {
	// User-facing CRUD lives at the same /channels prefix as the webhook
	// router. Webhooks are a deeper path (`/{type}/webhook`), so we just
	// attach bearer-auth on the user-facing leaves rather than at prefix
	// scope to keep the public webhook unguarded.
	r.With(gateway.UserAuthMiddleware(app.Config.UserJWTSecret)).
		Post("/channels", makeRegisterUserChannel(app))
	r.With(gateway.UserAuthMiddleware(app.Config.UserJWTSecret)).
		Get("/channels", makeListUserChannels(app))
	r.With(gateway.UserAuthMiddleware(app.Config.UserJWTSecret)).
		Delete("/channels/{channel_id}", makeDeleteUserChannel(app))
}

func makeRegisterUserChannel(app *gateway.App) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := gateway.PrincipalFromContext(r.Context())
		if p == nil {
			writeError(w, http.StatusUnauthorized, "unauthenticated")
			return
		}
		var body registerChannelUserReq
		if code, msg, err := decodeJSON(r, &body); err != nil {
			writeError(w, code, msg)
			return
		}
		if !nonEmpty(body.Type) || !nonEmpty(body.ExtID) || !nonEmpty(body.AgentID) {
			writeError(w, http.StatusUnprocessableEntity,
				"type, ext_id, agent_id are required")
			return
		}

		ctx, cancel := requestCtxWithTimeout(r, 5*time.Second)
		defer cancel()

		// 1) Authorize: agent_id must exist AND be owned by the caller.
		agent, err := app.AgentsRepo.Get(ctx, body.AgentID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "agent lookup failed")
			return
		}
		if agent == nil {
			writeError(w, http.StatusNotFound, "agent not found")
			return
		}
		if agent.UserID != p.UserID {
			writeError(w, http.StatusForbidden, "not your agent")
			return
		}

		// 2) Encrypt config under DB_ENCRYPTION_KEY.
		key, kerr := gateway.ParseEncryptionKey(app.Config.DBEncryptionKeyHex)
		if kerr != nil {
			var ke *gateway.EncryptionKeyError
			if errors.As(kerr, &ke) {
				writeError(w, http.StatusInternalServerError, ke.Error())
				return
			}
			writeError(w, http.StatusInternalServerError, "DB_ENCRYPTION_KEY error: "+kerr.Error())
			return
		}
		cfg := body.Config
		if cfg == nil {
			cfg = map[string]any{}
		}
		plain, err := gateway.EncodeMessagePayload(cfg)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "encode config: "+err.Error())
			return
		}
		ct, err := gateway.EncryptChannelConfig(key, []byte(plain))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to encrypt channel config")
			return
		}

		// 3) Persist via ChannelsRepo.Register.
		rec, err := app.ChannelsRepo.Register(
			ctx, p.UserID, body.Type, body.ExtID, ct, body.AgentID,
		)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "channel registration failed")
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{
			"channel_id": rec.ChannelID,
			"user_id":    rec.UserID,
			"agent_id":   rec.AgentID,
			"type":       body.Type,
			"ext_id":     body.ExtID,
		})
	}
}

func makeListUserChannels(app *gateway.App) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := gateway.PrincipalFromContext(r.Context())
		if p == nil {
			writeError(w, http.StatusUnauthorized, "unauthenticated")
			return
		}
		listable, ok := app.ChannelsRepo.(gateway.ChannelsRepoWithListGetDelete)
		if !ok {
			// Frozen ChannelsRepo Protocol doesn't include ListForUser; degrade
			// gracefully to an empty list rather than 5xx.
			writeJSON(w, http.StatusOK, map[string]any{"channels": []any{}})
			return
		}
		ctx, cancel := requestCtxWithTimeout(r, 5*time.Second)
		defer cancel()
		rows, err := listable.ListForUser(ctx, p.UserID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "list channels failed")
			return
		}
		out := make([]map[string]any, 0, len(rows))
		for i := range rows {
			out = append(out, channelView(&rows[i]))
		}
		writeJSON(w, http.StatusOK, map[string]any{"channels": out})
	}
}

func makeDeleteUserChannel(app *gateway.App) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := gateway.PrincipalFromContext(r.Context())
		if p == nil {
			writeError(w, http.StatusUnauthorized, "unauthenticated")
			return
		}
		full, ok := app.ChannelsRepo.(gateway.ChannelsRepoWithListGetDelete)
		if !ok {
			writeError(w, http.StatusNotImplemented,
				"channel lookup not supported by configured repo")
			return
		}
		id := chi.URLParam(r, "channel_id")
		ctx, cancel := requestCtxWithTimeout(r, 5*time.Second)
		defer cancel()
		row, err := full.Get(ctx, id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "get channel failed")
			return
		}
		if row == nil {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		if row.UserID != p.UserID {
			writeError(w, http.StatusForbidden, "not your channel")
			return
		}
		if _, err := full.Delete(ctx, id); err != nil {
			writeError(w, http.StatusInternalServerError, "delete failed")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
