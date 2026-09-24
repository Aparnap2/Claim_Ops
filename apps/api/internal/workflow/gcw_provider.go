package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// GCWProvider implements WorkflowProvider via REST to the GCW emulator.
type GCWProvider struct {
	baseURL  string
	project  string
	location string
	client   *http.Client
}

var _ WorkflowProvider = (*GCWProvider)(nil)

// normalizeHost ensures the host has a scheme and no trailing slash.
// If raw is empty, it defaults to http://localhost:8787.
// If raw contains host:port without scheme, http:// is prepended.
func normalizeHost(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "http://localhost:8787"
	}
	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		return strings.TrimSuffix(raw, "/")
	}
	return "http://" + strings.TrimSuffix(raw, "/")
}

// NewGCWProvider creates a GCWProvider.
// emulatorHost may be "" (defaults to http://localhost:8787),
// "localhost:8787", or "http://localhost:8787".
// project and location default to "my-project" and "us-central1" when empty.
func NewGCWProvider(emulatorHost, project, location string) *GCWProvider {
	if project == "" {
		project = "my-project"
	}
	if location == "" {
		location = "us-central1"
	}
	return &GCWProvider{
		baseURL:  normalizeHost(emulatorHost),
		project:  project,
		location: location,
		client: &http.Client{
			Timeout: 5 * time.Second,
		},
	}
}

// NewGCWProviderWithClient is test-only: allows injecting a custom http.Client (e.g., httptest).
func NewGCWProviderWithClient(emulatorHost, project, location string, client *http.Client) *GCWProvider {
	p := NewGCWProvider(emulatorHost, project, location)
	if client != nil {
		p.client = client
	}
	return p
}

// DeployWorkflow deploys workflow source to the emulator.
//
// POST /v1/projects/{project}/locations/{location}/workflows?workflowId={id}
// Body: {"sourceContents": "..."}
func (g *GCWProvider) DeployWorkflow(ctx context.Context, workflowID string, sourceContents string) error {
	if workflowID == "" {
		return fmt.Errorf("workflowID must not be empty")
	}
	endpoint := fmt.Sprintf("%s/v1/projects/%s/locations/%s/workflows?workflowId=%s",
		g.baseURL,
		url.PathEscape(g.project),
		url.PathEscape(g.location),
		url.QueryEscape(workflowID),
	)
	payload := map[string]string{
		"sourceContents": sourceContents,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal deploy payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create deploy request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.client.Do(req)
	if err != nil {
		return fmt.Errorf("deploy workflow %q: %w", workflowID, err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("deploy workflow %q failed: status %d: %s", workflowID, resp.StatusCode, string(respBody))
	}
	return nil
}

// StartExecution starts a workflow execution.
//
// POST /v1/projects/{project}/locations/{location}/workflows/{id}/executions
// Body: {"argument": "<json string>"}
func (g *GCWProvider) StartExecution(ctx context.Context, workflowID string, argument any) (string, error) {
	if workflowID == "" {
		return "", fmt.Errorf("workflowID must not be empty")
	}
	endpoint := fmt.Sprintf("%s/v1/projects/%s/locations/%s/workflows/%s/executions",
		g.baseURL,
		url.PathEscape(g.project),
		url.PathEscape(g.location),
		url.PathEscape(workflowID),
	)

	var argStr string
	if argument != nil {
		b, err := json.Marshal(argument)
		if err != nil {
			return "", fmt.Errorf("marshal argument: %w", err)
		}
		argStr = string(b)
	}

	payload := map[string]string{
		"argument": argStr,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal execution payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("create execution request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("start execution %q: %w", workflowID, err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("start execution %q failed: status %d: %s", workflowID, resp.StatusCode, string(respBody))
	}

	var out struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", fmt.Errorf("decode execution response: %w", err)
	}
	if out.Name == "" {
		// Some emulator versions return the name at top-level or nested; try raw map fallback.
		var m map[string]json.RawMessage
		if err2 := json.Unmarshal(respBody, &m); err2 == nil {
			if v, ok := m["name"]; ok {
				var n string
				if json.Unmarshal(v, &n) == nil && n != "" {
					return n, nil
				}
			}
		}
		return "", fmt.Errorf("execution response missing name: %s", string(respBody))
	}
	return out.Name, nil
}

// GetExecution fetches execution state.
//
// GET /v1/{executionName}
func (g *GCWProvider) GetExecution(ctx context.Context, executionName string) (string, json.RawMessage, error) {
	if executionName == "" {
		return "", nil, fmt.Errorf("executionName must not be empty")
	}
	trimmed := strings.TrimPrefix(executionName, "/")
	endpoint := fmt.Sprintf("%s/v1/%s", g.baseURL, trimmed)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", nil, fmt.Errorf("create get execution request: %w", err)
	}

	resp, err := g.client.Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("get execution %q: %w", executionName, err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusNotFound {
		return "", nil, fmt.Errorf("get execution %q failed: status %d: %s: %w", executionName, resp.StatusCode, string(respBody), ErrExecutionNotFound)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", nil, fmt.Errorf("get execution %q failed: status %d: %s", executionName, resp.StatusCode, string(respBody))
	}

	// Flexible decoding: handle both lowercase and uppercase keys.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(respBody, &raw); err != nil {
		return "", nil, fmt.Errorf("decode execution body: %w", err)
	}

	// state
	var state string
	for _, k := range []string{"state", "State"} {
		if v, ok := raw[k]; ok {
			_ = json.Unmarshal(v, &state)
			if state != "" {
				break
			}
			// If state is not a JSON string (unlikely), use raw as string.
			state = strings.Trim(string(v), "\"")
			if state != "" {
				break
			}
		}
	}

	// result
	var result json.RawMessage
	for _, k := range []string{"result", "Result"} {
		if v, ok := raw[k]; ok {
			result = v
			break
		}
	}

	// error payload (execution error when FAILED)
	var execErr error
	for _, k := range []string{"error", "Error"} {
		if v, ok := raw[k]; ok && len(v) > 0 && string(v) != "null" {
			// If error is a string, use it directly.
			var s string
			if json.Unmarshal(v, &s) == nil && s != "" {
				execErr = fmt.Errorf("execution failed: %s", s)
			} else {
				execErr = fmt.Errorf("execution failed: %s", string(v))
			}
			break
		}
	}

	// If state is FAILED and no explicit error field, synthesize error from result or generic.
	if state == "FAILED" && execErr == nil {
		if len(result) > 0 && string(result) != "null" {
			execErr = fmt.Errorf("execution failed: %s", string(result))
		} else {
			execErr = fmt.Errorf("execution failed with state FAILED")
		}
	}

	return state, result, execErr
}

// SendCallback delivers a callback payload.
//
// POST /callbacks/{callbackId}
// Body: payload marshaled as JSON (any)
func (g *GCWProvider) SendCallback(ctx context.Context, callbackID string, payload any) error {
	if callbackID == "" {
		return fmt.Errorf("callbackID must not be empty")
	}
	endpoint := fmt.Sprintf("%s/callbacks/%s", g.baseURL, url.PathEscape(callbackID))

	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("marshal callback payload: %w", err)
		}
		body = bytes.NewReader(b)
	} else {
		body = bytes.NewReader([]byte("{}"))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return fmt.Errorf("create callback request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.client.Do(req)
	if err != nil {
		return fmt.Errorf("send callback %q: %w", callbackID, err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("send callback %q failed: status %d: %s", callbackID, resp.StatusCode, string(respBody))
	}
	return nil
}
