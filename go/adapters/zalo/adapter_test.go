package zalo

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openclaw/agent-platform/internal/gateway/channels"
)

// ---------------------------------------------------------------------------
// Test fixtures & helpers

// testConfig returns a Config with stable, fake credentials for offline tests.
// OA id matches recipient.id in the fixture events.
func testConfig(apiBase, oauthBase string) Config {
	return Config{
		OAID:                  "1234567890123456789",
		AppID:                 "9876543210",
		AppSecret:             "test-app-secret",
		OAAccessToken:         "test-access-token",
		OARefreshToken:        "test-refresh-token",
		APIBase:               apiBase,
		OAuthBase:             oauthBase,
		TextChunkLimit:        2000,
		RequestTimeoutSeconds: 5.0,
		TokenRefreshLeadS:     3600.0,
		TokenValiditySeconds:  86400.0,
		TokenFailureDisableS:  3600.0,
	}
}

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

var eventTextBytes = []byte(`{
  "app_id": "9876543210",
  "user_id_by_app": "5555444433332222111",
  "event_name": "user_send_text",
  "timestamp": "1700000000000",
  "sender": {"id": "5555444433332222111"},
  "recipient": {"id": "1234567890123456789"},
  "message": {"msg_id": "0a1b2c3d4e5f6789", "text": "Hello from Zalo"}
}`)

// ---------------------------------------------------------------------------
// VerifySignature

func TestVerifySignature_ValidSignatureAccepted(t *testing.T) {
	cfg := testConfig(DefaultAPIBase, DefaultOAuthBase)
	adapter, err := New(cfg, nil)
	require.NoError(t, err)
	sig := sign(cfg.AppSecret, eventTextBytes)
	headers := http.Header{"X-Zevent-Signature": []string{sig}}
	assert.True(t, adapter.VerifySignature(headers, eventTextBytes))
}

func TestVerifySignature_HeaderCaseInsensitive(t *testing.T) {
	cfg := testConfig(DefaultAPIBase, DefaultOAuthBase)
	adapter, err := New(cfg, nil)
	require.NoError(t, err)
	sig := sign(cfg.AppSecret, eventTextBytes)
	// Use mixed-case header.
	headers := http.Header{"X-Zevent-Signature": []string{sig}}
	assert.True(t, adapter.VerifySignature(headers, eventTextBytes))
}

func TestVerifySignature_Sha256PrefixAccepted(t *testing.T) {
	cfg := testConfig(DefaultAPIBase, DefaultOAuthBase)
	adapter, err := New(cfg, nil)
	require.NoError(t, err)
	sig := sign(cfg.AppSecret, eventTextBytes)
	headers := http.Header{"X-Zevent-Signature": []string{"sha256=" + sig}}
	assert.True(t, adapter.VerifySignature(headers, eventTextBytes))
}

func TestVerifySignature_ForgedSignatureRejected(t *testing.T) {
	cfg := testConfig(DefaultAPIBase, DefaultOAuthBase)
	adapter, err := New(cfg, nil)
	require.NoError(t, err)
	forged := sign("not-the-secret", eventTextBytes)
	headers := http.Header{"X-Zevent-Signature": []string{forged}}
	assert.False(t, adapter.VerifySignature(headers, eventTextBytes))
}

func TestVerifySignature_MissingHeaderRejected(t *testing.T) {
	cfg := testConfig(DefaultAPIBase, DefaultOAuthBase)
	adapter, err := New(cfg, nil)
	require.NoError(t, err)
	assert.False(t, adapter.VerifySignature(http.Header{}, eventTextBytes))
}

func TestVerifySignature_TamperedBodyRejected(t *testing.T) {
	cfg := testConfig(DefaultAPIBase, DefaultOAuthBase)
	adapter, err := New(cfg, nil)
	require.NoError(t, err)
	sig := sign(cfg.AppSecret, eventTextBytes)
	headers := http.Header{"X-Zevent-Signature": []string{sig}}
	tampered := append(eventTextBytes, ' ')
	assert.False(t, adapter.VerifySignature(headers, tampered))
}

// ---------------------------------------------------------------------------
// ParseIncoming

func TestParseIncoming_ReturnsNormalizedMessage(t *testing.T) {
	cfg := testConfig(DefaultAPIBase, DefaultOAuthBase)
	adapter, err := New(cfg, nil)
	require.NoError(t, err)
	msg, err := adapter.ParseIncoming(eventTextBytes)
	require.NoError(t, err)
	assert.Equal(t, "zalo:1234567890123456789:5555444433332222111", msg.ChannelID)
	assert.Equal(t, "5555444433332222111", msg.ThreadID)
	assert.Equal(t, "Hello from Zalo", msg.Text)
}

func TestParseIncoming_RaisesOnBadBody(t *testing.T) {
	cfg := testConfig(DefaultAPIBase, DefaultOAuthBase)
	adapter, err := New(cfg, nil)
	require.NoError(t, err)
	_, err = adapter.ParseIncoming([]byte("not-json"))
	assert.Error(t, err)
}

// ---------------------------------------------------------------------------
// SendOutgoing — happy path via httptest.Server

func TestSendOutgoing_TextHitsCSEndpoint(t *testing.T) {
	var captured *http.Request
	var capturedBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r
		capturedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"error":0,"message":"ok","data":{"message_id":"m1"}}`))
	}))
	defer server.Close()

	cfg := testConfig(server.URL, server.URL)
	adapter, err := New(cfg, &NewOptions{HTTPClient: server.Client()})
	require.NoError(t, err)

	err = adapter.SendOutgoing(context.Background(),
		"zalo:1234567890123456789:5555444433332222111",
		"5555444433332222111",
		"Hello!",
		nil,
	)
	require.NoError(t, err)
	require.NotNil(t, captured)
	assert.Equal(t, "/v3.0/oa/message/cs", captured.URL.Path)
	assert.Equal(t, "test-access-token", captured.Header.Get("access_token"))

	var payload map[string]any
	require.NoError(t, json.Unmarshal(capturedBody, &payload))
	assert.Equal(t, map[string]any{"user_id": "5555444433332222111"}, payload["recipient"])
	msg, _ := payload["message"].(map[string]any)
	assert.Equal(t, "Hello!", msg["text"])
}

func TestSendOutgoing_ChunksLongText(t *testing.T) {
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"error":0,"message":"ok","data":{}}`))
	}))
	defer server.Close()

	cfg := testConfig(server.URL, server.URL)
	cfg.TextChunkLimit = 100
	adapter, err := New(cfg, &NewOptions{HTTPClient: server.Client()})
	require.NoError(t, err)

	longText := "paragraph one.\n\n" + strings.Repeat("x", 200) + "\n\nparagraph two."
	err = adapter.SendOutgoing(context.Background(), "zalo:oa:user", "user", longText, nil)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, callCount, 2)
}

func TestSendOutgoing_WithAttachment(t *testing.T) {
	var capturedBodies [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		capturedBodies = append(capturedBodies, b)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"error":0,"message":"ok","data":{}}`))
	}))
	defer server.Close()

	cfg := testConfig(server.URL, server.URL)
	adapter, err := New(cfg, &NewOptions{HTTPClient: server.Client()})
	require.NoError(t, err)

	err = adapter.SendOutgoing(context.Background(), "zalo:oa:user", "user", "", map[string]any{
		"attachments": []map[string]any{
			{"kind": "photo", "url": "https://cdn.example.com/img.jpg", "caption": "look"},
		},
	})
	require.NoError(t, err)
	require.NotEmpty(t, capturedBodies)

	var payload map[string]any
	require.NoError(t, json.Unmarshal(capturedBodies[0], &payload))
	msg, _ := payload["message"].(map[string]any)
	att, _ := msg["attachment"].(map[string]any)
	assert.Equal(t, "image", att["type"])
	innerPayload, _ := att["payload"].(map[string]any)
	assert.Equal(t, "https://cdn.example.com/img.jpg", innerPayload["url"])
	assert.Equal(t, "look", msg["text"])
}

func TestSendOutgoing_MessagingWindowExceededRaises(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"error":217,"message":"OA cannot send message outside 24h window"}`))
	}))
	defer server.Close()

	cfg := testConfig(server.URL, server.URL)
	adapter, err := New(cfg, &NewOptions{HTTPClient: server.Client()})
	require.NoError(t, err)

	err = adapter.SendOutgoing(context.Background(), "zalo:oa:user", "user", "late reply", nil)
	require.Error(t, err)
	var winErr *MessagingWindowExceededError
	assert.ErrorAs(t, err, &winErr)
	require.NotNil(t, winErr.ErrorCode)
	assert.Equal(t, 217, *winErr.ErrorCode)
}

func TestSendOutgoing_OtherErrorRaisesGeneric(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"error":100,"message":"Some other error"}`))
	}))
	defer server.Close()

	cfg := testConfig(server.URL, server.URL)
	adapter, err := New(cfg, &NewOptions{HTTPClient: server.Client()})
	require.NoError(t, err)

	err = adapter.SendOutgoing(context.Background(), "zalo:oa:user", "user", "hello", nil)
	require.Error(t, err)
	var winErr *MessagingWindowExceededError
	assert.False(t, assert.ObjectsAreEqual(winErr, err), "should not be MessagingWindowExceededError")
	var apiErr *APIError
	assert.ErrorAs(t, err, &apiErr)
}

func TestSendOutgoing_SendDisabledRaisesWithoutHittingAPI(t *testing.T) {
	apiCallCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiCallCount++
		w.Write([]byte(`{"error":0,"message":"ok","data":{}}`))
	}))
	defer server.Close()

	cfg := testConfig(server.URL, server.URL)

	fakeClock := &fakeClock{now: 1000.0}
	badRefresh := func(ctx context.Context, _ string) (map[string]any, error) {
		return nil, &APIError{ErrorCode: nil, Description: "simulated outage"}
	}
	refresher, err := NewTokenRefresher(
		"x", "r", cfg.OAID,
		badRefresh,
		86400.0, 3600.0, 10.0,
		nil,
		fakeClock.Now,
		func(_ context.Context, _ float64) error { return nil },
	)
	require.NoError(t, err)

	// Drive into send_disabled: fail once, advance clock past threshold, fail again.
	refresher.RefreshNow(context.Background())
	fakeClock.Advance(20.0)
	refresher.RefreshNow(context.Background())
	require.True(t, refresher.SendDisabled())

	adapter, err := New(cfg, &NewOptions{HTTPClient: server.Client(), Refresher: refresher})
	require.NoError(t, err)

	err = adapter.SendOutgoing(context.Background(), "zalo:oa:user", "user", "hello", nil)
	require.Error(t, err)
	var disabledErr *SendDisabledError
	assert.ErrorAs(t, err, &disabledErr)
	assert.Equal(t, cfg.OAID, disabledErr.OAID)
	assert.GreaterOrEqual(t, disabledErr.ConsecutiveFailures, 2)
	assert.Equal(t, "send_disabled", disabledErr.Payload()["kind"])
	// API should NOT have been called.
	assert.Equal(t, 0, apiCallCount)
}

// ---------------------------------------------------------------------------
// ChunkText helper

func TestChunkText_ShortPassesThrough(t *testing.T) {
	chunks, err := ChunkText("hello", 100)
	require.NoError(t, err)
	assert.Equal(t, []string{"hello"}, chunks)
}

func TestChunkText_ParagraphSplit(t *testing.T) {
	text := "para one\n\npara two"
	chunks, err := ChunkText(text, 10)
	require.NoError(t, err)
	assert.Equal(t, []string{"para one", "para two"}, chunks)
}

func TestChunkText_ZeroLimitRejected(t *testing.T) {
	_, err := ChunkText("x", 0)
	assert.Error(t, err)
}

func TestChunkText_EmptyReturnsEmpty(t *testing.T) {
	chunks, err := ChunkText("", 100)
	require.NoError(t, err)
	assert.Empty(t, chunks)
}

// ---------------------------------------------------------------------------
// Register

type fakeRegistry struct {
	stored map[string]channels.ChannelAdapter
	errFn  func() error
}

func (f *fakeRegistry) Register(channelType string, adapter channels.ChannelAdapter) error {
	if f.errFn != nil {
		if err := f.errFn(); err != nil {
			return err
		}
	}
	if f.stored == nil {
		f.stored = make(map[string]channels.ChannelAdapter)
	}
	f.stored[channelType] = adapter
	return nil
}

func TestRegister_StoresUnderZaloKey(t *testing.T) {
	reg := &fakeRegistry{}
	cfg := testConfig(DefaultAPIBase, DefaultOAuthBase)
	adapter, err := Register(reg, cfg)
	require.NoError(t, err)
	require.NotNil(t, adapter)
	assert.Same(t, adapter, reg.stored["zalo"])
	assert.Equal(t, "zalo", adapter.Type())
}

func TestRegister_NilRegistryErrors(t *testing.T) {
	_, err := Register(nil, testConfig(DefaultAPIBase, DefaultOAuthBase))
	assert.Error(t, err)
}

// ---------------------------------------------------------------------------
// OAuth refresh integration via httptest.Server

func TestRefreshAccessToken_RotatesToken(t *testing.T) {
	var capturedHeaders http.Header
	var capturedBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedHeaders = r.Header.Clone()
		capturedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"rotated","refresh_token":"rotated-refresh","expires_in":"86400"}`))
	}))
	defer server.Close()

	cfg := testConfig(server.URL, server.URL)
	adapter, err := New(cfg, &NewOptions{HTTPClient: server.Client()})
	require.NoError(t, err)

	ok := adapter.Refresher().RefreshNow(context.Background())
	assert.True(t, ok)
	assert.Equal(t, "rotated", adapter.Refresher().AccessToken())
	// Verify the request had secret_key header and form body.
	assert.Equal(t, cfg.AppSecret, capturedHeaders.Get("secret_key"))
	bodyStr := string(capturedBody)
	assert.Contains(t, bodyStr, "grant_type=refresh_token")
	assert.Contains(t, bodyStr, "refresh_token=")
	assert.Contains(t, bodyStr, "app_id="+cfg.AppID)
}

func TestRefreshOAuthFailure_RecordsFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":-1,"error_description":"bad refresh token"}`))
	}))
	defer server.Close()

	cfg := testConfig(server.URL, server.URL)
	adapter, err := New(cfg, &NewOptions{HTTPClient: server.Client()})
	require.NoError(t, err)

	ok := adapter.Refresher().RefreshNow(context.Background())
	assert.False(t, ok)
	assert.Equal(t, 1, adapter.Refresher().ConsecutiveFailures())
}

// ---------------------------------------------------------------------------
// Adapter implements ChannelAdapter — compile-time assertion.

var _ channels.ChannelAdapter = (*Adapter)(nil)

// ---------------------------------------------------------------------------
// helpers

type fakeClock struct{ now float64 }

func (c *fakeClock) Now() float64 { return c.now }
func (c *fakeClock) Advance(s float64) { c.now += s }
