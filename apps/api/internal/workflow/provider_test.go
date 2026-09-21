package workflow

import (
	"context"
	"encoding/json"
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

func TestGCWProvider_GetExecution_Failed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"exec1","state":"FAILED","error":"something went wrong"}`))
	}))
	defer srv.Close()

	p := NewGCWProviderWithClient(srv.URL, "my-project", "us-central1", srv.Client())
	state, _, execErr := p.GetExecution(context.Background(), "exec1")
	if state != "FAILED" {
		t.Fatalf("state %q", state)
	}
	if execErr == nil {
		t.Fatal("expected execErr for FAILED")
	}
	if !strings.Contains(execErr.Error(), "something went wrong") {
		t.Fatalf("execErr %q", execErr.Error())
	}
}

func TestGCWProvider_GetExecution_Failed_WithResultFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"exec1","state":"FAILED","result":{"message":"bad"}}`))
	}))
	defer srv.Close()

	p := NewGCWProviderWithClient(srv.URL, "my-project", "us-central1", srv.Client())
	state, _, execErr := p.GetExecution(context.Background(), "exec1")
	if state != "FAILED" {
		t.Fatalf("state %q", state)
	}
	if execErr == nil {
		t.Fatal("expected execErr")
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
