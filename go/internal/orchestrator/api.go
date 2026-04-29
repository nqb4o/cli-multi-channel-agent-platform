// FastAPI-equivalent HTTP routes via chi.
//
// Routes (frozen — F06's HttpOrchestratorClient depends on the exact shape):
//
//	GET    /healthz                          — liveness + Daytona reachability.
//	POST   /sandboxes                        — provision (create or resume).
//	GET    /sandboxes/{id}                   — look up by id.
//	POST   /sandboxes/{id}/resume            — explicit resume.
//	POST   /sandboxes/{id}/hibernate         — explicit hibernate.
//	DELETE /sandboxes/{id}                   — destroy.
//	POST   /sandboxes/{id}/exec              — admin one-shot exec.
//	POST   /sandboxes/{user_id}/prewarm      — presence-driven prewarm.
//	POST   /admin/warm-pool/refresh          — recompute top-N now.
//	GET    /admin/warm-pool/status           — current warm-pool snapshot.
package orchestrator

import (
	"encoding/base64"
	"encoding/json"
	"log"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

// Deps is the set of components the HTTP layer talks to. Pass any subset
// — nil components yield 503 responses on the corresponding routes.
type Deps struct {
	Orchestrator *Orchestrator
	Pool         *SandboxPool
	Presence     *PresenceTrigger
	WarmPool     *WarmPoolManager
}

// NewRouter composes the orchestrator's chi router from a Deps struct.
func NewRouter(d Deps) *chi.Mux {
	r := chi.NewRouter()
	r.Get("/healthz", d.healthz)
	r.Post("/sandboxes", d.postSandbox)
	r.Get("/sandboxes/{sandbox_id}", d.getSandbox)
	r.Post("/sandboxes/{sandbox_id}/resume", d.postResume)
	r.Post("/sandboxes/{sandbox_id}/hibernate", d.postHibernate)
	r.Delete("/sandboxes/{sandbox_id}", d.deleteSandbox)
	r.Post("/sandboxes/{sandbox_id}/exec", d.postExec)
	r.Post("/sandboxes/{user_id}/prewarm", d.postPrewarm)
	r.Post("/admin/warm-pool/refresh", d.warmPoolRefresh)
	r.Get("/admin/warm-pool/status", d.warmPoolStatus)
	return r
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, detail string) {
	writeJSON(w, status, map[string]string{"detail": detail})
}

func serializeSandbox(s *Sandbox) map[string]any {
	labels := s.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	return map[string]any{
		"id":      s.ID,
		"user_id": s.UserID,
		"state":   string(s.State),
		"labels":  labels,
	}
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func (d *Deps) healthz(w http.ResponseWriter, r *http.Request) {
	if d.Orchestrator == nil {
		writeErr(w, http.StatusServiceUnavailable, "orchestrator not configured")
		return
	}
	ok := d.Orchestrator.Client().Healthz(r.Context())
	if ok {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{
		"status":      "degraded",
		"provider_ok": false,
	})
}

type provisionSandboxRequest struct {
	UserID string `json:"user_id"`
}

func (d *Deps) postSandbox(w http.ResponseWriter, r *http.Request) {
	if d.Orchestrator == nil {
		writeErr(w, http.StatusServiceUnavailable, "orchestrator not configured")
		return
	}
	var body provisionSandboxRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if body.UserID == "" {
		writeErr(w, http.StatusBadRequest, "user_id is required")
		return
	}
	if len(body.UserID) > 128 {
		writeErr(w, http.StatusBadRequest, "user_id too long")
		return
	}
	var sandbox *Sandbox
	var err error
	if d.Pool != nil {
		sandbox, err = d.Pool.GetOrResume(r.Context(), body.UserID)
	} else {
		sandbox, err = d.Orchestrator.GetOrResume(r.Context(), body.UserID)
	}
	if err != nil {
		log.Printf("provision_sandbox failed: %v", err)
		writeErr(w, http.StatusBadGateway, "provisioning failed")
		return
	}
	writeJSON(w, http.StatusOK, serializeSandbox(sandbox))
}

func (d *Deps) getSandbox(w http.ResponseWriter, r *http.Request) {
	if d.Orchestrator == nil {
		writeErr(w, http.StatusServiceUnavailable, "orchestrator not configured")
		return
	}
	id := chi.URLParam(r, "sandbox_id")
	sandbox, err := d.Orchestrator.Get(r.Context(), id)
	if err != nil {
		if isNotFound(err) {
			writeErr(w, http.StatusNotFound, "sandbox not found")
			return
		}
		log.Printf("get_sandbox failed: %v", err)
		writeErr(w, http.StatusBadGateway, "lookup failed")
		return
	}
	writeJSON(w, http.StatusOK, serializeSandbox(sandbox))
}

func (d *Deps) postResume(w http.ResponseWriter, r *http.Request) {
	if d.Orchestrator == nil {
		writeErr(w, http.StatusServiceUnavailable, "orchestrator not configured")
		return
	}
	id := chi.URLParam(r, "sandbox_id")
	sandbox, err := d.Orchestrator.Get(r.Context(), id)
	if err != nil {
		if isNotFound(err) {
			writeErr(w, http.StatusNotFound, "sandbox not found")
			return
		}
		writeErr(w, http.StatusBadGateway, "lookup failed")
		return
	}
	if sandbox.State == StateDestroyed {
		writeErr(w, http.StatusGone, "sandbox destroyed")
		return
	}
	if sandbox.State == StateHibernated {
		sandbox, err = d.Orchestrator.GetOrResume(r.Context(), sandbox.UserID)
		if err != nil {
			writeErr(w, http.StatusBadGateway, "resume failed")
			return
		}
	}
	writeJSON(w, http.StatusOK, serializeSandbox(sandbox))
}

func (d *Deps) postHibernate(w http.ResponseWriter, r *http.Request) {
	if d.Orchestrator == nil {
		writeErr(w, http.StatusServiceUnavailable, "orchestrator not configured")
		return
	}
	id := chi.URLParam(r, "sandbox_id")
	if err := d.Orchestrator.Hibernate(r.Context(), id); err != nil {
		if isNotFound(err) {
			writeErr(w, http.StatusNotFound, "sandbox not found")
			return
		}
		log.Printf("hibernate failed: %v", err)
		writeErr(w, http.StatusBadGateway, "hibernate failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (d *Deps) deleteSandbox(w http.ResponseWriter, r *http.Request) {
	if d.Orchestrator == nil {
		writeErr(w, http.StatusServiceUnavailable, "orchestrator not configured")
		return
	}
	id := chi.URLParam(r, "sandbox_id")
	if err := d.Orchestrator.Destroy(r.Context(), id); err != nil {
		log.Printf("destroy failed: %v", err)
		writeErr(w, http.StatusBadGateway, "destroy failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type execRequest struct {
	Cmd      []string          `json:"cmd"`
	Env      map[string]string `json:"env,omitempty"`
	StdinB64 string            `json:"stdin_b64,omitempty"`
	TimeoutS int               `json:"timeout_s,omitempty"`
}

func (d *Deps) postExec(w http.ResponseWriter, r *http.Request) {
	if d.Orchestrator == nil {
		writeErr(w, http.StatusServiceUnavailable, "orchestrator not configured")
		return
	}
	id := chi.URLParam(r, "sandbox_id")
	var body execRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if len(body.Cmd) == 0 {
		writeErr(w, http.StatusBadRequest, "cmd must be non-empty")
		return
	}
	var stdin []byte
	if body.StdinB64 != "" {
		dec, err := base64.StdEncoding.DecodeString(body.StdinB64)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid stdin_b64")
			return
		}
		stdin = dec
	}
	timeout := body.TimeoutS
	if timeout <= 0 {
		timeout = 60
	}
	result, err := d.Orchestrator.Exec(r.Context(), id, body.Cmd, stdin, body.Env, timeout)
	if err != nil {
		if isNotFound(err) {
			writeErr(w, http.StatusNotFound, "sandbox not found")
			return
		}
		writeErr(w, http.StatusBadGateway, "exec failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"exit_code":  result.ExitCode,
		"stdout_b64": base64.StdEncoding.EncodeToString(result.Stdout),
		"stderr_b64": base64.StdEncoding.EncodeToString(result.Stderr),
		"timed_out":  result.TimedOut,
	})
}

type prewarmRequest struct {
	Signal string `json:"signal,omitempty"`
}

func (d *Deps) postPrewarm(w http.ResponseWriter, r *http.Request) {
	if d.Presence == nil {
		writeErr(w, http.StatusServiceUnavailable, "presence trigger not configured")
		return
	}
	userID := chi.URLParam(r, "user_id")
	// Decode the optional body — empty body is fine.
	if r.Body != nil && r.ContentLength > 0 {
		var _body prewarmRequest
		_ = json.NewDecoder(r.Body).Decode(&_body)
	}
	res, err := d.Presence.Prewarm(r.Context(), userID)
	if err != nil {
		if strings.Contains(err.Error(), "user_id is required") {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		log.Printf("prewarm failed: %v", err)
		writeErr(w, http.StatusBadGateway, "prewarm failed")
		return
	}
	writeJSON(w, http.StatusAccepted, res)
}

func (d *Deps) warmPoolRefresh(w http.ResponseWriter, r *http.Request) {
	if d.WarmPool == nil {
		writeErr(w, http.StatusServiceUnavailable, "warm pool not configured")
		return
	}
	snap, err := d.WarmPool.Refresh(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, "warm pool refresh failed")
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

func (d *Deps) warmPoolStatus(w http.ResponseWriter, _ *http.Request) {
	if d.WarmPool == nil {
		writeErr(w, http.StatusServiceUnavailable, "warm pool not configured")
		return
	}
	writeJSON(w, http.StatusOK, d.WarmPool.Status())
}

// isNotFound reports whether err looks like a "sandbox not found" error
// returned by the underlying client.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "not found")
}

