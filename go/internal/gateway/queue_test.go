package gateway

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// AgentRunJob FROZEN field set
// ---------------------------------------------------------------------------

func TestAgentRunJob_FieldOrderIsFrozen(t *testing.T) {
	want := []string{
		"run_id", "user_id", "agent_id", "channel_id",
		"thread_id", "message", "received_at",
	}
	if got := AgentRunJobFieldOrder(); !reflect.DeepEqual(got, want) {
		t.Fatalf("frozen field order drift:\n got %v\nwant %v", got, want)
	}
}

func TestAgentRunJob_StructFieldOrderIsFrozen(t *testing.T) {
	// Reflect over the dataclass to assert the Go struct's field order also
	// matches the FROZEN contract. Keeps the Go and Python implementations
	// in lockstep — see services/gateway/README.md.
	st := reflect.TypeOf(AgentRunJob{})
	want := []string{"RunID", "UserID", "AgentID", "ChannelID",
		"ThreadID", "Message", "ReceivedAt"}
	if st.NumField() != len(want) {
		t.Fatalf("expected %d fields, got %d", len(want), st.NumField())
	}
	for i, n := range want {
		if got := st.Field(i).Name; got != n {
			t.Fatalf("field %d: got %s, want %s", i, got, n)
		}
	}
}

func TestAgentRunJob_ToStreamFieldsReturnsStrings(t *testing.T) {
	msg, _ := EncodeMessagePayload(map[string]any{"text": "hi"})
	job := AgentRunJob{
		RunID: "r-1", UserID: "u-1", AgentID: "a-1",
		ChannelID: "c-1", ThreadID: "thread:42",
		Message: msg, ReceivedAt: "2026-04-28T00:00:00Z",
	}
	got := job.ToStreamFields()
	wantKeys := []string{"run_id", "user_id", "agent_id", "channel_id",
		"thread_id", "message", "received_at"}
	for _, k := range wantKeys {
		if _, ok := got[k]; !ok {
			t.Fatalf("missing key %q", k)
		}
	}
	var dec map[string]any
	if err := json.Unmarshal([]byte(got["message"]), &dec); err != nil {
		t.Fatalf("decode message: %v", err)
	}
	if dec["text"] != "hi" {
		t.Fatalf("decoded text wrong: %v", dec)
	}
}

func TestEncodeMessagePayload_RoundTrip(t *testing.T) {
	enc, err := EncodeMessagePayload(map[string]any{"text": "hi", "n": 1})
	if err != nil {
		t.Fatal(err)
	}
	var dec map[string]any
	if err := json.Unmarshal([]byte(enc), &dec); err != nil {
		t.Fatal(err)
	}
	if dec["text"] != "hi" {
		t.Fatalf("text mismatch: %v", dec)
	}
}

func TestEncodeMessagePayload_SortedForStability(t *testing.T) {
	enc, err := EncodeMessagePayload(map[string]any{"b": 1, "a": 2})
	if err != nil {
		t.Fatal(err)
	}
	if enc != `{"a":2,"b":1}` {
		t.Fatalf("expected sorted-key JSON, got %s", enc)
	}
}

func TestEncodeMessagePayload_NestedSorted(t *testing.T) {
	enc, _ := EncodeMessagePayload(map[string]any{
		"z": map[string]any{"y": 1, "x": 2},
		"a": []any{map[string]any{"q": 1, "p": 2}},
	})
	// Expected canonical form: keys sorted at every nested level.
	want := `{"a":[{"p":2,"q":1}],"z":{"x":2,"y":1}}`
	if enc != want {
		t.Fatalf("nested sort mismatch:\n got %s\nwant %s", enc, want)
	}
}

func TestEncodeMessagePayload_NilBecomesEmptyObject(t *testing.T) {
	enc, _ := EncodeMessagePayload(nil)
	if enc != "{}" {
		t.Fatalf("nil payload should encode as {}, got %s", enc)
	}
}

// ---------------------------------------------------------------------------
// AgentRunQueue
// ---------------------------------------------------------------------------

func TestAgentRunQueue_EnqueueReturnsStreamID(t *testing.T) {
	c, _ := newMiniredisClient(t)
	q, err := NewAgentRunQueue(c, "agent:runs")
	if err != nil {
		t.Fatal(err)
	}
	id, err := q.Enqueue(context.Background(), AgentRunJob{
		RunID: "r-1", UserID: "u", AgentID: "a", ChannelID: "c",
		ThreadID: "t", Message: "{}", ReceivedAt: "t",
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if !strings.Contains(id, "-") {
		t.Fatalf("expected stream entry id like 1700-0, got %q", id)
	}
}

func TestAgentRunQueue_EnqueueIncrementsXLEN(t *testing.T) {
	c, _ := newMiniredisClient(t)
	q, _ := NewAgentRunQueue(c, "agent:runs")
	ctx := context.Background()
	if n, _ := q.Length(ctx); n != 0 {
		t.Fatalf("expected empty stream, got %d", n)
	}
	job := AgentRunJob{
		RunID: "r-2", UserID: "u", AgentID: "a", ChannelID: "c",
		ThreadID: "t", Message: "{}", ReceivedAt: "t",
	}
	if _, err := q.Enqueue(ctx, job); err != nil {
		t.Fatal(err)
	}
	if n, _ := q.Length(ctx); n != 1 {
		t.Fatalf("expected len 1, got %d", n)
	}
}

func TestAgentRunQueue_PayloadRoundTrips(t *testing.T) {
	c, _ := newMiniredisClient(t)
	q, _ := NewAgentRunQueue(c, "agent:runs")
	msg, _ := EncodeMessagePayload(map[string]any{"text": "hello", "sender_id": "u"})
	job := AgentRunJob{
		RunID: "r-3", UserID: "u-1", AgentID: "a-1", ChannelID: "c-1",
		ThreadID: "thread:x", Message: msg, ReceivedAt: "2026-04-28T00:00:00Z",
	}
	if _, err := q.Enqueue(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	entries, err := c.XRange(context.Background(), "agent:runs", "-", "+").Result()
	if err != nil {
		t.Fatalf("xrange: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	fields := entries[0].Values
	if fields["run_id"] != "r-3" || fields["user_id"] != "u-1" {
		t.Fatalf("payload mismatch: %v", fields)
	}
	var dec map[string]any
	msgStr, _ := fields["message"].(string)
	if err := json.Unmarshal([]byte(msgStr), &dec); err != nil {
		t.Fatal(err)
	}
	if dec["text"] != "hello" {
		t.Fatalf("text wrong: %v", dec)
	}
}

func TestAgentRunQueue_UsesConfiguredStreamName(t *testing.T) {
	c, _ := newMiniredisClient(t)
	q, _ := NewAgentRunQueue(c, "custom:stream")
	job := AgentRunJob{
		RunID: "x", UserID: "u", AgentID: "a", ChannelID: "c",
		ThreadID: "t", Message: "{}", ReceivedAt: "t",
	}
	ctx := context.Background()
	if _, err := q.Enqueue(ctx, job); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.XLen(ctx, "custom:stream").Result(); got != 1 {
		t.Fatalf("custom:stream len = %d, want 1", got)
	}
	if got, _ := c.XLen(ctx, "agent:runs").Result(); got != 0 {
		t.Fatalf("agent:runs should be empty, got %d", got)
	}
}

func TestAgentRunQueue_EmptyStreamNameRejected(t *testing.T) {
	c, _ := newMiniredisClient(t)
	if _, err := NewAgentRunQueue(c, ""); err == nil {
		t.Fatal("empty streamName should error")
	}
}

func TestAgentRunQueue_MultipleEnqueuesPreserveOrder(t *testing.T) {
	c, _ := newMiniredisClient(t)
	q, _ := NewAgentRunQueue(c, "agent:runs")
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		job := AgentRunJob{
			RunID: "r-" + string(rune('0'+i)), UserID: "u",
			AgentID: "a", ChannelID: "c", ThreadID: "t",
			Message: "{}", ReceivedAt: "t",
		}
		if _, err := q.Enqueue(ctx, job); err != nil {
			t.Fatal(err)
		}
	}
	entries, _ := c.XRange(ctx, "agent:runs", "-", "+").Result()
	want := []string{"r-0", "r-1", "r-2"}
	for i, e := range entries {
		if e.Values["run_id"] != want[i] {
			t.Fatalf("entry %d: got %q, want %q", i, e.Values["run_id"], want[i])
		}
	}
}

func TestAgentRunQueue_StreamNameAccessor(t *testing.T) {
	c, _ := newMiniredisClient(t)
	q, _ := NewAgentRunQueue(c, "my:stream")
	if q.StreamName() != "my:stream" {
		t.Fatalf("StreamName() drift: %q", q.StreamName())
	}
}
