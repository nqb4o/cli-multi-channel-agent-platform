// Thin wrapper around the Daytona public REST API.
//
// The orchestrator is the **only** place that talks to Daytona; every
// other service in the platform talks to F01 via its own Orchestrator
// interface (see sandbox.go). Keeping the SDK contained here means we can
// swap providers later without rewiring callers.
//
// Two implementations are provided:
//
//   - LiveDaytonaClient — wraps the documented Daytona REST surface
//     (sandbox CRUD, volumes, exec). Used in production when
//     DAYTONA_API_KEY is set.
//   - FakeDaytonaClient — in-memory, deterministic. Used by unit tests +
//     local dev without DAYTONA_API_KEY. See FakeDaytonaClient in
//     daytona_client_fake.go.
package orchestrator

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// RawSandbox is a provider-agnostic sandbox descriptor that crosses the
// daytona_client boundary. Daytona's own state enum has more values than
// we expose; we collapse them onto our four-state model below.
type RawSandbox struct {
	ID     string
	State  string // provisioning|running|hibernated|destroyed
	Labels map[string]string
}

// RawExecResult is the one-shot exec output from the sandbox toolbox API.
type RawExecResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode *int // nil → killed by timeout
	TimedOut bool
}

// RawVolumeMount is a volume mount spec — passed to CreateSandbox.
type RawVolumeMount struct {
	VolumeID  string
	MountPath string
}

// CreateSandboxParams gathers the inputs to DaytonaClient.CreateSandbox.
type CreateSandboxParams struct {
	Name              string
	Image             string
	Env               map[string]string
	Labels            map[string]string
	Volumes           []RawVolumeMount
	AutoStopIntervalM int
}

// ExecParams gathers the optional inputs to DaytonaClient.ExecCommand.
type ExecParams struct {
	Env       map[string]string
	Cwd       string
	TimeoutS  int
	Stdin     []byte
}

// DaytonaClient is the protocol the orchestrator depends on. Both the live
// REST adapter and the in-memory fake satisfy it.
type DaytonaClient interface {
	CreateSandbox(ctx context.Context, p CreateSandboxParams) (*RawSandbox, error)
	GetSandbox(ctx context.Context, sandboxID string) (*RawSandbox, error)
	FindByLabel(ctx context.Context, key, value string) (*RawSandbox, error)
	StartSandbox(ctx context.Context, sandboxID string) (*RawSandbox, error)
	StopSandbox(ctx context.Context, sandboxID string) (*RawSandbox, error)
	DeleteSandbox(ctx context.Context, sandboxID string) error
	GetOrCreateVolume(ctx context.Context, name string) (string, error)
	ExecCommand(ctx context.Context, sandboxID string, command []string, p ExecParams) (*RawExecResult, error)
	Healthz(ctx context.Context) bool
}

// HealthPinger is implemented by clients that can probe a sandbox's
// runtime daemon directly. The fake client implements this; the live REST
// client delegates to a runtime daemon HTTP probe inside the sandbox (see
// PingHealth below for the expected shape).
type HealthPinger interface {
	PingHealth(ctx context.Context, sandboxID string, timeout time.Duration) bool
}

// daytonaToOurState collapses Daytona state strings onto our four-state
// model. Source: daytona_api_client.models.SandboxState.
var daytonaToOurState = map[string]string{
	"creating":      "provisioning",
	"pending_build": "provisioning",
	"starting":      "provisioning",
	"started":       "running",
	"stopping":      "running",
	"stopped":       "hibernated",
	"archiving":     "hibernated",
	"archived":      "hibernated",
	"restoring":     "provisioning",
	"destroyed":     "destroyed",
	"destroying":    "destroyed",
	"error":         "destroyed",
	"build_failed":  "destroyed",
	"unknown":       "destroyed",
}

// NormalizeState coerces a daytona state value (enum or string) to our
// four-state set.
func NormalizeState(raw string) string {
	if raw == "" {
		return "destroyed"
	}
	s := strings.ToLower(raw)
	// Daytona enum values are sometimes 'SandboxState.STARTED' when stringified.
	if i := strings.LastIndex(s, "."); i >= 0 {
		s = s[i+1:]
	}
	if mapped, ok := daytonaToOurState[s]; ok {
		return mapped
	}
	if s == "started" {
		return "running"
	}
	return "destroyed"
}

// -----------------------------------------------------------------------
// LiveDaytonaClient — REST-backed implementation.
// -----------------------------------------------------------------------

const defaultDaytonaAPIURL = "https://app.daytona.io/api"

// defaultToolboxProxyURL is the base URL for per-sandbox toolbox operations
// (exec, file access). The pattern is: <toolboxProxyURL>/<sandboxID>/<op>.
const defaultToolboxProxyURL = "https://proxy.app.daytona.io/toolbox"

// LiveDaytonaClient is a thin REST client targeting the Daytona public
// OpenAPI spec.
//
// We deliberately use a small surface (sandbox CRUD + volumes + exec) to
// keep the dependency footprint near zero. The shape of each REST payload
// matches what the Python SDK sends.
type LiveDaytonaClient struct {
	apiKey      string
	apiURL      string
	toolboxURL  string
	target      string
	http        *http.Client
}

// NewLiveDaytonaClient constructs a live client. apiKey must be non-empty.
func NewLiveDaytonaClient(apiKey, apiURL, target string) *LiveDaytonaClient {
	if apiURL == "" {
		apiURL = defaultDaytonaAPIURL
	}
	return &LiveDaytonaClient{
		apiKey:     apiKey,
		apiURL:     strings.TrimRight(apiURL, "/"),
		toolboxURL: defaultToolboxProxyURL,
		target:     target,
		http:       &http.Client{Timeout: 60 * time.Second},
	}
}

func (c *LiveDaytonaClient) doJSONAt(ctx context.Context, baseURL, method, path string, body any, out any) error {
	var buf io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal: %w", err)
		}
		buf = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, baseURL+path, buf)
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.target != "" {
		req.Header.Set("X-Daytona-Target", c.target)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("daytona %s %s: %d %s", method, path, resp.StatusCode, string(body))
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("decode: %w", err)
	}
	return nil
}

func (c *LiveDaytonaClient) doJSON(ctx context.Context, method, path string, body any, out any) error {
	return c.doJSONAt(ctx, c.apiURL, method, path, body, out)
}

// liveSandboxJSON is the wire shape we expect for a sandbox object.
type liveSandboxJSON struct {
	ID     string            `json:"id"`
	State  string            `json:"state"`
	Labels map[string]string `json:"labels,omitempty"`
}

func (j liveSandboxJSON) toRaw() *RawSandbox {
	labels := j.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	return &RawSandbox{
		ID:     j.ID,
		State:  NormalizeState(j.State),
		Labels: labels,
	}
}

// CreateSandbox implements DaytonaClient.
func (c *LiveDaytonaClient) CreateSandbox(ctx context.Context, p CreateSandboxParams) (*RawSandbox, error) {
	body := map[string]any{
		"name":   p.Name,
		"image":  p.Image,
		"env":    p.Env,
		"labels": p.Labels,
	}
	if p.AutoStopIntervalM > 0 {
		body["autoStopInterval"] = p.AutoStopIntervalM
	}
	if len(p.Volumes) > 0 {
		vs := make([]map[string]string, 0, len(p.Volumes))
		for _, v := range p.Volumes {
			vs = append(vs, map[string]string{
				"volumeId":  v.VolumeID,
				"mountPath": v.MountPath,
			})
		}
		body["volumes"] = vs
	}
	var out liveSandboxJSON
	if err := c.doJSON(ctx, http.MethodPost, "/sandbox", body, &out); err != nil {
		return nil, err
	}
	return out.toRaw(), nil
}

// GetSandbox implements DaytonaClient.
func (c *LiveDaytonaClient) GetSandbox(ctx context.Context, sandboxID string) (*RawSandbox, error) {
	var out liveSandboxJSON
	if err := c.doJSON(ctx, http.MethodGet, "/sandbox/"+sandboxID, nil, &out); err != nil {
		return nil, err
	}
	return out.toRaw(), nil
}

// FindByLabel implements DaytonaClient.
func (c *LiveDaytonaClient) FindByLabel(ctx context.Context, key, value string) (*RawSandbox, error) {
	// Daytona's list endpoint accepts ?label=key=value query params.
	q := "/sandbox?label=" + key + "%3D" + value
	var items []liveSandboxJSON
	if err := c.doJSON(ctx, http.MethodGet, q, nil, &items); err != nil {
		return nil, err
	}
	for _, item := range items {
		if item.Labels[key] == value {
			return item.toRaw(), nil
		}
	}
	return nil, nil
}

// StartSandbox implements DaytonaClient.
func (c *LiveDaytonaClient) StartSandbox(ctx context.Context, sandboxID string) (*RawSandbox, error) {
	if err := c.doJSON(ctx, http.MethodPost, "/sandbox/"+sandboxID+"/start", nil, nil); err != nil {
		return nil, err
	}
	return c.GetSandbox(ctx, sandboxID)
}

// StopSandbox implements DaytonaClient.
func (c *LiveDaytonaClient) StopSandbox(ctx context.Context, sandboxID string) (*RawSandbox, error) {
	if err := c.doJSON(ctx, http.MethodPost, "/sandbox/"+sandboxID+"/stop", nil, nil); err != nil {
		return nil, err
	}
	return c.GetSandbox(ctx, sandboxID)
}

// DeleteSandbox implements DaytonaClient.
func (c *LiveDaytonaClient) DeleteSandbox(ctx context.Context, sandboxID string) error {
	return c.doJSON(ctx, http.MethodDelete, "/sandbox/"+sandboxID, nil, nil)
}

// GetOrCreateVolume implements DaytonaClient.
// The Daytona API does not support upsert — it returns 400 if a volume with
// the same name already exists. We list all volumes and match by name first,
// falling back to create only when it is truly absent.
func (c *LiveDaytonaClient) GetOrCreateVolume(ctx context.Context, name string) (string, error) {
	// 1. Try to find an existing volume with this name.
	var volumes []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/volumes", nil, &volumes); err != nil {
		return "", fmt.Errorf("list volumes: %w", err)
	}
	for _, v := range volumes {
		if v.Name == name {
			return v.ID, nil
		}
	}
	// 2. Not found — create it.
	var out struct {
		ID string `json:"id"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/volumes", map[string]any{"name": name}, &out); err != nil {
		return "", err
	}
	return out.ID, nil
}

// ExecCommand implements DaytonaClient.
func (c *LiveDaytonaClient) ExecCommand(ctx context.Context, sandboxID string, command []string, p ExecParams) (*RawExecResult, error) {
	cmdStr := shellJoin(command)
	if p.Stdin != nil {
		b64 := base64.StdEncoding.EncodeToString(p.Stdin)
		cmdStr = "echo '" + b64 + "' | base64 -d | " + cmdStr
	}
	timeout := p.TimeoutS
	if timeout <= 0 {
		timeout = 60
	}
	body := map[string]any{
		"command": cmdStr,
		"timeout": timeout,
	}
	if p.Cwd != "" {
		body["cwd"] = p.Cwd
	}
	if p.Env != nil {
		body["env"] = p.Env
	}
	var out struct {
		Result   string `json:"result"`
		ExitCode *int   `json:"exitCode"`
	}
	reqCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout+5)*time.Second)
	defer cancel()
	// Exec goes via the toolbox proxy: POST <toolboxURL>/<sandboxID>/process/execute
	err := c.doJSONAt(reqCtx, c.toolboxURL, http.MethodPost, "/"+sandboxID+"/process/execute", body, &out)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return &RawExecResult{TimedOut: true}, nil
		}
		return nil, err
	}
	return &RawExecResult{
		Stdout:   []byte(out.Result),
		ExitCode: out.ExitCode,
	}, nil
}

// Healthz implements DaytonaClient. Probes the control plane.
func (c *LiveDaytonaClient) Healthz(ctx context.Context) bool {
	hctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var items []liveSandboxJSON
	err := c.doJSON(hctx, http.MethodGet, "/sandbox?limit=1", nil, &items)
	return err == nil
}

// shellJoin renders an argv list as a single shell command, quoting each
// argument with single quotes (escaping embedded single quotes). Mirrors
// Python's shlex.join.
func shellJoin(argv []string) string {
	out := make([]string, 0, len(argv))
	for _, a := range argv {
		out = append(out, shellQuote(a))
	}
	return strings.Join(out, " ")
}

// shellQuote returns the argument quoted for safe shell use. Mirrors
// shlex.quote (Python).
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '/' || r == '.' || r == '=' || r == '@' || r == '+' || r == ':' || r == ',') {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

// Compile-time assertion: LiveDaytonaClient satisfies DaytonaClient.
var _ DaytonaClient = (*LiveDaytonaClient)(nil)

// keep sync import alive (used by other files in the package).
var _ = sync.Mutex{}
