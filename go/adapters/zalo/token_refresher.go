package zalo

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"
)

// backoffBaseSeconds is the failure-mode backoff schedule (60/120/240/480
// seconds), capped at the configured token refresh lead. Mirrors the Python
// adapter so dashboards line up across language ports.
var backoffBaseSeconds = []float64{60.0, 120.0, 240.0, 480.0}

// AlertKind enumerates the state-machine transitions the refresher emits.
type AlertKind string

const (
	AlertKindFailure       AlertKind = "failure"
	AlertKindRecovered     AlertKind = "recovered"
	AlertKindSendDisabled  AlertKind = "send_disabled"
	AlertKindSendEnabled   AlertKind = "send_enabled"
)

// TokenRefreshAlert is the structured event surfaced to AlertCallback. Mirrors
// the Python TokenRefreshAlert dataclass field-for-field so the cross-language
// observability story remains consistent.
type TokenRefreshAlert struct {
	Kind          AlertKind
	OAID          string
	Attempt       int
	NextRetryInS  *float64
	Error         string
	Timestamp     float64
	Extra         map[string]any
}

// AlertCallback consumes alerts. Keep it cheap; it runs synchronously inside
// the refresher loop.
type AlertCallback func(TokenRefreshAlert)

// RefreshFunc takes the current refresh token and returns the OAuth response
// dict {access_token, refresh_token, expires_in}.
type RefreshFunc func(ctx context.Context, refreshToken string) (map[string]any, error)

// TokenRefresher owns the OA access-token / refresh-token pair and rotates it.
//
// The refresher is fully testable offline — callers inject a Clock returning
// "now" plus a SleepFunc the test can gate. Real callers leave both nil.
type TokenRefresher struct {
	mu sync.Mutex

	accessToken  string
	refreshToken string
	oaID         string
	refresh      RefreshFunc
	validityS    float64
	leadS        float64
	disableS     float64
	alert        AlertCallback
	clock        func() float64
	sleep        func(ctx context.Context, seconds float64) error

	issuedAt            float64
	consecutiveFailures int
	firstFailureAt      *float64
	sendDisabled        bool

	loopCancel context.CancelFunc
	loopDone   chan struct{}
}

// NewTokenRefresher constructs a refresher.
func NewTokenRefresher(
	accessToken, refreshToken, oaID string,
	refresh RefreshFunc,
	validityS, leadS, disableS float64,
	alert AlertCallback,
	clock func() float64,
	sleep func(ctx context.Context, seconds float64) error,
) (*TokenRefresher, error) {
	if leadS <= 0 {
		return nil, errors.New("token_refresh_lead_s must be > 0")
	}
	if validityS <= leadS {
		return nil, errors.New(
			"token_validity_s must be > token_refresh_lead_s (otherwise the refresher would fire immediately)",
		)
	}
	if refresh == nil {
		return nil, errors.New("refresh function must be non-nil")
	}
	if clock == nil {
		clock = func() float64 {
			return float64(time.Now().UnixNano()) / float64(time.Second)
		}
	}
	if sleep == nil {
		sleep = realSleep
	}
	tr := &TokenRefresher{
		accessToken:  accessToken,
		refreshToken: refreshToken,
		oaID:         oaID,
		refresh:      refresh,
		validityS:    validityS,
		leadS:        leadS,
		disableS:     disableS,
		alert:        alert,
		clock:        clock,
		sleep:        sleep,
	}
	tr.issuedAt = clock()
	return tr, nil
}

// AccessToken returns the current access token (latest known good).
func (t *TokenRefresher) AccessToken() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.accessToken
}

// RefreshToken returns the current refresh token.
func (t *TokenRefresher) RefreshToken() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.refreshToken
}

// SendDisabled is true iff the failure window has exceeded the configured
// threshold. The adapter checks this before every outbound call.
func (t *TokenRefresher) SendDisabled() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sendDisabled
}

// ConsecutiveFailures returns the running count of refresh failures since the
// last success.
func (t *TokenRefresher) ConsecutiveFailures() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.consecutiveFailures
}

// TokenProvider returns a function suitable for OaAPI(provider=...). Captures
// the refresher so the caller doesn't need to build a closure.
func (t *TokenRefresher) TokenProvider() TokenProvider {
	return func() string { return t.AccessToken() }
}

// RefreshNow runs a single refresh attempt synchronously. Returns true on
// success, false on failure. Updates the internal state machine (failure
// counter, send_disabled flag) and emits the corresponding alert(s).
func (t *TokenRefresher) RefreshNow(ctx context.Context) bool {
	response, err := t.refresh(ctx, t.RefreshToken())
	if err != nil {
		t.onFailure(err)
		return false
	}
	if response == nil {
		t.onFailure(fmt.Errorf("refresh returned nil response"))
		return false
	}
	rawAccess, ok := response["access_token"]
	if !ok {
		t.onFailure(fmt.Errorf("refresh returned malformed response: %v", response))
		return false
	}
	newAccess, ok := rawAccess.(string)
	if !ok || newAccess == "" {
		t.onFailure(fmt.Errorf("refresh returned empty access_token"))
		return false
	}
	newRefresh := t.RefreshToken()
	if v, ok := response["refresh_token"].(string); ok && v != "" {
		newRefresh = v
	}
	expiresIn := t.validityS
	if v, ok := response["expires_in"]; ok {
		if parsed, ok := coerceFloat(v); ok && parsed > 0 {
			expiresIn = parsed
		}
	}

	t.onSuccess(newAccess, newRefresh, expiresIn)
	return true
}

// Start launches the background refresh loop. Idempotent.
func (t *TokenRefresher) Start(ctx context.Context) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.loopCancel != nil {
		return
	}
	loopCtx, cancel := context.WithCancel(ctx)
	t.loopCancel = cancel
	done := make(chan struct{})
	t.loopDone = done
	// Pass done explicitly so the goroutine closes the right channel regardless
	// of whether Stop() has already cleared t.loopDone.
	go t.loop(loopCtx, done)
}

// Stop signals the loop to exit and waits for termination. Idempotent.
func (t *TokenRefresher) Stop() {
	t.mu.Lock()
	cancel := t.loopCancel
	done := t.loopDone
	t.loopCancel = nil
	t.loopDone = nil
	t.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	if done != nil {
		<-done
	}
}

// ---------------------------------------------------------------------------
// internals

func (t *TokenRefresher) loop(ctx context.Context, done chan struct{}) {
	defer close(done)
	for {
		if ctx.Err() != nil {
			return
		}
		wait := t.NextWaitS()
		if err := t.sleep(ctx, wait); err != nil {
			return
		}
		if ctx.Err() != nil {
			return
		}
		t.RefreshNow(ctx)
	}
}

// NextWaitS computes the seconds to wait before the next refresh attempt. Used
// by tests; the loop calls it directly.
func (t *TokenRefresher) NextWaitS() float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.nextWaitSLocked()
}

func (t *TokenRefresher) nextWaitSLocked() float64 {
	if t.consecutiveFailures == 0 {
		elapsed := t.clock() - t.issuedAt
		remaining := t.validityS - t.leadS - elapsed
		if remaining < 0 {
			remaining = 0
		}
		return remaining
	}
	idx := t.consecutiveFailures - 1
	if idx >= len(backoffBaseSeconds) {
		idx = len(backoffBaseSeconds) - 1
	}
	base := backoffBaseSeconds[idx]
	if base > t.leadS {
		return t.leadS
	}
	return base
}

func (t *TokenRefresher) onSuccess(access, refresh string, validityS float64) {
	t.mu.Lock()
	wasFailing := t.consecutiveFailures > 0
	wasDisabled := t.sendDisabled
	t.accessToken = access
	t.refreshToken = refresh
	t.validityS = validityS
	t.issuedAt = t.clock()
	t.consecutiveFailures = 0
	t.firstFailureAt = nil
	if wasDisabled {
		t.sendDisabled = false
	}
	now := t.clock()
	t.mu.Unlock()

	if wasDisabled {
		t.emit(TokenRefreshAlert{
			Kind:      AlertKindSendEnabled,
			OAID:      t.oaID,
			Timestamp: now,
		})
	}
	if wasFailing {
		t.emit(TokenRefreshAlert{
			Kind:      AlertKindRecovered,
			OAID:      t.oaID,
			Timestamp: now,
		})
	}
}

func (t *TokenRefresher) onFailure(err error) {
	t.mu.Lock()
	t.consecutiveFailures++
	now := t.clock()
	if t.firstFailureAt == nil {
		ts := now
		t.firstFailureAt = &ts
	}
	nextRetry := t.nextWaitSLocked()
	elapsedFailure := now - *t.firstFailureAt
	tripped := false
	if !t.sendDisabled && elapsedFailure >= t.disableS {
		t.sendDisabled = true
		tripped = true
	}
	attempt := t.consecutiveFailures
	t.mu.Unlock()

	if tripped {
		t.emit(TokenRefreshAlert{
			Kind:      AlertKindSendDisabled,
			OAID:      t.oaID,
			Attempt:   attempt,
			Error:     shortError(err),
			Timestamp: now,
			Extra: map[string]any{
				"failure_window_s": elapsedFailure,
			},
		})
	}
	t.emit(TokenRefreshAlert{
		Kind:         AlertKindFailure,
		OAID:         t.oaID,
		Attempt:      attempt,
		NextRetryInS: &nextRetry,
		Error:        shortError(err),
		Timestamp:    now,
	})
}

func (t *TokenRefresher) emit(alert TokenRefreshAlert) {
	if t.alert == nil {
		return
	}
	defer func() {
		// Never let an alert handler crash the refresher.
		_ = recover()
	}()
	t.alert(alert)
}

// ---------------------------------------------------------------------------
// utilities

func realSleep(ctx context.Context, seconds float64) error {
	if seconds <= 0 {
		// Yield to scheduler so cancel can win immediately.
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}
	timer := time.NewTimer(time.Duration(seconds * float64(time.Second)))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func coerceFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case string:
		if f, err := strconv.ParseFloat(x, 64); err == nil {
			return f, true
		}
	}
	// json.Number handled via .String() through strconv.
	if n, ok := v.(interface{ String() string }); ok {
		if f, err := strconv.ParseFloat(n.String(), 64); err == nil {
			return f, true
		}
	}
	return 0, false
}

func shortError(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
