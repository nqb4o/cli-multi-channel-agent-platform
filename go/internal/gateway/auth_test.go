package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

func TestAdminAuth_FailsClosedWhenTokenUnset(t *testing.T) {
	mw := AdminAuthMiddleware("")
	srv := httptest.NewServer(mw(okHandler()))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	req.Header.Set("Authorization", "Bearer anything")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", resp.StatusCode)
	}
}

func TestAdminAuth_MissingBearerReturns401(t *testing.T) {
	mw := AdminAuthMiddleware("expected")
	srv := httptest.NewServer(mw(okHandler()))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
	if resp.Header.Get("WWW-Authenticate") == "" {
		t.Fatal("WWW-Authenticate should be set on 401")
	}
}

func TestAdminAuth_MalformedHeaderReturns401(t *testing.T) {
	mw := AdminAuthMiddleware("expected")
	srv := httptest.NewServer(mw(okHandler()))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	req.Header.Set("Authorization", "Token nope")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestAdminAuth_BadTokenReturns403(t *testing.T) {
	mw := AdminAuthMiddleware("expected")
	srv := httptest.NewServer(mw(okHandler()))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	req.Header.Set("Authorization", "Bearer wrong")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", resp.StatusCode)
	}
}

func TestAdminAuth_GoodTokenPassesThrough(t *testing.T) {
	mw := AdminAuthMiddleware("expected")
	srv := httptest.NewServer(mw(okHandler()))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	req.Header.Set("Authorization", "Bearer expected")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

func TestExtractBearer_HappyPath(t *testing.T) {
	tok, ok := extractBearer("Bearer xyz")
	if !ok || tok != "xyz" {
		t.Fatalf("got (%q, %v)", tok, ok)
	}
}

func TestExtractBearer_NotBearer(t *testing.T) {
	if _, ok := extractBearer("Basic xyz"); ok {
		t.Fatal("non-bearer should fail")
	}
}

func TestExtractBearer_Empty(t *testing.T) {
	if _, ok := extractBearer(""); ok {
		t.Fatal("empty should fail")
	}
	if _, ok := extractBearer("Bearer "); ok {
		t.Fatal("empty token should fail")
	}
}
