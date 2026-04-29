package routes

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/openclaw/agent-platform/internal/gateway"
)

type createAgentReq struct {
	Name       string `json:"name"`
	ConfigYAML string `json:"config_yaml"`
}

type patchAgentReq struct {
	ConfigYAML string `json:"config_yaml"`
}

func agentToDict(a *gateway.Agent) map[string]any {
	out := map[string]any{
		"agent_id":    a.AgentID,
		"user_id":     a.UserID,
		"name":        a.Name,
		"config_yaml": a.ConfigYAML,
	}
	if !a.CreatedAt.IsZero() {
		out["created_at"] = a.CreatedAt.UTC().Format(time.RFC3339Nano)
	} else {
		out["created_at"] = nil
	}
	if !a.UpdatedAt.IsZero() {
		out["updated_at"] = a.UpdatedAt.UTC().Format(time.RFC3339Nano)
	} else {
		out["updated_at"] = nil
	}
	return out
}

func mountAgents(r chi.Router, app *gateway.App) {
	mw := gateway.UserAuthMiddleware(app.Config.UserJWTSecret)
	// Don't use Route("/agents") + Post("/") — chi treats the trailing slash
	// as a distinct path (404 on /agents). Mount each leaf at its full path.
	r.With(mw).Post("/agents", makeCreateAgent(app))
	r.With(mw).Get("/agents", makeListAgents(app))
	r.With(mw).Get("/agents/{agent_id}", makeGetAgent(app))
	r.With(mw).Patch("/agents/{agent_id}", makePatchAgent(app))
	r.With(mw).Delete("/agents/{agent_id}", makeDeleteAgent(app))
}

func makeCreateAgent(app *gateway.App) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := gateway.PrincipalFromContext(r.Context())
		if p == nil {
			writeError(w, http.StatusUnauthorized, "unauthenticated")
			return
		}
		var body createAgentReq
		if code, msg, err := decodeJSON(r, &body); err != nil {
			writeError(w, code, msg)
			return
		}
		if !nonEmpty(body.Name) || !nonEmpty(body.ConfigYAML) {
			writeError(w, http.StatusUnprocessableEntity, "name and config_yaml are required")
			return
		}
		ctx, cancel := requestCtxWithTimeout(r, 5*time.Second)
		defer cancel()
		a, err := app.AgentsRepo.Create(ctx, p.UserID, body.Name, body.ConfigYAML)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "create agent failed")
			return
		}
		writeJSON(w, http.StatusCreated, agentToDict(a))
	}
}

func makeListAgents(app *gateway.App) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := gateway.PrincipalFromContext(r.Context())
		if p == nil {
			writeError(w, http.StatusUnauthorized, "unauthenticated")
			return
		}
		ctx, cancel := requestCtxWithTimeout(r, 5*time.Second)
		defer cancel()
		rows, err := app.AgentsRepo.ListForUser(ctx, p.UserID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "list agents failed")
			return
		}
		out := make([]map[string]any, 0, len(rows))
		for i := range rows {
			out = append(out, agentToDict(&rows[i]))
		}
		writeJSON(w, http.StatusOK, map[string]any{"agents": out})
	}
}

func makeGetAgent(app *gateway.App) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := gateway.PrincipalFromContext(r.Context())
		if p == nil {
			writeError(w, http.StatusUnauthorized, "unauthenticated")
			return
		}
		id := chi.URLParam(r, "agent_id")
		ctx, cancel := requestCtxWithTimeout(r, 5*time.Second)
		defer cancel()
		a, err := app.AgentsRepo.Get(ctx, id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "get agent failed")
			return
		}
		if a == nil {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		if a.UserID != p.UserID {
			writeError(w, http.StatusForbidden, "not your agent")
			return
		}
		writeJSON(w, http.StatusOK, agentToDict(a))
	}
}

func makePatchAgent(app *gateway.App) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := gateway.PrincipalFromContext(r.Context())
		if p == nil {
			writeError(w, http.StatusUnauthorized, "unauthenticated")
			return
		}
		id := chi.URLParam(r, "agent_id")
		var body patchAgentReq
		if code, msg, err := decodeJSON(r, &body); err != nil {
			writeError(w, code, msg)
			return
		}
		if !nonEmpty(body.ConfigYAML) {
			writeError(w, http.StatusUnprocessableEntity, "config_yaml is required")
			return
		}
		ctx, cancel := requestCtxWithTimeout(r, 5*time.Second)
		defer cancel()
		existing, err := app.AgentsRepo.Get(ctx, id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "get agent failed")
			return
		}
		if existing == nil {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		if existing.UserID != p.UserID {
			writeError(w, http.StatusForbidden, "not your agent")
			return
		}
		updated, err := app.AgentsRepo.UpdateConfig(ctx, id, body.ConfigYAML)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "update agent failed")
			return
		}
		if updated == nil {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		writeJSON(w, http.StatusOK, agentToDict(updated))
	}
}

func makeDeleteAgent(app *gateway.App) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := gateway.PrincipalFromContext(r.Context())
		if p == nil {
			writeError(w, http.StatusUnauthorized, "unauthenticated")
			return
		}
		id := chi.URLParam(r, "agent_id")
		ctx, cancel := requestCtxWithTimeout(r, 5*time.Second)
		defer cancel()
		existing, err := app.AgentsRepo.Get(ctx, id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "get agent failed")
			return
		}
		if existing == nil {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		if existing.UserID != p.UserID {
			writeError(w, http.StatusForbidden, "not your agent")
			return
		}
		deletable, ok := app.AgentsRepo.(gateway.AgentsRepoWithDelete)
		if !ok {
			writeError(w, http.StatusNotImplemented,
				"agent deletion not supported by configured repo")
			return
		}
		if _, err := deletable.Delete(ctx, id); err != nil {
			writeError(w, http.StatusInternalServerError, "delete failed")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
