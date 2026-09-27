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
// POST /v1/projects/{project}/locations/{location}/workflows/{id}/executions[?executionId={id}]
// Body: {"argument": "<json string>"}
//
// CREATE-OR-RETURN (the launch/idempotency contract, investigate/launch.go)
// When the argument map carries a non-empty "execution_name", that value is
// forwarded as Cloud Workflows' executionId — the request's caller-chosen
// execution identifier — so the call is create-or-return rather than
// create-always: a repeat with the same execution_name addresses the SAME
// execution. That is what makes the crash window between StartExecution and
// RecordLaunch recoverable by reconciliation instead of a duplicate start.
//
// Two mechanisms implement it, and both are needed against a provider that
// enforces the identifier:
//   - forward executionId on create, and
//   - map a create-409 (identifier already exists) onto returning the
//     existing execution instead of failing the launch.
//
// When the argument carries no execution_name the behaviour is unchanged:
// a plain create, and 409 remains an error. No identifier is invented.
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

	// requestedExecutionID extracts the caller-chosen execution id from
	// the launch argument. Returns "" when the argument is not a map, has
	// no execution_name, or carries a blank one — never invents a value.
	requested := requestedExecutionID(argument)
	if requested != "" {
		endpoint += "?executionId=" + url.QueryEscape(requested)
	}

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
	if requested != "" {
		payload["executionId"] = requested
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
	// create-409 with a requested identifier means the execution already
	// exists: that is the create-or-return outcome, not a failure. Return
	// the existing execution's resource name so the launch converges.
	if resp.StatusCode == http.StatusConflict && requested != "" {
		return g.ExecutionResourceName(workflowID, requested), nil
	}
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

// requestedExecutionID pulls the caller-chosen execution identifier out of
// a launch argument map. Returns "" (not an error) when the argument is
// absent, is not a map, or carries no usable execution_name: an argument
// without an identifier is a plain create, not a malformed one.
func requestedExecutionID(argument any) string {
	m, ok := argument.(map[string]string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(m["execution_name"])
}

// ExecutionResourceName returns the fully-qualified Cloud Workflows
// resource name for an execution: the "name" field of an Execution
// resource, of the form
//
//	projects/{project}/locations/{location}/workflows/{workflowID}/executions/{executionID}
//
// It is the form GetExecution accepts. A bare execution id is NOT a valid
// input to GetExecution: the REST path needs the full collection prefix.
func (g *GCWProvider) ExecutionResourceName(workflowID, executionID string) string {
	return fmt.Sprintf("projects/%s/locations/%s/workflows/%s/executions/%s",
		g.project, g.location, workflowID, executionID)
}

// GetExecution fetches execution state.
//
// GET /v1/{executionName}
//
// The third return value classifies the LOOKUP, never the execution's outcome:
//
//	resolved  -> nil error, whatever the state. ACTIVE, SUCCEEDED and FAILED
//	            are all executions that exist; the state is returned so the
//	            caller can act on it, and the execution's own result (or its
//	            error payload, when it reports no result) is returned with it.
//	absent    -> ErrExecutionNotFound, for a genuinely missing execution only.
//	failed    -> any other error, for a transport failure, a non-404 error
//	            status, or an undecodable body. These are never
//	            ErrExecutionNotFound: a lookup that did not complete cannot
//	            prove absence, and the launch reconciler must keep failing
//	            closed on them rather than starting a duplicate execution.
//
// A terminal FAILED execution is therefore a SUCCESSFUL lookup, not a failure
// of it (APA-42). Reporting it as an error made a resolved execution read as a
// lookup outage to callers that classify on this error — see the classification
// block below.
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

	// error payload: a terminal execution's own error. It is DIAGNOSTIC data
	// about an execution that was resolved, never a lookup outcome (APA-42).
	var execError json.RawMessage
	for _, k := range []string{"error", "Error"} {
		if v, ok := raw[k]; ok && len(v) > 0 && string(v) != "null" {
			execError = v
			break
		}
	}

	// CLASSIFICATION (APA-42). The HTTP lookup SUCCEEDED, so the execution
	// EXISTS whatever its outcome. The third return value reports whether the
	// LOOKUP worked, never how the execution ended:
	//
	//	resolved (ACTIVE | SUCCEEDED | FAILED)  ->  (state, result, nil)
	//	genuinely absent (404 / NOT_FOUND)     ->  ErrExecutionNotFound
	//	transport / 5xx / undecodable body     ->  a real error, and NEVER
	//	                                          ErrExecutionNotFound
	//
	// A FAILED execution is RESOLVED. Reporting it as an error is what made a
	// real GCW execution that terminated — including via the workflow's own
	// timeout → EXPIRE path — read as a lookup outage: the launch reconciler
	// (investigate/launch.go) adopts only when this error is nil and takes the
	// launch path only when it is ErrExecutionNotFound, so a crash-window
	// execution that had already terminated was never adopted, its durable
	// workflow_launches row was never repaired, and every redelivery retried.
	// Classification belongs here, at the boundary that owns it: no caller
	// should ever have to parse an error to learn whether an execution exists.
	//
	// The terminal state is reported through the FIRST return value, so it is
	// never an opaque success, and the execution's own outcome payload is not
	// lost on the way out: a result is preferred, and the error payload is
	// surfaced when the execution reports no result of its own.
	if len(execError) > 0 && (len(result) == 0 || string(result) == "null") {
		result = execError
	}

	return state, result, nil
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
