package repos

import (
	"context"
	"sort"
	"testing"

	"github.com/google/uuid"

	"github.com/openclaw/agent-platform/internal/persistence"
)

func TestRunsLifecycleOKAndError(t *testing.T) {
	pool := freshPool(t)
	users := NewUsersRepo(pool)
	agents := NewAgentsRepo(pool)
	channels, _ := NewChannelsRepo(pool, nil)
	runs := NewRunsRepo(pool)

	ctx := context.Background()
	user, _ := users.Create(ctx, "runs@example.com")
	agent, _ := agents.Create(ctx, user.ID, "ag", "x: 1")
	blob, _ := persistence.Encrypt(fakeKey(), []byte(`{}`))
	chanRow, _ := channels.Register(ctx, user.ID.String(), "telegram", "tg:run:1", blob, agent.ID.String())
	chanUUID, _ := uuid.Parse(chanRow.ChannelID)

	provider := "claude-cli"
	started, err := runs.Start(ctx, user.ID, agent.ID, chanUUID, "thread", &provider)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if started.Status != "accepted" {
		t.Fatalf("expected accepted, got %q", started.Status)
	}

	finished, err := runs.FinishOK(ctx, started.ID, 1234, nil)
	if err != nil {
		t.Fatalf("finish_ok: %v", err)
	}
	if finished == nil || finished.Status != "ok" || finished.LatencyMS == nil || *finished.LatencyMS != 1234 {
		t.Fatalf("unexpected finished: %+v", finished)
	}

	errRun, err := runs.Start(ctx, user.ID, agent.ID, chanUUID, "thread", &provider)
	if err != nil {
		t.Fatalf("start err run: %v", err)
	}
	errDone, err := runs.FinishError(ctx, errRun.ID, 42, "rate_limit", "quota", nil)
	if err != nil {
		t.Fatalf("finish_error: %v", err)
	}
	if errDone == nil || errDone.Status != "error" {
		t.Fatalf("expected error status, got %+v", errDone)
	}
	if errDone.ErrorClass == nil || *errDone.ErrorClass != "rate_limit" {
		t.Fatalf("expected rate_limit, got %v", errDone.ErrorClass)
	}
	if errDone.ErrorMsg == nil || *errDone.ErrorMsg != "quota" {
		t.Fatalf("expected quota, got %v", errDone.ErrorMsg)
	}
}

func TestRunsListRecentForUser(t *testing.T) {
	pool := freshPool(t)
	users := NewUsersRepo(pool)
	agents := NewAgentsRepo(pool)
	channels, _ := NewChannelsRepo(pool, nil)
	runs := NewRunsRepo(pool)

	ctx := context.Background()
	user, _ := users.Create(ctx, "recent@example.com")
	agent, _ := agents.Create(ctx, user.ID, "ag", "x: 1")
	blob, _ := persistence.Encrypt(fakeKey(), []byte(`{}`))
	chanRow, _ := channels.Register(ctx, user.ID.String(), "telegram", "tg:recent:1", blob, agent.ID.String())
	chanUUID, _ := uuid.Parse(chanRow.ChannelID)

	provider := "claude-cli"
	for i := 0; i < 3; i++ {
		r, err := runs.Start(ctx, user.ID, agent.ID, chanUUID, "t", &provider)
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		if _, err := runs.FinishOK(ctx, r.ID, 1, nil); err != nil {
			t.Fatalf("finish_ok: %v", err)
		}
	}
	listing, err := runs.ListRecentForUser(ctx, user.ID, 50)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listing) != 3 {
		t.Fatalf("expected 3, got %d", len(listing))
	}
	for _, r := range listing {
		if r.UserID != user.ID {
			t.Fatalf("foreign run in listing: %+v", r)
		}
	}
	// started_at DESC
	starts := make([]int64, len(listing))
	for i, r := range listing {
		starts[i] = r.StartedAt.UnixNano()
	}
	if !sort.SliceIsSorted(starts, func(i, j int) bool { return starts[i] >= starts[j] }) {
		t.Fatalf("expected started_at DESC, got %v", starts)
	}
}
