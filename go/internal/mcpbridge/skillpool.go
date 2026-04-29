package mcpbridge

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// skillChild — one stdio MCP subprocess

type skillChild struct {
	slug     string
	manifest SkillManifestForBridge

	mu           sync.Mutex
	process      *exec.Cmd
	stdin        io.WriteCloser
	stdout       *bufio.Reader
	tools        []map[string]any
	initialized  bool
	nextReqID    int
}

func newSkillChild(slug string, manifest SkillManifestForBridge) *skillChild {
	return &skillChild{
		slug:      slug,
		manifest:  manifest,
		nextReqID: 1,
	}
}

// ensureStarted spawns the child (if not yet running) and runs initialize +
// tools/list. Thread-safe via mu; idempotent once initialized.
func (c *skillChild) ensureStarted(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.initialized {
		return nil
	}
	if err := c.spawn(); err != nil {
		return err
	}
	if err := c.rpcInitialize(ctx); err != nil {
		return err
	}
	tools, err := c.rpcToolsList(ctx)
	if err != nil {
		return err
	}
	c.tools = tools
	c.initialized = true
	return nil
}

func (c *skillChild) spawn() error {
	argv := c.manifest.MCPCommand
	if len(argv) == 0 {
		return fmt.Errorf("skill %q declares mcp.enabled but no mcp.command", c.slug)
	}
	workDir := c.manifest.WorkDir
	if workDir == "" || !dirExists(workDir) {
		wd, err := os.Getwd()
		if err == nil {
			workDir = wd
		}
	}

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = workDir

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("skill child %q stdin pipe: %w", c.slug, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("skill child %q stdout pipe: %w", c.slug, err)
	}
	// Discard stderr (the child may write diagnostics; we don't want them
	// mixed into the RPC stream).
	cmd.Stderr = io.Discard

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("skill child %q failed to start: %w", c.slug, err)
	}

	c.process = cmd
	c.stdin = stdin
	c.stdout = bufio.NewReader(stdout)
	return nil
}

func (c *skillChild) rpcInitialize(ctx context.Context) error {
	params := map[string]any{
		"protocolVersion": MCPProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "openclaw-runtime", "version": "0.1.0"},
	}
	_, err := c.sendRPC(ctx, "initialize", params)
	return err
}

func (c *skillChild) rpcToolsList(ctx context.Context) ([]map[string]any, error) {
	result, err := c.sendRPC(ctx, "tools/list", map[string]any{})
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, nil
	}
	m, ok := result.(map[string]any)
	if !ok {
		return nil, nil
	}
	raw, ok := m["tools"].([]any)
	if !ok {
		return nil, nil
	}
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if t, ok := item.(map[string]any); ok {
			out = append(out, t)
		}
	}
	return out, nil
}

// callTool calls the given tool name (local, un-prefixed) with arguments.
func (c *skillChild) callTool(ctx context.Context, tool string, arguments map[string]any) (any, error) {
	if err := c.ensureStarted(ctx); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sendRPC(ctx, "tools/call", map[string]any{"name": tool, "arguments": arguments})
}

func (c *skillChild) sendRPC(ctx context.Context, method string, params map[string]any) (any, error) {
	if c.stdin == nil || c.stdout == nil {
		return nil, fmt.Errorf("skill child %q not running", c.slug)
	}
	reqID := c.nextReqID
	c.nextReqID++

	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      reqID,
		"method":  method,
		"params":  params,
	}
	line, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("skill child %q marshal: %w", c.slug, err)
	}
	line = append(line, '\n')

	if _, err := c.stdin.Write(line); err != nil {
		return nil, fmt.Errorf("skill child %q stdin write: %w", c.slug, err)
	}

	// Read the response line with a timeout.
	type readResult struct {
		line []byte
		err  error
	}
	ch := make(chan readResult, 1)
	go func() {
		l, err := c.stdout.ReadBytes('\n')
		ch <- readResult{l, err}
	}()

	timeout := time.Duration(childRPCTimeoutS * float64(time.Second))
	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("skill child %q %s: context done: %w", c.slug, method, ctx.Err())
	case <-time.After(timeout):
		return nil, fmt.Errorf("skill child %q did not respond to %q within %.0fs", c.slug, method, childRPCTimeoutS)
	case res := <-ch:
		if res.err != nil && res.err != io.EOF {
			return nil, fmt.Errorf("skill child %q stdout read: %w", c.slug, res.err)
		}
		if len(res.line) == 0 {
			return nil, fmt.Errorf("skill child %q closed stdout", c.slug)
		}
		var payload map[string]any
		dec := json.NewDecoder(strings.NewReader(strings.TrimSpace(string(res.line))))
		dec.UseNumber()
		if err := dec.Decode(&payload); err != nil {
			return nil, fmt.Errorf("skill child %q returned invalid JSON: %w", c.slug, err)
		}
		// Verify ID.
		if rawID, ok := payload["id"]; ok {
			if n, ok := rawID.(json.Number); ok {
				if i, err := n.Int64(); err == nil && int(i) != reqID {
					return nil, fmt.Errorf("skill child %q response id mismatch (expected %d, got %d)", c.slug, reqID, i)
				}
			}
		}
		if errObj, ok := payload["error"]; ok && errObj != nil {
			if m, ok := errObj.(map[string]any); ok {
				msg, _ := m["message"].(string)
				if msg == "" {
					msg = "child error"
				}
				return nil, fmt.Errorf("skill child %q error: %s", c.slug, msg)
			}
			return nil, fmt.Errorf("skill child %q error: %v", c.slug, errObj)
		}
		return payload["result"], nil
	}
}

// stop sends SIGTERM, waits up to 1s, then SIGKILL.
func (c *skillChild) stop() {
	c.mu.Lock()
	proc := c.process
	c.mu.Unlock()
	if proc == nil || proc.Process == nil {
		return
	}
	// Close stdin to signal graceful shutdown.
	if c.stdin != nil {
		_ = c.stdin.Close()
	}
	done := make(chan struct{})
	go func() {
		_ = proc.Wait()
		close(done)
	}()
	// First try SIGTERM (best-effort; Windows may not support it).
	_ = proc.Process.Signal(os.Interrupt)
	select {
	case <-done:
		return
	case <-time.After(time.Duration(terminateGraceS * float64(time.Second))):
	}
	// Escalate to SIGKILL.
	_ = proc.Process.Kill()
	select {
	case <-done:
	case <-time.After(time.Duration(terminateGraceS * float64(time.Second))):
	}
}

// ---------------------------------------------------------------------------
// skillChildPool — lazy pool of stdio MCP children, one per skill slug.

type skillChildPool struct {
	mu        sync.Mutex
	manifests map[string]SkillManifestForBridge
	children  map[string]*skillChild
}

func newSkillChildPool(manifests map[string]SkillManifestForBridge) *skillChildPool {
	return &skillChildPool{
		manifests: manifests,
		children:  make(map[string]*skillChild),
	}
}

// slugs returns all slugs whose skill declares an MCP server.
func (p *skillChildPool) slugs() []string {
	var out []string
	for slug, m := range p.manifests {
		if m.MCPEnabled && len(m.MCPCommand) > 0 {
			out = append(out, slug)
		}
	}
	return out
}

// get returns (or creates) the skillChild for the given slug.
func (p *skillChildPool) get(slug string) (*skillChild, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if child, ok := p.children[slug]; ok {
		return child, nil
	}
	manifest, ok := p.manifests[slug]
	if !ok {
		return nil, fmt.Errorf("unknown skill %q", slug)
	}
	if !manifest.MCPEnabled || len(manifest.MCPCommand) == 0 {
		return nil, fmt.Errorf("skill %q does not declare an MCP server", slug)
	}
	child := newSkillChild(slug, manifest)
	p.children[slug] = child
	return child, nil
}

// listAllTools starts every MCP-enabled child and returns all prefixed tools.
func (p *skillChildPool) listAllTools(ctx context.Context) ([]map[string]any, error) {
	slugs := p.slugs()
	var out []map[string]any
	for _, slug := range slugs {
		child, err := p.get(slug)
		if err != nil {
			return nil, err
		}
		if err := child.ensureStarted(ctx); err != nil {
			return nil, fmt.Errorf("skill %q: %w", slug, err)
		}
		child.mu.Lock()
		tools := child.tools
		child.mu.Unlock()
		for _, t := range tools {
			localName, _ := t["name"].(string)
			if localName == "" {
				continue
			}
			prefixed := make(map[string]any, len(t))
			for k, v := range t {
				prefixed[k] = v
			}
			prefixed["name"] = slug + "." + localName
			out = append(out, prefixed)
		}
	}
	if out == nil {
		out = []map[string]any{}
	}
	return out, nil
}

// callTool dispatches a tool call to the correct child.
func (p *skillChildPool) callTool(ctx context.Context, slug, tool string, arguments map[string]any) (any, error) {
	child, err := p.get(slug)
	if err != nil {
		return nil, err
	}
	return child.callTool(ctx, tool, arguments)
}

// stopAll reaps all children concurrently.
func (p *skillChildPool) stopAll() {
	p.mu.Lock()
	children := make([]*skillChild, 0, len(p.children))
	for _, c := range p.children {
		children = append(children, c)
	}
	p.children = make(map[string]*skillChild)
	p.mu.Unlock()

	var wg sync.WaitGroup
	for _, c := range children {
		wg.Add(1)
		go func(child *skillChild) {
			defer wg.Done()
			child.stop()
		}(c)
	}
	wg.Wait()
}

// ---------------------------------------------------------------------------
// helpers

func hashArgs(arguments any) string {
	b, err := json.Marshal(arguments)
	if err != nil {
		b = []byte(fmt.Sprintf("%v", arguments))
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16]
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
