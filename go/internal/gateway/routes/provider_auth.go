package routes

// Provider auth routes — POST /users/me/provider-auth/{provider}
//
// Lets an authenticated user bootstrap CLI credentials inside their Daytona
// sandbox. The orchestrator execs the provider's auth command and returns the
// captured output (typically a browser URL or device code) so the user can
// complete the OAuth flow on their device.
//
// Supported providers: claude, codex, gemini
//
// POST /users/me/provider-auth/{provider}
//   → provisions sandbox, runs auth command, returns {output, status}
//
// GET  /users/me/provider-auth/{provider}/status
//   → runs a non-interactive probe (e.g. claude auth status) to check whether
//     auth is complete.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/openclaw/agent-platform/internal/gateway"
)

func b64Encode(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}

func pathDir(p string) string {
	return path.Dir(p)
}

// providerAuthCmd maps provider name → auth command argv.
var providerAuthCmd = map[string][]string{
	"claude": {"claude", "auth", "login"},
	"codex":  {"codex", "login"},
	"gemini": {"gemini", "auth", "login"},
}

// providerStatusCmd maps provider name → status-check command argv.
// We run a lightweight non-interactive check that exits 0 if authenticated.
var providerStatusCmd = map[string][]string{
	"claude": {"claude", "-p", "hi", "--output-format", "json", "--no-session-persistence"},
	"codex":  {"codex", "-q", "--no-interactive", "echo hi"},
	"gemini": {"gemini", "--output-format", "json", "--prompt", "hi"},
}

// credentialsPath maps provider name → canonical (persistent volume) credentials path.
// We also write a copy to the active user's home so the CLI finds them immediately.
var credentialsPath = map[string]string{
	"claude": "/home/user/.claude/.credentials.json",
	"codex":  "/home/user/.codex/auth.json",
	"gemini": "/home/user/.gemini/credentials.json",
}

// credentialsDaytonaPath is the active user (daytona) path — credentials must
// also be written here since the sandbox process runs as user 'daytona'.
var credentialsDaytonaPath = map[string]string{
	"claude": "/home/daytona/.claude/.credentials.json",
	"codex":  "/home/daytona/.codex/auth.json",
	"gemini": "/home/daytona/.gemini/credentials.json",
}

func mountProviderAuth(r chi.Router, app *gateway.App) {
	r.Route("/users/me/provider-auth", func(pr chi.Router) {
		pr.Use(gateway.UserAuthMiddleware(app.Config.UserJWTSecret))

		// POST /users/me/provider-auth/{provider}
		// Starts the interactive auth flow. Returns captured output which should
		// contain a browser URL or device code the user can act on.
		pr.Post("/{provider}", func(w http.ResponseWriter, r *http.Request) {
			p := gateway.PrincipalFromContext(r.Context())
			if p == nil {
				writeError(w, http.StatusUnauthorized, "unauthenticated")
				return
			}
			provider := strings.ToLower(chi.URLParam(r, "provider"))
			cmd, ok := providerAuthCmd[provider]
			if !ok {
				writeError(w, http.StatusBadRequest, "unknown provider: "+provider+"; supported: claude, codex, gemini")
				return
			}

			// Provision (or resume) the user's sandbox. Cold-start can take 60-90s.
			provCtx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
			defer cancel()
			sandbox, err := app.Orchestrator.ProvisionSandbox(provCtx, p.UserID)
			if err != nil {
				writeError(w, http.StatusBadGateway, "sandbox provision failed: "+err.Error())
				return
			}

			// Run the auth command with a short capture timeout.
			// Most CLI auth commands print an auth URL immediately and then
			// block waiting for the browser redirect — we capture the initial
			// output and return it. The auth flow completes asynchronously.
			execCtx, execCancel := context.WithTimeout(r.Context(), 20*time.Second)
			defer execCancel()
			result, err := app.Orchestrator.ExecInSandbox(execCtx, sandbox.ID, cmd, 15)
			if err != nil {
				writeError(w, http.StatusBadGateway, "exec auth command failed: "+err.Error())
				return
			}

			output := strings.TrimSpace(result.Stdout)
			if result.Stderr != "" {
				if output != "" {
					output += "\n" + strings.TrimSpace(result.Stderr)
				} else {
					output = strings.TrimSpace(result.Stderr)
				}
			}

			writeJSON(w, http.StatusOK, map[string]any{
				"provider":   provider,
				"sandbox_id": sandbox.ID,
				"output":     output,
				"timed_out":  result.TimedOut,
				"exit_code":  result.ExitCode,
			})
		})

		// POST /users/me/provider-auth/{provider}/credentials
		// Seeds the sandbox's persistent volume with provider credentials.
		// The request body must be the raw credentials JSON as accepted by
		// the provider's CLI (e.g. ~/.claude/.credentials.json content).
		// On first call this writes the credentials; subsequent calls
		// overwrite them (re-auth / token refresh).
		pr.Post("/{provider}/credentials", func(w http.ResponseWriter, r *http.Request) {
			p := gateway.PrincipalFromContext(r.Context())
			if p == nil {
				writeError(w, http.StatusUnauthorized, "unauthenticated")
				return
			}
			provider := strings.ToLower(chi.URLParam(r, "provider"))
			destPath, ok := credentialsPath[provider]
			if !ok {
				writeError(w, http.StatusBadRequest, "unknown provider: "+provider+"; supported: claude, codex, gemini")
				return
			}

			// Read and re-marshal the body to validate it's JSON.
			var credJSON any
			if code, msg, err := decodeJSON(r, &credJSON); err != nil {
				writeError(w, code, msg)
				return
			}
			credBytes, err := json.Marshal(credJSON)
			if err != nil {
				writeError(w, http.StatusBadRequest, "credentials encode failed: "+err.Error())
				return
			}

			provCtx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
			defer cancel()
			sandbox, err := app.Orchestrator.ProvisionSandbox(provCtx, p.UserID)
			if err != nil {
				writeError(w, http.StatusBadGateway, "sandbox provision failed: "+err.Error())
				return
			}

			// Write credentials to the persistent volume using base64 to avoid
			// shell escaping issues with the credentials content.
			b64 := b64Encode(credBytes)
			destDir := pathDir(destPath)
			daytonaPath := credentialsDaytonaPath[provider]
			daytonaDir := pathDir(daytonaPath)
			// Write to both the persistent volume path and the active user's
			// home so the CLI finds them immediately. No chmod — volume is
			// world-writable and owned by root.
			writeCmd := []string{
				"sh", "-c",
				"mkdir -p " + destDir + " && " +
					"printf '%s' " + b64 + " | base64 -d > " + destPath + " && " +
					"mkdir -p " + daytonaDir + " && " +
					"cp " + destPath + " " + daytonaPath,
			}
			execCtx, execCancel := context.WithTimeout(r.Context(), 15*time.Second)
			defer execCancel()
			result, err := app.Orchestrator.ExecInSandbox(execCtx, sandbox.ID, writeCmd, 10)
			if err != nil {
				writeError(w, http.StatusBadGateway, "exec write credentials failed: "+err.Error())
				return
			}
			if result.ExitCode != nil && *result.ExitCode != 0 {
				writeError(w, http.StatusBadGateway, "write credentials failed (exit "+fmt.Sprintf("%d", *result.ExitCode)+"): "+result.Stderr)
				return
			}

			writeJSON(w, http.StatusOK, map[string]any{
				"provider":   provider,
				"sandbox_id": sandbox.ID,
				"written_to": destPath,
				"status":     "ok",
			})
		})

		// GET /users/me/provider-auth/{provider}/status
		// Runs a lightweight non-interactive probe to check whether the
		// provider's CLI is authenticated.
		pr.Get("/{provider}/status", func(w http.ResponseWriter, r *http.Request) {
			p := gateway.PrincipalFromContext(r.Context())
			if p == nil {
				writeError(w, http.StatusUnauthorized, "unauthenticated")
				return
			}
			provider := strings.ToLower(chi.URLParam(r, "provider"))
			cmd, ok := providerStatusCmd[provider]
			if !ok {
				writeError(w, http.StatusBadRequest, "unknown provider: "+provider+"; supported: claude, codex, gemini")
				return
			}

			provCtx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
			defer cancel()
			sandbox, err := app.Orchestrator.ProvisionSandbox(provCtx, p.UserID)
			if err != nil {
				writeError(w, http.StatusBadGateway, "sandbox provision failed: "+err.Error())
				return
			}

			execCtx, execCancel := context.WithTimeout(r.Context(), 15*time.Second)
			defer execCancel()
			result, err := app.Orchestrator.ExecInSandbox(execCtx, sandbox.ID, cmd, 10)
			if err != nil {
				writeError(w, http.StatusBadGateway, "exec status check failed: "+err.Error())
				return
			}

			// For Claude, we get JSON output — detect auth errors by result text.
			authenticated := result.ExitCode != nil && *result.ExitCode == 0
			output := strings.TrimSpace(result.Stdout)
			if result.Stderr != "" {
				output += "\n" + strings.TrimSpace(result.Stderr)
			}
			// Claude CLI sets is_error=true and puts auth message in result on auth failure.
			if strings.Contains(output, "Invalid API key") || strings.Contains(output, "Please run /login") {
				authenticated = false
			}

			writeJSON(w, http.StatusOK, map[string]any{
				"provider":      provider,
				"sandbox_id":    sandbox.ID,
				"authenticated": authenticated,
				"output":        output,
				"exit_code":     result.ExitCode,
			})
		})
	})
}
