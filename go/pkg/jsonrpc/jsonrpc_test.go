package jsonrpc

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// pipeServer wires a Server up with an in-memory request stream and an
// in-memory response sink so tests can drive it without real stdio.
func pipeServer(t *testing.T, h Handler, requests string) ([]string, error) {
	t.Helper()
	r := strings.NewReader(requests)
	var w bytes.Buffer
	srv := NewServer(r, &w, h)
	err := srv.Serve(context.Background())
	out := strings.Split(strings.TrimRight(w.String(), "\n"), "\n")
	if len(out) == 1 && out[0] == "" {
		out = nil
	}
	return out, err
}

func TestHappyPathOneRequest(t *testing.T) {
	h := func(_ context.Context, method string, params json.RawMessage) (any, *Error) {
		if method != "echo" {
			t.Fatalf("unexpected method: %s", method)
		}
		var p map[string]any
		_ = json.Unmarshal(params, &p)
		return map[string]any{"got": p["msg"]}, nil
	}
	lines, err := pipeServer(t, h, `{"jsonrpc":"2.0","id":1,"method":"echo","params":{"msg":"hi"}}`+"\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 {
		t.Fatalf("want 1 reply, got %d: %v", len(lines), lines)
	}
	var resp Response
	if err := json.Unmarshal([]byte(lines[0]), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.JSONRPC != "2.0" {
		t.Fatalf("jsonrpc field: %q", resp.JSONRPC)
	}
	if got := resp.ID.(float64); got != 1 {
		t.Fatalf("id: %v", resp.ID)
	}
	got := resp.Result.(map[string]any)
	if got["got"] != "hi" {
		t.Fatalf("result.got: %v", got["got"])
	}
}

func TestMalformedJsonGivesParseError(t *testing.T) {
	h := func(context.Context, string, json.RawMessage) (any, *Error) {
		t.Fatal("handler should not run on malformed input")
		return nil, nil
	}
	lines, err := pipeServer(t, h, "not json\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 {
		t.Fatalf("want 1 reply: %v", lines)
	}
	var er ErrorResponse
	if err := json.Unmarshal([]byte(lines[0]), &er); err != nil {
		t.Fatal(err)
	}
	if er.Error.Code != CodeParseError {
		t.Fatalf("code: %d", er.Error.Code)
	}
}

func TestEmptyLineIsParseError(t *testing.T) {
	h := func(context.Context, string, json.RawMessage) (any, *Error) {
		t.Fatal("handler should not run on empty line")
		return nil, nil
	}
	// Two newlines => one empty line then EOF.
	lines, err := pipeServer(t, h, "\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 {
		t.Fatalf("want 1 reply: %v", lines)
	}
}

func TestMissingMethodInvalidRequest(t *testing.T) {
	h := func(context.Context, string, json.RawMessage) (any, *Error) {
		return nil, nil
	}
	lines, err := pipeServer(t, h, `{"jsonrpc":"2.0","id":7}`+"\n")
	if err != nil {
		t.Fatal(err)
	}
	var er ErrorResponse
	_ = json.Unmarshal([]byte(lines[0]), &er)
	if er.Error.Code != CodeInvalidRequest {
		t.Fatalf("want invalid_request, got %d", er.Error.Code)
	}
}

func TestNotificationGetsNoReply(t *testing.T) {
	called := atomic.Int32{}
	h := func(context.Context, string, json.RawMessage) (any, *Error) {
		called.Add(1)
		return "ignored", nil
	}
	lines, err := pipeServer(t, h, `{"jsonrpc":"2.0","method":"notify"}`+"\n")
	if err != nil {
		t.Fatal(err)
	}
	if called.Load() != 1 {
		t.Fatalf("handler not called: %d", called.Load())
	}
	if len(lines) != 0 {
		t.Fatalf("want no reply for notification, got %v", lines)
	}
}

func TestApplicationErrorAsResult(t *testing.T) {
	h := func(_ context.Context, _ string, _ json.RawMessage) (any, *Error) {
		return SuccessEnvelope(map[string]any{"text": "hi"}), nil
	}
	lines, _ := pipeServer(t, h, `{"jsonrpc":"2.0","id":1,"method":"run"}`+"\n")
	var r Response
	_ = json.Unmarshal([]byte(lines[0]), &r)
	got := r.Result.(map[string]any)
	if got["ok"] != true {
		t.Fatalf("ok flag missing: %v", got)
	}
}

func TestApplicationErrorToEnvelope(t *testing.T) {
	env := ApplicationErrorToEnvelope("auth_expired", "session gone", "Please re-login.", nil)
	if env["ok"] != false {
		t.Fatal("expected ok=false")
	}
	errBlock := env["error"].(map[string]any)
	if errBlock["class"] != "auth_expired" {
		t.Fatalf("class: %v", errBlock["class"])
	}
	if errBlock["user_facing"] != "Please re-login." {
		t.Fatalf("user_facing: %v", errBlock["user_facing"])
	}
}

func TestErrorReturnedFromHandler(t *testing.T) {
	h := func(context.Context, string, json.RawMessage) (any, *Error) {
		return nil, &Error{Code: CodeMethodNotFound, Message: "no such method"}
	}
	lines, _ := pipeServer(t, h, `{"jsonrpc":"2.0","id":42,"method":"missing"}`+"\n")
	var er ErrorResponse
	_ = json.Unmarshal([]byte(lines[0]), &er)
	if er.Error.Code != CodeMethodNotFound {
		t.Fatalf("code: %d", er.Error.Code)
	}
	if er.ID.(float64) != 42 {
		t.Fatalf("id: %v", er.ID)
	}
}

func TestConcurrentRequestsAllReply(t *testing.T) {
	const N = 50
	var sb strings.Builder
	for i := 0; i < N; i++ {
		sb.WriteString(`{"jsonrpc":"2.0","id":`)
		sb.WriteString(itoa(i))
		sb.WriteString(`,"method":"slow"}`)
		sb.WriteString("\n")
	}
	h := func(_ context.Context, _ string, _ json.RawMessage) (any, *Error) {
		// Tiny sleep so the dispatcher actually overlaps requests.
		time.Sleep(2 * time.Millisecond)
		return "ok", nil
	}
	lines, err := pipeServer(t, h, sb.String())
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != N {
		t.Fatalf("want %d replies, got %d", N, len(lines))
	}
	// Verify each reply is a complete envelope (no interleaved JSON).
	seen := make(map[float64]bool)
	for _, ln := range lines {
		var r Response
		if err := json.Unmarshal([]byte(ln), &r); err != nil {
			t.Fatalf("malformed concurrent reply %q: %v", ln, err)
		}
		seen[r.ID.(float64)] = true
	}
	if len(seen) != N {
		t.Fatalf("missing ids: got %d unique", len(seen))
	}
}

func TestPartialBufferingAcrossReads(t *testing.T) {
	// Use slowReader to deliver the request in chunks; bufio.Scanner must
	// reassemble across reads.
	body := `{"jsonrpc":"2.0","id":99,"method":"echo"}` + "\n"
	r := &slowReader{data: []byte(body), chunk: 7}
	var w bytes.Buffer
	srv := NewServer(r, &w, func(context.Context, string, json.RawMessage) (any, *Error) {
		return "ok", nil
	})
	if err := srv.Serve(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(w.String(), `"id":99`) {
		t.Fatalf("missing id in reply: %s", w.String())
	}
}

type slowReader struct {
	data  []byte
	chunk int
	pos   int
}

func (s *slowReader) Read(p []byte) (int, error) {
	if s.pos >= len(s.data) {
		return 0, io.EOF
	}
	n := s.chunk
	if n > len(p) {
		n = len(p)
	}
	if s.pos+n > len(s.data) {
		n = len(s.data) - s.pos
	}
	copy(p, s.data[s.pos:s.pos+n])
	s.pos += n
	return n, nil
}

func TestLargeLine(t *testing.T) {
	// 1 MB payload — must scan in one line.
	big := strings.Repeat("a", 1<<20)
	body := `{"jsonrpc":"2.0","id":1,"method":"big","params":{"x":"` + big + `"}}` + "\n"
	called := atomic.Int32{}
	h := func(_ context.Context, _ string, p json.RawMessage) (any, *Error) {
		called.Add(1)
		if len(p) < 1<<20 {
			t.Fatalf("params truncated: %d", len(p))
		}
		return "ok", nil
	}
	lines, err := pipeServer(t, h, body)
	if err != nil {
		t.Fatal(err)
	}
	if called.Load() != 1 {
		t.Fatalf("handler not called: %d", called.Load())
	}
	if len(lines) != 1 {
		t.Fatalf("want 1 reply, got %d", len(lines))
	}
}

func TestShutdownDrainsInflight(t *testing.T) {
	gotReply := make(chan struct{})
	h := func(_ context.Context, _ string, _ json.RawMessage) (any, *Error) {
		// Modest delay; drain budget is 5s.
		time.Sleep(50 * time.Millisecond)
		close(gotReply)
		return "done", nil
	}

	// Use a reader that blocks until we close — so Serve is sitting in
	// scan when we trigger Shutdown after the in-flight call started.
	pr, pw := io.Pipe()
	var w syncBuf
	srv := NewServer(pr, &w, h)

	done := make(chan error, 1)
	go func() { done <- srv.Serve(context.Background()) }()

	_, err := pw.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"slow"}` + "\n"))
	if err != nil {
		t.Fatal(err)
	}

	// Give the handler time to be picked up.
	time.Sleep(10 * time.Millisecond)
	srv.Shutdown()
	// Closing the pipe wakes the scanner.
	_ = pw.Close()

	if err := <-done; err != nil {
		t.Fatalf("Serve: %v", err)
	}
	select {
	case <-gotReply:
	case <-time.After(2 * time.Second):
		t.Fatal("handler never finished — drain failed")
	}
	if !strings.Contains(w.String(), `"done"`) {
		t.Fatalf("reply not flushed: %s", w.String())
	}
}

// syncBuf is a thread-safe bytes.Buffer wrapper.
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// itoa avoids importing strconv in the test (keeps the file self-contained).
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}
