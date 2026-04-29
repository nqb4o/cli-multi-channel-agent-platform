package routes

import (
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/openclaw/agent-platform/internal/gateway"
)

var emailRE = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

type signupReq struct {
	Email string `json:"email"`
}

type loginReq struct {
	Email     string `json:"email"`
	MagicCode string `json:"magic_code"`
}

func normalizeEmail(raw string) (string, bool) {
	e := strings.ToLower(strings.TrimSpace(raw))
	if !emailRE.MatchString(e) {
		return "", false
	}
	return e, true
}

func mountAuth(r chi.Router, app *gateway.App) {
	r.Route("/auth", func(ar chi.Router) {
		ar.Post("/signup", func(w http.ResponseWriter, r *http.Request) {
			var body signupReq
			if code, msg, err := decodeJSON(r, &body); err != nil {
				writeError(w, code, msg)
				return
			}
			if body.Email == "" {
				writeError(w, http.StatusUnprocessableEntity, "email is required")
				return
			}
			email, ok := normalizeEmail(body.Email)
			if !ok {
				writeError(w, http.StatusUnprocessableEntity, "invalid email")
				return
			}
			secret := app.Config.UserJWTSecret
			if secret == "" {
				writeError(w, http.StatusServiceUnavailable, "user auth not configured")
				return
			}

			ctx, cancel := requestCtxWithTimeout(r, 5*time.Second)
			defer cancel()
			existing, err := app.UsersRepo.GetByEmail(ctx, email)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "users lookup failed")
				return
			}
			var user *gateway.User
			created := false
			if existing != nil {
				user = existing
			} else {
				user, err = app.UsersRepo.Create(ctx, email)
				if err != nil {
					writeError(w, http.StatusInternalServerError, "user create failed")
					return
				}
				created = true
			}
			tok, err := gateway.IssueUserToken(user.UserID, user.Email, secret, app.UserJWTTTL())
			if err != nil {
				writeError(w, http.StatusInternalServerError, "token issue failed")
				return
			}
			writeJSON(w, http.StatusCreated, map[string]any{
				"user_id": user.UserID,
				"email":   user.Email,
				"token":   tok,
				"created": created,
			})
		})

		ar.Post("/login", func(w http.ResponseWriter, r *http.Request) {
			var body loginReq
			if code, msg, err := decodeJSON(r, &body); err != nil {
				writeError(w, code, msg)
				return
			}
			if body.Email == "" || body.MagicCode == "" {
				writeError(w, http.StatusUnprocessableEntity, "email and magic_code are required")
				return
			}
			email, ok := normalizeEmail(body.Email)
			if !ok {
				writeError(w, http.StatusUnprocessableEntity, "invalid email")
				return
			}
			if !app.Config.BypassLogin {
				writeError(w, http.StatusUnauthorized,
					"login is disabled (set BYPASS_LOGIN=1 to enable stub)")
				return
			}
			if body.MagicCode != "BYPASS" {
				writeError(w, http.StatusUnauthorized, "invalid magic code")
				return
			}
			secret := app.Config.UserJWTSecret
			if secret == "" {
				writeError(w, http.StatusServiceUnavailable, "user auth not configured")
				return
			}

			ctx, cancel := requestCtxWithTimeout(r, 5*time.Second)
			defer cancel()
			user, err := app.UsersRepo.GetByEmail(ctx, email)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "users lookup failed")
				return
			}
			if user == nil {
				user, err = app.UsersRepo.Create(ctx, email)
				if err != nil {
					writeError(w, http.StatusInternalServerError, "user create failed")
					return
				}
			}
			tok, err := gateway.IssueUserToken(user.UserID, user.Email, secret, app.UserJWTTTL())
			if err != nil {
				writeError(w, http.StatusInternalServerError, "token issue failed")
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"user_id": user.UserID,
				"email":   user.Email,
				"token":   tok,
			})
		})

		ar.Group(func(pr chi.Router) {
			pr.Use(gateway.UserAuthMiddleware(app.Config.UserJWTSecret))
			pr.Get("/me", func(w http.ResponseWriter, r *http.Request) {
				p := gateway.PrincipalFromContext(r.Context())
				if p == nil {
					writeError(w, http.StatusUnauthorized, "unauthenticated")
					return
				}
				writeJSON(w, http.StatusOK, map[string]string{
					"user_id": p.UserID,
					"email":   p.Email,
				})
			})
		})
	})
}
