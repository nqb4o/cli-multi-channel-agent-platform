package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// SandboxView is the subset of F01's Sandbox model that gateway callers need.
type SandboxView struct {
	ID     string `json:"id"`
	UserID string `json:"user_id"`
	State  string `json:"state"` // provisioning|running|hibernated|destroyed
}

// OrchestratorClient is the minimal client surface used by gateway admin routes.
type OrchestratorClient interface {
	ProvisionSandbox(ctx context.Context, userID string) (*SandboxView, error)
	Healthz(ctx context.Context) bool
}

// OrchestratorStatusError is returned by HttpOrchestratorClient.ProvisionSandbox
// for non-2xx responses. The admin route maps these to 502 Bad Gateway.
type OrchestratorStatusError struct {
	StatusCode int
	Body       string
}

func (e *OrchestratorStatusError) Error() string {
	return fmt.Sprintf("orchestrator HTTP %d: %s", e.StatusCode, e.Body)
}

// OrchestratorTransportError wraps a connection / DNS / timeout failure.
type OrchestratorTransportError struct{ Err error }

func (e *OrchestratorTransportError) Error() string { return e.Err.Error() }
func (e *OrchestratorTransportError) Unwrap() error { return e.Err }

// HttpOrchestratorClient is the concrete net/http-based client for F01.
type HttpOrchestratorClient struct {
	baseURL string
	client  *http.Client
}

// NewHttpOrchestratorClient builds a client targeting baseURL. timeout is the
// per-request timeout (use 0 to disable).
func NewHttpOrchestratorClient(baseURL string, timeout time.Duration) *HttpOrchestratorClient {
	return &HttpOrchestratorClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  &http.Client{Timeout: timeout},
	}
}

// ProvisionSandbox POSTs to /sandboxes with {"user_id": ...} and returns the
// decoded SandboxView on 2xx. Non-2xx returns OrchestratorStatusError;
// transport errors return OrchestratorTransportError.
func (c *HttpOrchestratorClient) ProvisionSandbox(ctx context.Context, userID string) (*SandboxView, error) {
	body, err := json.Marshal(map[string]string{"user_id": userID})
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/sandboxes", bytes.NewReader(body))
	if err != nil {
		return nil, &OrchestratorTransportError{Err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, &OrchestratorTransportError{Err: err}
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &OrchestratorStatusError{StatusCode: resp.StatusCode, Body: string(respBody)}
	}
	var view SandboxView
	if err := json.Unmarshal(respBody, &view); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if view.ID == "" || view.UserID == "" {
		return nil, errors.New("orchestrator response missing required fields")
	}
	return &view, nil
}

// Healthz returns true if the orchestrator's /healthz returns 2xx.
func (c *HttpOrchestratorClient) Healthz(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/healthz", nil)
	if err != nil {
		return false
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}
