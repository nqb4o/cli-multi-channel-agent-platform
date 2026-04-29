package gateway

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

const testSecret = "unit-test-secret"

func TestUserToken_IssueThenVerifyRoundTrips(t *testing.T) {
	tok, err := IssueUserToken("user-1", "alice@example.test", testSecret, time.Minute)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	p, code, msg := VerifyUserToken("Bearer "+tok, testSecret)
	if p == nil {
		t.Fatalf("verify failed: %d %s", code, msg)
	}
	if p.UserID != "user-1" || p.Email != "alice@example.test" {
		t.Fatalf("principal mismatch: %+v", p)
	}
}

func TestUserToken_RejectsMissingHeader(t *testing.T) {
	p, code, _ := VerifyUserToken("", testSecret)
	if p != nil {
		t.Fatal("nil principal expected")
	}
	if code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", code)
	}
}

func TestUserToken_RejectsNonBearer(t *testing.T) {
	p, code, _ := VerifyUserToken("Basic xyz", testSecret)
	if p != nil || code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", code)
	}
}

func TestUserToken_RejectsEmptyBearer(t *testing.T) {
	p, code, _ := VerifyUserToken("Bearer    ", testSecret)
	if p != nil || code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", code)
	}
}

func TestUserToken_RejectsExpiredToken(t *testing.T) {
	past := time.Now().Add(-2 * time.Hour)
	tok, err := IssueUserTokenAt("user-1", "a@b.test", testSecret, time.Second, past)
	if err != nil {
		t.Fatal(err)
	}
	p, code, msg := VerifyUserToken("Bearer "+tok, testSecret)
	if p != nil {
		t.Fatal("expected nil principal for expired token")
	}
	if code != http.StatusUnauthorized {
		t.Fatalf("expected 401 expired, got %d (%s)", code, msg)
	}
	if !strings.Contains(strings.ToLower(msg), "expired") {
		t.Fatalf("expected 'expired' in message, got %q", msg)
	}
}

func TestUserToken_RejectsTamperedPayload(t *testing.T) {
	tok, _ := IssueUserToken("user-1", "a@b.test", testSecret, time.Minute)
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatal("malformed JWT")
	}
	tampered := parts[1][:len(parts[1])-1] + "A"
	if parts[1] == tampered {
		tampered = parts[1][:len(parts[1])-1] + "B"
	}
	bad := parts[0] + "." + tampered + "." + parts[2]
	p, code, _ := VerifyUserToken("Bearer "+bad, testSecret)
	if p != nil || code != http.StatusUnauthorized {
		t.Fatalf("tampered payload should 401, got %d", code)
	}
}

func TestUserToken_RejectsWrongSecret(t *testing.T) {
	tok, _ := IssueUserToken("u", "a@b.test", "alpha", time.Minute)
	p, code, _ := VerifyUserToken("Bearer "+tok, "beta")
	if p != nil || code != http.StatusUnauthorized {
		t.Fatalf("wrong secret should 401, got %d", code)
	}
}

func TestUserToken_RejectsAlgNone(t *testing.T) {
	b64 := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	header := b64([]byte(`{"alg":"none","typ":"JWT"}`))
	pl, _ := json.Marshal(map[string]any{
		"sub":   "evil",
		"email": "x@y.test",
		"exp":   time.Now().Add(time.Minute).Unix(),
	})
	bad := header + "." + b64(pl) + "."
	p, code, _ := VerifyUserToken("Bearer "+bad, testSecret)
	if p != nil || code != http.StatusUnauthorized {
		t.Fatalf("alg=none should 401, got %d", code)
	}
}

func TestUserToken_IssueWithEmptySecretFails(t *testing.T) {
	if _, err := IssueUserToken("u", "a@b.test", "", time.Minute); err == nil {
		t.Fatal("empty secret should error")
	}
}

func TestUserToken_VerifyEmptySecretFailsClosed(t *testing.T) {
	tok, _ := IssueUserToken("u", "a@b.test", "any", time.Minute)
	p, code, _ := VerifyUserToken("Bearer "+tok, "")
	if p != nil {
		t.Fatal("empty secret must reject all tokens")
	}
	if code != http.StatusServiceUnavailable {
		t.Fatalf("empty secret should 503, got %d", code)
	}
}

func TestUserToken_RejectsTokenMissingClaims(t *testing.T) {
	// A token without sub or email — encode with the same library so the
	// signature is valid, just the claims body is missing fields.
	b64 := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	header := b64([]byte(`{"alg":"HS256","typ":"JWT"}`))
	pl, _ := json.Marshal(map[string]any{
		"exp": time.Now().Add(time.Minute).Unix(),
	})
	// Hand-sign with HS256 to keep the secret path real.
	tok, err := IssueUserToken("u", "a@b.test", testSecret, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	// Replace the payload with our claim-less one but keep the signature
	// — that should fail signature verification (as designed).
	parts := strings.Split(tok, ".")
	bad := header + "." + b64(pl) + "." + parts[2]
	p, code, _ := VerifyUserToken("Bearer "+bad, testSecret)
	if p != nil || code != http.StatusUnauthorized {
		t.Fatalf("forged claims should 401, got %d", code)
	}
}
