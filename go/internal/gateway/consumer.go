package gateway

// AgentRunConsumer reads jobs from the agent:runs Redis Stream (XREADGROUP),
// spawns runtime-daemon as a local subprocess for each job, and delivers the
// reply via the matching channel adapter's SendOutgoing.
//
// This is the consumer side of the Redis producer/consumer pair — the gateway
// produces jobs when a webhook arrives; this consumer drives the actual agent
// turn and sends the reply back to the user.
//
// For local/dev mode (no Daytona sandbox), runtime-daemon runs on the host
// machine using the operator's CLI credentials in ~/.claude, ~/.codex, etc.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	consumerGroupName = "orchestrator"
	consumerIDPrefix  = "gw-consumer"
	readBlockDuration = 2 * time.Second
	readBatchSize     = 10
)

// AgentRunConsumer is the stream consumer. Create via NewAgentRunConsumer and
// start with Run. It blocks until ctx is cancelled.
type AgentRunConsumer struct {
	rdb        *redis.Client
	streamName string
	daemonBin  string
	channels   *ChannelRegistry
	chanDB     ChannelsRepoWithListGetDelete
	agentsDB   AgentsRepo
}

// NewAgentRunConsumer wires up the consumer. daemonBin is the path to the
// runtime-daemon binary (absolute or on PATH).
func NewAgentRunConsumer(
	rdb *redis.Client,
	streamName string,
	daemonBin string,
	channels *ChannelRegistry,
	chanDB ChannelsRepoWithListGetDelete,
	agentsDB AgentsRepo,
) *AgentRunConsumer {
	return &AgentRunConsumer{
		rdb:        rdb,
		streamName: streamName,
		daemonBin:  daemonBin,
		channels:   channels,
		chanDB:     chanDB,
		agentsDB:   agentsDB,
	}
}

// Run blocks until ctx is cancelled, processing jobs from the stream.
func (c *AgentRunConsumer) Run(ctx context.Context) error {
	consumerID := fmt.Sprintf("%s-%d", consumerIDPrefix, os.Getpid())
	log.Printf("consumer starting: stream=%s group=%s consumer=%s daemon=%s",
		c.streamName, consumerGroupName, consumerID, c.daemonBin)

	// Create consumer group (MKSTREAM ensures the stream exists).
	if err := c.rdb.XGroupCreateMkStream(ctx, c.streamName, consumerGroupName, "0").Err(); err != nil {
		// BUSYGROUP means the group already exists — that's fine.
		if err.Error() != "BUSYGROUP Consumer Group name already exists" {
			return fmt.Errorf("create consumer group: %w", err)
		}
	}

	// Re-deliver any pending messages from a prior crashed consumer first.
	if err := c.processPending(ctx, consumerID); err != nil {
		log.Printf("consumer: warning processing pending: %v", err)
	}

	// Main loop: read new messages.
	for {
		select {
		case <-ctx.Done():
			log.Printf("consumer stopping")
			return nil
		default:
		}

		msgs, err := c.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group:    consumerGroupName,
			Consumer: consumerID,
			Streams:  []string{c.streamName, ">"},
			Count:    readBatchSize,
			Block:    readBlockDuration,
			NoAck:    false,
		}).Result()
		if err != nil {
			if err == redis.Nil || err.Error() == "redis: nil" {
				continue // timeout — no new messages
			}
			if ctx.Err() != nil {
				return nil
			}
			log.Printf("consumer: XReadGroup error: %v — retrying in 1s", err)
			time.Sleep(time.Second)
			continue
		}

		for _, stream := range msgs {
			for _, msg := range stream.Messages {
				c.handleMessage(ctx, consumerID, msg)
			}
		}
	}
}

// processPending re-delivers any messages that were delivered to crashed
// consumers but never acknowledged.
func (c *AgentRunConsumer) processPending(ctx context.Context, consumerID string) error {
	msgs, err := c.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    consumerGroupName,
		Consumer: consumerID,
		Streams:  []string{c.streamName, "0"},
		Count:    100,
		Block:    0,
		NoAck:    false,
	}).Result()
	if err != nil && err != redis.Nil {
		return err
	}
	for _, stream := range msgs {
		for _, msg := range stream.Messages {
			c.handleMessage(ctx, consumerID, msg)
		}
	}
	return nil
}

// handleMessage processes one stream message and ACKs it on success.
func (c *AgentRunConsumer) handleMessage(ctx context.Context, consumerID string, msg redis.XMessage) {
	fields := make(map[string]string, len(msg.Values))
	for k, v := range msg.Values {
		fields[k] = fmt.Sprintf("%v", v)
	}

	log.Printf("consumer: processing msg=%s run_id=%s user_id=%s",
		msg.ID, fields["run_id"], fields["user_id"])

	if err := c.processJob(ctx, fields); err != nil {
		log.Printf("consumer: job failed msg=%s: %v", msg.ID, err)
		// ACK anyway to avoid infinite redelivery of broken jobs.
	}

	if err := c.rdb.XAck(ctx, c.streamName, consumerGroupName, msg.ID).Err(); err != nil {
		log.Printf("consumer: XAck failed msg=%s: %v", msg.ID, err)
	}
}

// processJob dispatches one agent run: spawn daemon → JSON-RPC run → send reply.
func (c *AgentRunConsumer) processJob(ctx context.Context, fields map[string]string) error {
	channelID := fields["channel_id"]
	threadID := fields["thread_id"]

	// Look up channel_type so we can route the reply.
	chanRow, err := c.chanDB.Get(ctx, channelID)
	if err != nil {
		return fmt.Errorf("lookup channel %s: %w", channelID, err)
	}
	if chanRow == nil {
		return fmt.Errorf("channel %s not found", channelID)
	}
	channelType := chanRow.ChannelType

	// Dispatch to runtime-daemon.
	replyText, err := c.spawnDaemon(ctx, fields)
	if err != nil {
		return fmt.Errorf("spawn daemon: %w", err)
	}

	// Deliver reply via channel adapter.
	if err := c.sendReply(ctx, channelType, channelID, threadID, replyText); err != nil {
		return fmt.Errorf("send reply: %w", err)
	}
	return nil
}

// spawnDaemon starts runtime-daemon, sends one JSON-RPC run request, and
// returns the reply text.
func (c *AgentRunConsumer) spawnDaemon(ctx context.Context, fields map[string]string) (string, error) {
	runCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	// Extract plain text from the message JSON.
	userText := fields["message"]
	if userText != "" {
		var msg map[string]any
		if json.Unmarshal([]byte(userText), &msg) == nil {
			if t, ok := msg["text"].(string); ok {
				userText = t
			}
		}
	}

	daemonBin := c.daemonBin
	if daemonBin == "" {
		var err error
		daemonBin, err = exec.LookPath("runtime-daemon")
		if err != nil {
			return "", fmt.Errorf("runtime-daemon not on PATH and RUNTIME_DAEMON_BIN not set")
		}
	}

	// Look up the agent config and write a temp agent.yaml for the daemon.
	agentID := fields["agent_id"]
	agentYAMLPath, workdir, cleanup, err := c.prepareAgentWorkdir(ctx, agentID)
	if err != nil {
		return "", fmt.Errorf("prepare agent workdir: %w", err)
	}
	defer cleanup()

	cmd := exec.CommandContext(runCtx, daemonBin,
		"--config", agentYAMLPath,
		"--workspace", workdir,
	)
	cmd.Env = os.Environ()
	cmd.Stderr = os.Stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "", fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start daemon: %w", err)
	}
	defer func() {
		stdin.Close()
		cmd.Wait() //nolint:errcheck
	}()

	rpc := map[string]any{
		"jsonrpc": "2.0",
		"id":      fields["run_id"],
		"method":  "run",
		"params": map[string]any{
			"user_id":    fields["user_id"],
			"agent_id":   fields["agent_id"],
			"channel_id": fields["channel_id"],
			"thread_id":  fields["thread_id"],
			"run_id":     fields["run_id"],
			"message":    map[string]any{"text": userText, "images": []any{}},
		},
	}
	b, _ := json.Marshal(rpc)
	if _, err := fmt.Fprintf(stdin, "%s\n", b); err != nil {
		return "", fmt.Errorf("write rpc: %w", err)
	}
	stdin.Close()

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	if !scanner.Scan() {
		return "", fmt.Errorf("daemon closed stdout without response")
	}

	var resp struct {
		Result any `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		return "", fmt.Errorf("parse daemon response: %w", err)
	}
	if resp.Error != nil {
		return "", fmt.Errorf("daemon rpc error %d: %s", resp.Error.Code, resp.Error.Message)
	}
	return extractReplyText(resp.Result), nil
}

// prepareAgentWorkdir fetches the agent config, writes it to a temp dir, and
// returns (agentYAMLPath, workdir, cleanup). Caller must call cleanup() when done.
func (c *AgentRunConsumer) prepareAgentWorkdir(ctx context.Context, agentID string) (string, string, func(), error) {
	agent, err := c.agentsDB.Get(ctx, agentID)
	if err != nil {
		return "", "", func() {}, fmt.Errorf("get agent %s: %w", agentID, err)
	}
	if agent == nil {
		return "", "", func() {}, fmt.Errorf("agent %s not found", agentID)
	}

	workdir, err := os.MkdirTemp("", "gw-run-")
	if err != nil {
		return "", "", func() {}, fmt.Errorf("mktemp workdir: %w", err)
	}
	cleanup := func() { os.RemoveAll(workdir) }

	agentYAML := filepath.Join(workdir, "agent.yaml")
	if err := os.WriteFile(agentYAML, []byte(agent.ConfigYAML), 0o644); err != nil {
		cleanup()
		return "", "", func() {}, fmt.Errorf("write agent.yaml: %w", err)
	}
	return agentYAML, workdir, cleanup, nil
}

// sendReply routes the reply text to the correct channel adapter.
func (c *AgentRunConsumer) sendReply(ctx context.Context, channelType, channelID, threadID, text string) error {
	adapter := c.channels.Get(channelType)
	if adapter == nil {
		log.Printf("consumer: no adapter registered for channel_type=%s — dropping reply", channelType)
		return nil
	}
	return adapter.SendOutgoing(ctx, channelID, threadID, text, nil)
}

// extractReplyText unwraps nested result envelopes from the daemon response.
func extractReplyText(v any) string {
	if v == nil {
		return ""
	}
	if m, ok := v.(map[string]any); ok {
		if t, ok := m["text"].(string); ok {
			return t
		}
		if inner, ok := m["result"]; ok {
			return extractReplyText(inner)
		}
	}
	return fmt.Sprintf("%v", v)
}
