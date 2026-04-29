package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/redis/go-redis/v9"
)

// AgentRunJob is the FROZEN cross-language Redis Stream payload for the
// agent:runs queue. The exact field set + names + string-only values are part
// of the contract between the Go (or Python) producer and the Python (or Go)
// orchestrator consumer. See services/gateway/README.md § "Job payload".
//
// Field order MUST stay synchronised with the Python AgentRunJob dataclass.
type AgentRunJob struct {
	RunID      string
	UserID     string
	AgentID    string
	ChannelID  string
	ThreadID   string
	Message    string // JSON-encoded normalized message payload
	ReceivedAt string // ISO-8601 UTC
}

// agentRunJobFieldOrder is the canonical field order. Tests assert against
// this slice to keep the contract from drifting silently.
var agentRunJobFieldOrder = []string{
	"run_id",
	"user_id",
	"agent_id",
	"channel_id",
	"thread_id",
	"message",
	"received_at",
}

// AgentRunJobFieldOrder returns the frozen field order.
func AgentRunJobFieldOrder() []string {
	out := make([]string, len(agentRunJobFieldOrder))
	copy(out, agentRunJobFieldOrder)
	return out
}

// ToStreamFields renders the job to the flat string-only mapping XADD expects.
func (j AgentRunJob) ToStreamFields() map[string]string {
	return map[string]string{
		"run_id":      j.RunID,
		"user_id":     j.UserID,
		"agent_id":    j.AgentID,
		"channel_id":  j.ChannelID,
		"thread_id":   j.ThreadID,
		"message":     j.Message,
		"received_at": j.ReceivedAt,
	}
}

// EncodeMessagePayload serializes the adapter-provided payload to a sorted-key
// JSON string, byte-for-byte identical to Python's
// `json.dumps(obj, separators=(",",":"), sort_keys=True)`.
func EncodeMessagePayload(payload map[string]any) (string, error) {
	if payload == nil {
		payload = map[string]any{}
	}
	return marshalSortedJSON(payload)
}

// marshalSortedJSON walks the value tree producing canonical JSON (sorted
// object keys, no whitespace) — matches Python's sort_keys=True dumps.
func marshalSortedJSON(v any) (string, error) {
	var buf []byte
	buf, err := appendCanonicalJSON(buf, v)
	if err != nil {
		return "", err
	}
	return string(buf), nil
}

func appendCanonicalJSON(buf []byte, v any) ([]byte, error) {
	switch t := v.(type) {
	case nil:
		return append(buf, "null"...), nil
	case bool:
		if t {
			return append(buf, "true"...), nil
		}
		return append(buf, "false"...), nil
	case string:
		b, err := json.Marshal(t)
		if err != nil {
			return nil, err
		}
		return append(buf, b...), nil
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf = append(buf, '{')
		for i, k := range keys {
			if i > 0 {
				buf = append(buf, ',')
			}
			kb, err := json.Marshal(k)
			if err != nil {
				return nil, err
			}
			buf = append(buf, kb...)
			buf = append(buf, ':')
			buf, err = appendCanonicalJSON(buf, t[k])
			if err != nil {
				return nil, err
			}
		}
		return append(buf, '}'), nil
	case []any:
		buf = append(buf, '[')
		for i, x := range t {
			if i > 0 {
				buf = append(buf, ',')
			}
			var err error
			buf, err = appendCanonicalJSON(buf, x)
			if err != nil {
				return nil, err
			}
		}
		return append(buf, ']'), nil
	default:
		// numbers + everything else: defer to encoding/json.
		b, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		return append(buf, b...), nil
	}
}

// AgentRunQueue is the producer-side interface for the agent:runs Redis Stream.
type AgentRunQueue struct {
	client     RedisDoer
	streamName string
}

// NewAgentRunQueue constructs a queue. Returns an error if streamName is empty.
func NewAgentRunQueue(client RedisDoer, streamName string) (*AgentRunQueue, error) {
	if streamName == "" {
		return nil, errors.New("streamName must be non-empty")
	}
	return &AgentRunQueue{client: client, streamName: streamName}, nil
}

// StreamName returns the configured stream name.
func (q *AgentRunQueue) StreamName() string { return q.streamName }

// Enqueue XADDs the job onto the stream, returning the stream entry id.
func (q *AgentRunQueue) Enqueue(ctx context.Context, job AgentRunJob) (string, error) {
	fields := job.ToStreamFields()
	values := make(map[string]any, len(fields))
	for k, v := range fields {
		values[k] = v
	}
	id, err := q.client.XAdd(ctx, &redis.XAddArgs{
		Stream: q.streamName,
		Values: values,
	}).Result()
	if err != nil {
		return "", fmt.Errorf("AgentRunQueue.Enqueue: %w", err)
	}
	return id, nil
}

// Length returns XLEN for the stream.
func (q *AgentRunQueue) Length(ctx context.Context) (int64, error) {
	return q.client.XLen(ctx, q.streamName).Result()
}
