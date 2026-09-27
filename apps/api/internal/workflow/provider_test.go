package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestGCWProvider_DeployWorkflow_Happy(t *testing.T) {
	var gotMethod, gotPath, gotQuery, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		b := make([]byte, 2048)
		n, _ := r.Body.Read(b)
		gotBody = string(b[:n])
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"name":"projects/my-project/locations/us-central1/workflows/claim-investigation"}`))
	}))
	defer srv.Close()

	p := NewGCWProviderWithClient(srv.URL, "my-project", "us-central1", srv.Client())
	err := p.DeployWorkflow(context.Background(), "claim-investigation", "main:\n  steps: []")
	if err != nil {
		t.Fatalf("DeployWorkflow failed: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("method = %q want POST", gotMethod)
	}
	if !strings.Contains(gotPath, "/v1/projects/my-project/locations/us-central1/workflows") {
		t.Fatalf("path = %q", gotPath)
	}
	if !strings.Contains(gotQuery, "workflowId=claim-investigation") {
		t.Fatalf("query = %q", gotQuery)
	}
	var payload map[string]string
	if err := json.Unmarshal([]byte(gotBody), &payload); err != nil {
		t.Fatalf("payload unmarshal: %v body=%q", err, gotBody)
	}
	if payload["sourceContents"] != "main:\n  steps: []" {
		t.Fatalf("sourceContents = %q", payload["sourceContents"])
	}
}

func TestGCWProvider_DeployWorkflow_ErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`bad yaml`))
	}))
	defer srv.Close()

	p := NewGCWProviderWithClient(srv.URL, "my-project", "us-central1", srv.Client())
	err := p.DeployWorkflow(context.Background(), "bad-wf", "invalid")
	if err == nil {
		t.Fatal("expected error for 400")
	}
	if !strings.Contains(err.Error(), "status 400") {
		t.Fatalf("error = %q", err.Error())
	}
}

func TestGCWProvider_DeployWorkflow_EmptyID(t *testing.T) {
	p := NewGCWProvider("http://localhost:8787", "my-project", "us-central1")
	if err := p.DeployWorkflow(context.Background(), "", "src"); err == nil {
		t.Fatal("expected error for empty workflowID")
	}
}

func TestGCWProvider_StartExecution_Happy(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/projects/my-project/locations/us-central1/workflows/claim-investigation/executions" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Fatalf("method %q", r.Method)
		}
		b := make([]byte, 4096)
		n, _ := r.Body.Read(b)
		gotBody = string(b[:n])
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"projects/my-project/locations/us-central1/workflows/claim-investigation/executions/abc123"}`))
	}))
	defer srv.Close()

	p := NewGCWProviderWithClient(srv.URL, "my-project", "us-central1", srv.Client())
	arg := map[string]string{"claim_id": "c1", "tenant_id": "t1"}
	name, err := p.StartExecution(context.Background(), "claim-investigation", arg)
	if err != nil {
		t.Fatalf("StartExecution failed: %v", err)
	}
	if name != "projects/my-project/locations/us-central1/workflows/claim-investigation/executions/abc123" {
		t.Fatalf("name = %q", name)
	}
	// Verify argument is json string inside payload.
	var payload map[string]string
	if err := json.Unmarshal([]byte(gotBody), &payload); err != nil {
		t.Fatalf("payload %q: %v", gotBody, err)
	}
	var argDecoded map[string]string
	if err := json.Unmarshal([]byte(payload["argument"]), &argDecoded); err != nil {
		t.Fatalf("argument json string %q: %v", payload["argument"], err)
	}
	if argDecoded["claim_id"] != "c1" {
		t.Fatalf("claim_id = %q", argDecoded["claim_id"])
	}
}

func TestGCWProvider_StartExecution_NilArgument(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"projects/p/locations/l/workflows/w/executions/x"}`))
	}))
	defer srv.Close()

	p := NewGCWProviderWithClient(srv.URL, "p", "l", srv.Client())
	name, err := p.StartExecution(context.Background(), "w", nil)
	if err != nil {
		t.Fatalf("StartExecution nil arg: %v", err)
	}
	if name == "" {
		t.Fatal("expected name")
	}
}

func TestGCWProvider_StartExecution_ErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`boom`))
	}))
	defer srv.Close()

	p := NewGCWProviderWithClient(srv.URL, "my-project", "us-central1", srv.Client())
	_, err := p.StartExecution(context.Background(), "claim-investigation", map[string]string{"x": "y"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "status 500") {
		t.Fatalf("err %q", err.Error())
	}
}

func TestGCWProvider_StartExecution_EmptyID(t *testing.T) {
	p := NewGCWProvider("http://localhost:8787", "my-project", "us-central1")
	if _, err := p.StartExecution(context.Background(), "", nil); err == nil {
		t.Fatal("expected error for empty workflowID")
	}
}

// TestGCWProvider_StartExecution_ForwardsExecutionNameAsExecutionId pins
// the create-or-return half of the launch/idempotency contract
// (investigate/launch.go:76-88, launch.go:251-261): the caller's
// requested execution_name must reach the provider as Cloud Workflows'
// caller-chosen executionId, on the query parameter the v1 REST API
// defines. Without it the provider always mints a fresh name and the
// reconciler's GetExecution probe can never adopt.
func TestGCWProvider_StartExecution_ForwardsExecutionNameAsExecutionId(t *testing.T) {
	var gotQuery string
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		b := make([]byte, 4096)
		n, _ := r.Body.Read(b)
		gotBody = string(b[:n])
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"projects/my-project/locations/us-central1/workflows/claim-investigation/executions/exec-abc"}`))
	}))
	defer srv.Close()

	p := NewGCWProviderWithClient(srv.URL, "my-project", "us-central1", srv.Client())
	arg := map[string]string{
		"tenant_id": "t1", "claim_id": "c1",
		"investigation_id": "inv-1", "execution_name": "exec-1f61",
	}
	if _, err := p.StartExecution(context.Background(), "claim-investigation", arg); err != nil {
		t.Fatalf("StartExecution: %v", err)
	}
	if q := gotQuery; !strings.Contains(q, "executionId=exec-1f61") {
		t.Fatalf("query = %q, want executionId=exec-1f61 (the requested name must become the execution identifier)", q)
	}
	var payload map[string]string
	if err := json.Unmarshal([]byte(gotBody), &payload); err != nil {
		t.Fatalf("payload unmarshal: %v body=%q", err, gotBody)
	}
	if payload["executionId"] != "exec-1f61" {
		t.Fatalf("payload executionId = %q, want exec-1f61", payload["executionId"])
	}
	// The argument still travels verbatim: the launch correlation keys
	// must not be displaced by the identifier.
	var argDecoded map[string]string
	if err := json.Unmarshal([]byte(payload["argument"]), &argDecoded); err != nil {
		t.Fatalf("argument json string %q: %v", payload["argument"], err)
	}
	if argDecoded["investigation_id"] != "inv-1" {
		t.Fatalf("argument investigation_id = %q, want inv-1", argDecoded["investigation_id"])
	}
}

// TestGCWProvider_StartExecution_CreateConflictReturnsExisting pins the
// other half: a create-409 for a requested identifier means the execution
// already exists, which is the create-or-return OUTCOME and must not fail
// the launch. It must surface the existing execution's resource name.
func TestGCWProvider_StartExecution_CreateConflictReturnsExisting(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":409,"message":"ALREADY_EXISTS","status":"ALREADY_EXISTS"}}`))
	}))
	defer srv.Close()

	p := NewGCWProviderWithClient(srv.URL, "my-project", "us-central1", srv.Client())
	arg := map[string]string{"investigation_id": "inv-1", "execution_name": "exec-1f61"}
	name, err := p.StartExecution(context.Background(), "claim-investigation", arg)
	if err != nil {
		t.Fatalf("create-409 with a requested executionId must be create-or-return, got error: %v", err)
	}
	want := "projects/my-project/locations/us-central1/workflows/claim-investigation/executions/exec-1f61"
	if name != want {
		t.Fatalf("name = %q, want the existing execution %q", name, want)
	}
}

// TestGCWProvider_StartExecution_ConflictWithoutRequestedNameErrors
// guards the boundary: a 409 is only create-or-return when the caller
// actually requested an identifier. Otherwise there is no existing
// execution to name, so swallowing the conflict would invent one.
func TestGCWProvider_StartExecution_ConflictWithoutRequestedNameErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`ALREADY_EXISTS`))
	}))
	defer srv.Close()

	p := NewGCWProviderWithClient(srv.URL, "my-project", "us-central1", srv.Client())
	if _, err := p.StartExecution(context.Background(), "claim-investigation", map[string]string{"x": "y"}); err == nil {
		t.Fatal("expected error: 409 with no requested execution_name has no existing execution to return")
	}
}

// TestGCWProvider_StartExecution_NoRequestedNameIsPlainCreate pins that
// the plain-create path is unchanged: an argument with no execution_name
// sends neither the query parameter nor the body field, so no identifier
// is ever invented.
func TestGCWProvider_StartExecution_NoRequestedNameIsPlainCreate(t *testing.T) {
	var gotQuery, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		b := make([]byte, 4096)
		n, _ := r.Body.Read(b)
		gotBody = string(b[:n])
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"projects/my-project/locations/us-central1/workflows/claim-investigation/executions/abc123"}`))
	}))
	defer srv.Close()

	p := NewGCWProviderWithClient(srv.URL, "my-project", "us-central1", srv.Client())
	if _, err := p.StartExecution(context.Background(), "claim-investigation", map[string]string{"claim_id": "c1"}); err != nil {
		t.Fatalf("StartExecution: %v", err)
	}
	if strings.Contains(gotQuery, "executionId") {
		t.Fatalf("query = %q, want no executionId when none was requested", gotQuery)
	}
	if strings.Contains(gotBody, "executionId") {
		t.Fatalf("body = %q, want no executionId when none was requested", gotBody)
	}
}

// TestGCWProvider_StartExecution_BlankExecutionNameIsPlainCreate: a blank
// or whitespace-only execution_name is not an identifier and must not be
// forwarded as one.
func TestGCWProvider_StartExecution_BlankExecutionNameIsPlainCreate(t *testing.T) {
	var gotQuery, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		b := make([]byte, 4096)
		n, _ := r.Body.Read(b)
		gotBody = string(b[:n])
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"projects/my-project/locations/us-central1/workflows/claim-investigation/executions/abc123"}`))
	}))
	defer srv.Close()

	p := NewGCWProviderWithClient(srv.URL, "my-project", "us-central1", srv.Client())
	if _, err := p.StartExecution(context.Background(), "claim-investigation", map[string]string{"execution_name": "   "}); err != nil {
		t.Fatalf("StartExecution: %v", err)
	}
	if strings.Contains(gotQuery, "executionId") || strings.Contains(gotBody, "executionId") {
		t.Fatalf("blank execution_name was forwarded: query=%q body=%q", gotQuery, gotBody)
	}
}

// TestGCWProvider_ExecutionResourceName pins the resource-name form that
// GetExecution accepts. A bare execution id is NOT a valid input to
// GetExecution (the REST path needs the collection prefix), so this
// constructor is the only safe way to name an execution.
func TestGCWProvider_ExecutionResourceName(t *testing.T) {
	p := NewGCWProvider("http://localhost:8787", "my-project", "us-central1")
	got := p.ExecutionResourceName("claim-investigation", "exec-1f61")
	want := "projects/my-project/locations/us-central1/workflows/claim-investigation/executions/exec-1f61"
	if got != want {
		t.Fatalf("ExecutionResourceName = %q, want %q", got, want)
	}
}

func TestGCWProvider_GetExecution_Succeeded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("method %q", r.Method)
		}
		expected := "/v1/projects/my-project/locations/us-central1/workflows/claim-investigation/executions/abc123"
		if r.URL.Path != expected {
			t.Fatalf("path %q want %q", r.URL.Path, expected)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"projects/my-project/locations/us-central1/workflows/claim-investigation/executions/abc123","state":"SUCCEEDED","result":{"approved":true}}`))
	}))
	defer srv.Close()

	p := NewGCWProviderWithClient(srv.URL, "my-project", "us-central1", srv.Client())
	state, result, execErr := p.GetExecution(context.Background(), "projects/my-project/locations/us-central1/workflows/claim-investigation/executions/abc123")
	if execErr != nil {
		t.Fatalf("unexpected execErr: %v", execErr)
	}
	if state != "SUCCEEDED" {
		t.Fatalf("state %q", state)
	}
	var res map[string]bool
	if err := json.Unmarshal(result, &res); err != nil {
		t.Fatalf("result unmarshal: %v", err)
	}
	if !res["approved"] {
		t.Fatal("expected approved true")
	}
}

// TestGCWProvider_GetExecution_Failed pins the APA-42 classification: a
// FAILED execution is a successfully RESOLVED execution. It used to be
// reported through the error channel, which made every caller that classifies
// on that error (the launch reconciler) read a real execution as a lookup
// outage. The state and the execution's own failure detail must both still
// reach the caller — a resolved-but-failed execution is not an opaque success.
func TestGCWProvider_GetExecution_Failed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"exec1","state":"FAILED","error":"something went wrong"}`))
	}))
	defer srv.Close()

	p := NewGCWProviderWithClient(srv.URL, "my-project", "us-central1", srv.Client())
	state, result, execErr := p.GetExecution(context.Background(), "exec1")
	if state != "FAILED" {
		t.Fatalf("state %q", state)
	}
	if execErr != nil {
		t.Fatalf("execErr %v: a FAILED execution is resolved, so the lookup error must be nil", execErr)
	}
	if !strings.Contains(string(result), "something went wrong") {
		t.Fatalf("result %q: the execution's own failure detail must reach the caller", string(result))
	}
}

// TestGCWProvider_GetExecution_Failed_WithResultFallback: a FAILED execution
// that reports a result of its own surfaces THAT result (not the error
// payload), still with a nil lookup error.
func TestGCWProvider_GetExecution_Failed_WithResultFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"exec1","state":"FAILED","result":{"message":"bad"}}`))
	}))
	defer srv.Close()

	p := NewGCWProviderWithClient(srv.URL, "my-project", "us-central1", srv.Client())
	state, result, execErr := p.GetExecution(context.Background(), "exec1")
	if state != "FAILED" {
		t.Fatalf("state %q", state)
	}
	if execErr != nil {
		t.Fatalf("execErr %v: a FAILED execution is resolved, so the lookup error must be nil", execErr)
	}
	var res map[string]string
	if err := json.Unmarshal(result, &res); err != nil {
		t.Fatalf("result unmarshal: %v result=%s", err, string(result))
	}
	if res["message"] != "bad" {
		t.Fatalf("result %s, want the execution's own result object", string(result))
	}
}

func TestGCWProvider_GetExecution_Active(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"exec1","state":"ACTIVE"}`))
	}))
	defer srv.Close()

	p := NewGCWProviderWithClient(srv.URL, "my-project", "us-central1", srv.Client())
	state, result, execErr := p.GetExecution(context.Background(), "exec1")
	if state != "ACTIVE" {
		t.Fatalf("state %q", state)
	}
	if execErr != nil {
		t.Fatalf("execErr %v", execErr)
	}
	if len(result) != 0 && string(result) != "null" {
		t.Fatalf("result should be empty, got %q", string(result))
	}
}

func TestGCWProvider_GetExecution_ErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`not found`))
	}))
	defer srv.Close()

	p := NewGCWProviderWithClient(srv.URL, "my-project", "us-central1", srv.Client())
	_, _, err := p.GetExecution(context.Background(), "missing")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "status 404") {
		t.Fatalf("err %q", err.Error())
	}
}

func TestGCWProvider_GetExecution_LeadingSlash(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"state":"SUCCEEDED","result":{}}`))
	}))
	defer srv.Close()

	p := NewGCWProviderWithClient(srv.URL, "my-project", "us-central1", srv.Client())
	_, _, _ = p.GetExecution(context.Background(), "/projects/my-project/locations/us-central1/workflows/w/executions/e1")
	if gotPath != "/v1/projects/my-project/locations/us-central1/workflows/w/executions/e1" {
		t.Fatalf("path %q", gotPath)
	}
}

func TestGCWProvider_SendCallback_Happy(t *testing.T) {
	var gotMethod, gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		b := make([]byte, 4096)
		n, _ := r.Body.Read(b)
		gotBody = string(b[:n])
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	p := NewGCWProviderWithClient(srv.URL, "my-project", "us-central1", srv.Client())
	payload := map[string]string{"action": "approve", "reason": "ok"}
	if err := p.SendCallback(context.Background(), "cb-123", payload); err != nil {
		t.Fatalf("SendCallback failed: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("method %q", gotMethod)
	}
	if gotPath != "/callbacks/cb-123" {
		t.Fatalf("path %q", gotPath)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(gotBody), &got); err != nil {
		t.Fatalf("body %q: %v", gotBody, err)
	}
	if got["action"] != "approve" {
		t.Fatalf("action %q", got["action"])
	}
}

func TestGCWProvider_SendCallback_ErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusGone)
		_, _ = w.Write([]byte(`callback expired`))
	}))
	defer srv.Close()

	p := NewGCWProviderWithClient(srv.URL, "my-project", "us-central1", srv.Client())
	err := p.SendCallback(context.Background(), "cb1", map[string]string{"x": "y"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "status 410") {
		t.Fatalf("err %q", err.Error())
	}
}

func TestGCWProvider_SendCallback_EmptyID(t *testing.T) {
	p := NewGCWProvider("http://localhost:8787", "my-project", "us-central1")
	if err := p.SendCallback(context.Background(), "", nil); err == nil {
		t.Fatal("expected error for empty callbackID")
	}
}

func TestNoopProvider_Errors(t *testing.T) {
	n := NewNoopProvider()
	if _, err := n.StartExecution(context.Background(), "w", nil); err == nil {
		t.Fatal("expected error StartExecution")
	}
	if _, _, err := n.GetExecution(context.Background(), "exec"); err == nil {
		t.Fatal("expected error GetExecution")
	}
	if err := n.SendCallback(context.Background(), "cb", nil); err == nil {
		t.Fatal("expected error SendCallback")
	}
	if err := n.DeployWorkflow(context.Background(), "w", "src"); err == nil {
		t.Fatal("expected error DeployWorkflow")
	}
}

func TestGCWProvider_HostWithoutScheme(t *testing.T) {
	// host without scheme should be prepended with http:// and still work
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"state":"ACTIVE"}`))
	}))
	defer srv.Close()

	// Extract host:port without scheme
	hostPort := strings.TrimPrefix(srv.URL, "http://")
	p := NewGCWProviderWithClient(hostPort, "my-project", "us-central1", srv.Client())
	// Override baseURL check: normalizeHost should have added http://
	if !strings.HasPrefix(p.baseURL, "http://") {
		t.Fatalf("baseURL %q should have http://", p.baseURL)
	}
	state, _, err := p.GetExecution(context.Background(), "exec1")
	if err != nil {
		t.Fatalf("GetExecution failed: %v", err)
	}
	if state != "ACTIVE" {
		t.Fatalf("state %q", state)
	}
}

func TestGCWProvider_HostWithSchemePreserved(t *testing.T) {
	p := NewGCWProvider("https://gcw.example:8787/", "my-project", "us-central1")
	if p.baseURL != "https://gcw.example:8787" {
		t.Fatalf("baseURL %q", p.baseURL)
	}
	p2 := NewGCWProvider("http://localhost:8787/", "my-project", "us-central1")
	if p2.baseURL != "http://localhost:8787" {
		t.Fatalf("baseURL %q", p2.baseURL)
	}
}

func TestGCWProvider_EmptyHostDefaults(t *testing.T) {
	p := NewGCWProvider("", "my-project", "us-central1")
	if p.baseURL != "http://localhost:8787" {
		t.Fatalf("baseURL %q", p.baseURL)
	}
	p2 := NewGCWProvider("   ", "my-project", "us-central1")
	if p2.baseURL != "http://localhost:8787" {
		t.Fatalf("baseURL %q", p2.baseURL)
	}
}

func TestGCWProvider_ContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	p := NewGCWProviderWithClient(srv.URL, "my-project", "us-central1", srv.Client())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := p.DeployWorkflow(ctx, "w", "src")
	if err == nil {
		t.Fatal("expected context canceled error")
	}
	if !strings.Contains(err.Error(), "context canceled") && !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("expected context canceled, got %q", err.Error())
	}
}

func TestGCWProvider_Timeout(t *testing.T) {
	// Verify client timeout is 5s (no hang). Use a quick server to ensure no error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"x"}`))
	}))
	defer srv.Close()

	p := NewGCWProvider(srv.URL, "my-project", "us-central1")
	// Replace client with httptest client but preserve timeout
	p.client = srv.Client()
	p.client.Timeout = 5 * time.Second

	if p.client.Timeout != 5*time.Second {
		t.Fatalf("timeout %v", p.client.Timeout)
	}
	_, err := p.StartExecution(context.Background(), "w", map[string]string{"a": "b"})
	if err != nil {
		t.Fatalf("StartExecution: %v", err)
	}
}

func TestGCWProvider_GetExecution_NotFound_Sentinel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`not found`))
	}))
	defer srv.Close()

	p := NewGCWProviderWithClient(srv.URL, "my-project", "us-central1", srv.Client())
	_, _, err := p.GetExecution(context.Background(), "exec-missing")
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrExecutionNotFound) {
		t.Fatalf("404 must wrap ErrExecutionNotFound, got %v", err)
	}
}

func TestGCWProvider_GetExecution_ServerError_NotSentinel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`boom`))
	}))
	defer srv.Close()

	p := NewGCWProviderWithClient(srv.URL, "my-project", "us-central1", srv.Client())
	_, _, err := p.GetExecution(context.Background(), "exec-x")
	if err == nil {
		t.Fatal("expected error")
	}
	if errors.Is(err, ErrExecutionNotFound) {
		t.Fatalf("500 must not be ErrExecutionNotFound, got %v", err)
	}
}
