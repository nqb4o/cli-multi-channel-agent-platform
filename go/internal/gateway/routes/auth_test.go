package routes

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func bearerHeaders(tok string) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+tok)
	return h
}

// ---------------------------------------------------------------------------
// /auth/signup
// ---------------------------------------------------------------------------

func TestSignup_HappyPath(t *testing.T) {
	env := newTestEnv(t, defaultEnvOpts())
	rec := postJSON(t, env, "/auth/signup", nil,
		map[string]string{"email": "new@example.test"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["email"] != "new@example.test" {
		t.Fatalf("email=%v", body["email"])
	}
	if body["user_id"].(string) == "" || body["token"].(string) == "" {
		t.Fatalf("missing fields: %v", body)
	}
	if body["created"] != true {
		t.Fatalf("created should be true, got %v", body["created"])
	}
}

func TestSignup_IdempotentForExistingEmail(t *testing.T) {
	env := newTestEnv(t, defaultEnvOpts())
	r1 := postJSON(t, env, "/auth/signup", nil,
		map[string]string{"email": "dup@example.test"})
	r2 := postJSON(t, env, "/auth/signup", nil,
		map[string]string{"email": "dup@example.test"})

	var b1, b2 map[string]any
	_ = json.Unmarshal(r1.Body.Bytes(), &b1)
	_ = json.Unmarshal(r2.Body.Bytes(), &b2)
	if b1["user_id"] != b2["user_id"] {
		t.Fatalf("user_id should be stable: %v vs %v", b1["user_id"], b2["user_id"])
	}
	if b2["created"] != false {
		t.Fatalf("second signup should be created=false, got %v", b2["created"])
	}
}

func TestSignup_NormalizesEmailCase(t *testing.T) {
	env := newTestEnv(t, defaultEnvOpts())
	r1 := postJSON(t, env, "/auth/signup", nil,
		map[string]string{"email": "MixedCase@Example.test"})
	r2 := postJSON(t, env, "/auth/signup", nil,
		map[string]string{"email": "mixedcase@example.test"})
	var b1, b2 map[string]any
	_ = json.Unmarshal(r1.Body.Bytes(), &b1)
	_ = json.Unmarshal(r2.Body.Bytes(), &b2)
	if b1["user_id"] != b2["user_id"] {
		t.Fatalf("normalised email mismatch: %v vs %v", b1["user_id"], b2["user_id"])
	}
}

func TestSignup_RejectsMalformedEmail(t *testing.T) {
	env := newTestEnv(t, defaultEnvOpts())
	rec := postJSON(t, env, "/auth/signup", nil,
		map[string]string{"email": "not-an-email"})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d", rec.Code)
	}
}

func TestSignup_RejectsMissingEmail(t *testing.T) {
	env := newTestEnv(t, defaultEnvOpts())
	rec := postJSON(t, env, "/auth/signup", nil, map[string]any{})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d", rec.Code)
	}
}

func TestSignup_503WhenJWTSecretUnset(t *testing.T) {
	opts := defaultEnvOpts()
	opts.jwtSecret = ""
	env := newTestEnv(t, opts)
	rec := postJSON(t, env, "/auth/signup", nil,
		map[string]string{"email": "x@example.test"})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// /auth/login
// ---------------------------------------------------------------------------

func TestLogin_BypassHappyPath(t *testing.T) {
	env := newTestEnv(t, defaultEnvOpts())
	rec := postJSON(t, env, "/auth/login", nil, map[string]string{
		"email": "any@example.test", "magic_code": "BYPASS",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["email"] != "any@example.test" {
		t.Fatalf("email=%v", body["email"])
	}
	if body["token"].(string) == "" {
		t.Fatal("missing token")
	}
}

func TestLogin_BypassWrongCode_401(t *testing.T) {
	env := newTestEnv(t, defaultEnvOpts())
	rec := postJSON(t, env, "/auth/login", nil, map[string]string{
		"email": "any@example.test", "magic_code": "WRONG",
	})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestLogin_DisabledWhenBypassOff(t *testing.T) {
	opts := defaultEnvOpts()
	opts.bypassLogin = false
	env := newTestEnv(t, opts)
	rec := postJSON(t, env, "/auth/login", nil, map[string]string{
		"email": "x@example.test", "magic_code": "BYPASS",
	})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// /auth/me
// ---------------------------------------------------------------------------

func TestMe_WithoutToken_401(t *testing.T) {
	env := newTestEnv(t, defaultEnvOpts())
	req := httptest.NewRequest(http.MethodGet, "/auth/me", nil)
	rec := httptest.NewRecorder()
	env.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestMe_InvalidToken_401(t *testing.T) {
	env := newTestEnv(t, defaultEnvOpts())
	req := httptest.NewRequest(http.MethodGet, "/auth/me", nil)
	req.Header.Set("Authorization", "Bearer not-a-token")
	rec := httptest.NewRecorder()
	env.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestMe_ReturnsAuthedUser(t *testing.T) {
	env := newTestEnv(t, defaultEnvOpts())
	user, tok := env.authedToken("alice@example.test")
	req := httptest.NewRequest(http.MethodGet, "/auth/me", nil)
	for k, vs := range bearerHeaders(tok) {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	rec := httptest.NewRecorder()
	env.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["user_id"] != user.UserID || body["email"] != user.Email {
		t.Fatalf("body=%v want=%+v", body, user)
	}
}

func TestSignupThenUseTokenForMe(t *testing.T) {
	env := newTestEnv(t, defaultEnvOpts())
	rec := postJSON(t, env, "/auth/signup", nil,
		map[string]string{"email": "flow@example.test"})
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	tok := body["token"].(string)

	req := httptest.NewRequest(http.MethodGet, "/auth/me", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec2 := httptest.NewRecorder()
	env.router.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec2.Code)
	}
	var meBody map[string]string
	_ = json.Unmarshal(rec2.Body.Bytes(), &meBody)
	if meBody["email"] != "flow@example.test" {
		t.Fatalf("email mismatch: %v", meBody)
	}
}
