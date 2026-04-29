package zalo

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Test helpers

type testClock struct{ now float64 }

func newTestClock(start float64) *testClock { return &testClock{now: start} }
func (c *testClock) Now() float64           { return c.now }
func (c *testClock) Advance(s float64)      { c.now += s }

type alertRecorder struct{ alerts []TokenRefreshAlert }

func (r *alertRecorder) Record(a TokenRefreshAlert) { r.alerts = append(r.alerts, a) }

func makeRefresher(
	t *testing.T,
	refresh RefreshFunc,
	clock *testClock,
	ar *alertRecorder,
	validityS, leadS, disableS float64,
) *TokenRefresher {
	t.Helper()
	noopSleep := func(_ context.Context, _ float64) error { return nil }
	tr, err := NewTokenRefresher(
		"initial-access", "initial-refresh", "oa-test",
		refresh,
		validityS, leadS, disableS,
		ar.Record,
		clock.Now,
		noopSleep,
	)
	require.NoError(t, err)
	return tr
}

func successRefresh(_ context.Context, _ string) (map[string]any, error) {
	return map[string]any{
		"access_token":  "rotated-access",
		"refresh_token": "rotated-refresh",
		"expires_in":    "86400",
	}, nil
}

// ---------------------------------------------------------------------------
// Successful refresh

func TestRefresher_SuccessfulRefreshRotatesTokenPair(t *testing.T) {
	clock := newTestClock(1000.0)
	ar := &alertRecorder{}

	tr := makeRefresher(t, successRefresh, clock, ar, 86400.0, 3600.0, 3600.0)
	ok := tr.RefreshNow(context.Background())

	assert.True(t, ok)
	assert.Equal(t, "rotated-access", tr.AccessToken())
	assert.Equal(t, "rotated-refresh", tr.RefreshToken())
	assert.Equal(t, 0, tr.ConsecutiveFailures())
	assert.False(t, tr.SendDisabled())
	// No alerts on clean first-time success.
	assert.Empty(t, ar.alerts)
}

func TestRefresher_TokenProviderReturnsCurrentAccessToken(t *testing.T) {
	clock := newTestClock(1000.0)
	ar := &alertRecorder{}
	tr := makeRefresher(t, successRefresh, clock, ar, 86400.0, 3600.0, 3600.0)

	assert.Equal(t, "initial-access", tr.TokenProvider()())
	tr.RefreshNow(context.Background())
	assert.Equal(t, "rotated-access", tr.TokenProvider()())
}

// ---------------------------------------------------------------------------
// Failure / backoff / send_disabled

func TestRefresher_FailureEmitsStructuredAlertWithBackoff(t *testing.T) {
	clock := newTestClock(1000.0)
	ar := &alertRecorder{}

	badRefresh := func(_ context.Context, _ string) (map[string]any, error) {
		return nil, &APIError{Description: "boom"}
	}
	tr := makeRefresher(t, badRefresh, clock, ar, 86400.0, 3600.0, 3600.0)
	ok := tr.RefreshNow(context.Background())

	assert.False(t, ok)
	assert.Equal(t, 1, tr.ConsecutiveFailures())
	assert.False(t, tr.SendDisabled())
	require.Len(t, ar.alerts, 1)
	fa := ar.alerts[0]
	assert.Equal(t, AlertKindFailure, fa.Kind)
	assert.Equal(t, 1, fa.Attempt)
	require.NotNil(t, fa.NextRetryInS)
	assert.Equal(t, 60.0, *fa.NextRetryInS)
	assert.Contains(t, fa.Error, "boom")
	assert.Equal(t, "oa-test", fa.OAID)
}

func TestRefresher_BackoffProgressesThroughSchedule(t *testing.T) {
	clock := newTestClock(1000.0)
	ar := &alertRecorder{}

	badRefresh := func(_ context.Context, _ string) (map[string]any, error) {
		return nil, &APIError{Description: "still broken"}
	}
	tr := makeRefresher(t, badRefresh, clock, ar, 86400.0, 3600.0, 86400.0)
	for i := 0; i < 4; i++ {
		tr.RefreshNow(context.Background())
	}

	failureAlerts := filterAlerts(ar.alerts, AlertKindFailure)
	require.Len(t, failureAlerts, 4)
	backoffs := make([]float64, len(failureAlerts))
	for i, a := range failureAlerts {
		require.NotNil(t, a.NextRetryInS)
		backoffs[i] = *a.NextRetryInS
	}
	assert.Equal(t, []float64{60.0, 120.0, 240.0, 480.0}, backoffs)
}

func TestRefresher_SendDisabledAfterFailureWindow(t *testing.T) {
	clock := newTestClock(1000.0)
	ar := &alertRecorder{}

	badRefresh := func(_ context.Context, _ string) (map[string]any, error) {
		return nil, &APIError{Description: "oauth down"}
	}
	tr := makeRefresher(t, badRefresh, clock, ar, 86400.0, 3600.0, 3600.0)

	// First failure at t=0; not yet disabled.
	tr.RefreshNow(context.Background())
	assert.False(t, tr.SendDisabled())

	// Advance to just before threshold.
	clock.Advance(3599.0)
	tr.RefreshNow(context.Background())
	assert.False(t, tr.SendDisabled())

	// One more second past the threshold.
	clock.Advance(2.0)
	tr.RefreshNow(context.Background())
	assert.True(t, tr.SendDisabled())

	disableAlerts := filterAlerts(ar.alerts, AlertKindSendDisabled)
	require.Len(t, disableAlerts, 1)
	assert.Equal(t, 3, disableAlerts[0].Attempt)
	fw, _ := disableAlerts[0].Extra["failure_window_s"].(float64)
	assert.GreaterOrEqual(t, fw, 3600.0)
}

func TestRefresher_RecoveryEmitsRecoveredAndSendEnabledAlerts(t *testing.T) {
	clock := newTestClock(1000.0)
	ar := &alertRecorder{}

	callCount := 0
	refresh := func(_ context.Context, _ string) (map[string]any, error) {
		callCount++
		if callCount <= 3 {
			return nil, &APIError{Description: "transient"}
		}
		return map[string]any{
			"access_token":  "recovered-access",
			"refresh_token": "recovered-refresh",
			"expires_in":    "86400",
		}, nil
	}
	tr := makeRefresher(t, refresh, clock, ar, 86400.0, 3600.0, 3600.0)

	tr.RefreshNow(context.Background())
	clock.Advance(1800.0)
	tr.RefreshNow(context.Background())
	clock.Advance(1900.0)
	tr.RefreshNow(context.Background())
	assert.True(t, tr.SendDisabled())

	ok := tr.RefreshNow(context.Background())
	assert.True(t, ok)
	assert.False(t, tr.SendDisabled())
	assert.Equal(t, 0, tr.ConsecutiveFailures())
	assert.Equal(t, "recovered-access", tr.AccessToken())

	kinds := alertKinds(ar.alerts)
	assert.Contains(t, kinds, string(AlertKindRecovered))
	assert.Contains(t, kinds, string(AlertKindSendEnabled))

	// send_enabled fires before recovered.
	seIdx := indexOfKind(ar.alerts, AlertKindSendEnabled)
	recIdx := indexOfKind(ar.alerts, AlertKindRecovered)
	assert.Less(t, seIdx, recIdx)
}

func TestRefresher_MalformedResponseCountedAsFailure(t *testing.T) {
	clock := newTestClock(1000.0)
	ar := &alertRecorder{}

	badRefresh := func(_ context.Context, _ string) (map[string]any, error) {
		return map[string]any{"unexpected": "shape"}, nil
	}
	tr := makeRefresher(t, badRefresh, clock, ar, 86400.0, 3600.0, 3600.0)
	ok := tr.RefreshNow(context.Background())

	assert.False(t, ok)
	assert.Equal(t, 1, tr.ConsecutiveFailures())
	assert.True(t, hasAlertKind(ar.alerts, AlertKindFailure))
}

func TestRefresher_EmptyAccessTokenCountedAsFailure(t *testing.T) {
	clock := newTestClock(1000.0)
	ar := &alertRecorder{}

	badRefresh := func(_ context.Context, _ string) (map[string]any, error) {
		return map[string]any{"access_token": "", "refresh_token": "x"}, nil
	}
	tr := makeRefresher(t, badRefresh, clock, ar, 86400.0, 3600.0, 3600.0)
	ok := tr.RefreshNow(context.Background())

	assert.False(t, ok)
	assert.True(t, hasAlertKind(ar.alerts, AlertKindFailure))
}

// ---------------------------------------------------------------------------
// Scheduling

func TestRefresher_FirstWaitTargetsLeadWindowBeforeExpiry(t *testing.T) {
	clock := newTestClock(1000.0)
	ar := &alertRecorder{}

	tr := makeRefresher(t, successRefresh, clock, ar, 86400.0, 3600.0, 3600.0)
	// validity - lead = 86400 - 3600 = 82800 seconds.
	assert.InDelta(t, 82800.0, tr.NextWaitS(), 0.001)

	// After 1h elapsed, the wait drops by 1h.
	clock.Advance(3600.0)
	assert.InDelta(t, 79200.0, tr.NextWaitS(), 0.001)
}

func TestRefresher_InvalidLeadVsValidityRejected(t *testing.T) {
	noopSleep := func(_ context.Context, _ float64) error { return nil }
	_, err := NewTokenRefresher(
		"a", "r", "oa",
		successRefresh,
		300.0, 600.0, 3600.0, // lead > validity
		nil,
		newTestClock(0).Now,
		noopSleep,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "token_validity_s must be > token_refresh_lead_s")
}

// ---------------------------------------------------------------------------
// Start / Stop lifecycle

func TestRefresher_StartAndStop_Idempotent(t *testing.T) {
	clock := newTestClock(1000.0)
	ar := &alertRecorder{}

	// sleepFn blocks until ctx is cancelled so Stop() can reliably terminate
	// the loop goroutine without a tight busy loop.
	sleepFn := func(ctx context.Context, _ float64) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	tr, err := NewTokenRefresher(
		"a", "r", "oa",
		successRefresh,
		10.0, 5.0, 3600.0,
		ar.Record,
		clock.Now,
		sleepFn,
	)
	require.NoError(t, err)

	ctx := context.Background()
	tr.Start(ctx)
	tr.Start(ctx) // idempotent — second Start must not spawn a second goroutine
	tr.Stop()
	tr.Stop() // idempotent
}

// ---------------------------------------------------------------------------
// helpers

func filterAlerts(alerts []TokenRefreshAlert, kind AlertKind) []TokenRefreshAlert {
	var out []TokenRefreshAlert
	for _, a := range alerts {
		if a.Kind == kind {
			out = append(out, a)
		}
	}
	return out
}

func hasAlertKind(alerts []TokenRefreshAlert, kind AlertKind) bool {
	for _, a := range alerts {
		if a.Kind == kind {
			return true
		}
	}
	return false
}

func alertKinds(alerts []TokenRefreshAlert) []string {
	kinds := make([]string, len(alerts))
	for i, a := range alerts {
		kinds[i] = string(a.Kind)
	}
	return kinds
}

func indexOfKind(alerts []TokenRefreshAlert, kind AlertKind) int {
	for i, a := range alerts {
		if a.Kind == kind {
			return i
		}
	}
	return -1
}
