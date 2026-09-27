package workflow

// APA-42 at the provider boundary: GetExecution classifies the LOOKUP, not the
// execution's OUTCOME.
//
// THE DEFECT (R5, measured against the live emulator and reproduced here)
// -----------------------------------------------------------------------
// GetExecution resolves an execution and then, when the execution's state is
// FAILED, synthesises "execution failed ..." into its third return value. That
// value is not ErrExecutionNotFound, so investigate/launch.go:261-276 — which
// adopts only when the error is nil and starts only when the error IS
// ErrExecutionNotFound — reads a resolved-but-FAILED execution as a LOOKUP
// OUTAGE. A crash-window execution that had already terminated (including via
// the workflow's own timeout → EXPIRE path) is therefore never adopted, its
// durable workflow_launches row is never repaired, and every redelivery
// retries. This is a property of the provider/reconciler pair, not an emulator
// artifact: real GCW behaves identically.
//
// THE CONTRACT (what these tests pin)
// -----------------------------------
//	third return value == nil  ⇔  the LOOKUP succeeded
//	ErrExecutionNotFound       ⇔  the execution is genuinely absent
//	any other error            ⇔  the lookup itself failed (transport/5xx)
//
// A terminal state — FAILED included — is a successfully RESOLVED execution, and
// the state is reported through the first return value so a caller can act on
// it. Classification belongs HERE, in the provider: a caller must never have to
// parse an error to learn whether an execution exists.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	apa42Project  = "claimops-live"
	apa42Location = "us-central1"
	apa42Workflow = "claim-investigation"
	apa42FullName = "projects/" + apa42Project + "/locations/" + apa42Location +
		"/workflows/" + apa42Workflow + "/executions/exec-c3c21e923d3b7851a6aa98cb76ca7377"
)

// apa42Provider serves one canned response to every GetExecution probe.
func apa42Provider(t *testing.T, status int, body string) *GCWProvider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return NewGCWProviderWithClient(srv.URL, apa42Project, apa42Location, srv.Client())
}

// AREA 1: ACTIVE is a resolved execution.
func TestGetExecution_Active_IsResolvedSuccess(t *testing.T) {
	p := apa42Provider(t, http.StatusOK,
		`{"name":"`+apa42FullName+`","state":"ACTIVE"}`)

	state, _, err := p.GetExecution(context.Background(), apa42FullName)
	if err != nil {
		t.Fatalf("ACTIVE: GetExecution err = %v, want nil (lookup succeeded)", err)
	}
	if state != "ACTIVE" {
		t.Fatalf("state = %q, want ACTIVE", state)
	}
}

// AREA 2: SUCCEEDED is a resolved execution, and its result is the payload.
func TestGetExecution_Succeeded_IsResolvedSuccess(t *testing.T) {
	p := apa42Provider(t, http.StatusOK,
		`{"name":"`+apa42FullName+`","state":"SUCCEEDED","result":"{\"ok\":true}"}`)

	state, result, err := p.GetExecution(context.Background(), apa42FullName)
	if err != nil {
		t.Fatalf("SUCCEEDED: GetExecution err = %v, want nil (lookup succeeded)", err)
	}
	if state != "SUCCEEDED" {
		t.Fatalf("state = %q, want SUCCEEDED", state)
	}
	// result is the raw JSON of the execution's own result field: a JSON
	// string here, decoded back to the document the execution returned.
	var got string
	if err := json.Unmarshal(result, &got); err != nil {
		t.Fatalf("result %s is not the raw JSON of the result field: %v", string(result), err)
	}
	if want := `{"ok":true}`; got != want {
		t.Fatalf("result = %s, want %s", got, want)
	}
}

// AREA 3 (THE DEFECT): FAILED is a successfully RESOLVED execution. The error
// must be nil and the FAILED state must reach the caller — not an opaque
// success, and never an error the reconciler reads as a lookup outage.
func TestGetExecution_Failed_IsResolvedSuccess_AndStateReported(t *testing.T) {
	p := apa42Provider(t, http.StatusOK,
		`{"name":"`+apa42FullName+`","state":"FAILED",`+
			`"result":"{\"message\":\"environment variable AGENT_URL not found\"}",`+
			`"error":{"payload":"env missing","context":"step=investigate"}}`)

	state, result, err := p.GetExecution(context.Background(), apa42FullName)
	if err != nil {
		t.Fatalf("FAILED execution: GetExecution err = %v, want nil — the lookup "+
			"succeeded; a terminal outcome is not a lookup failure (APA-42)", err)
	}
	if state != "FAILED" {
		t.Fatalf("state = %q, want FAILED reported to the caller", state)
	}
	if errors.Is(err, ErrExecutionNotFound) {
		t.Fatal("a resolved FAILED execution must never be typed absence")
	}
	// The failure stays observable: the caller can see both the terminal
	// state AND the payload that explains it.
	if !strings.Contains(string(result), "AGENT_URL") {
		t.Fatalf("result = %s, want the execution's own failure payload so a "+
			"resolved-but-failed execution is never an opaque success", string(result))
	}
}

// AREA 3b: a FAILED execution with NO payload at all is still a resolved
// execution reported as FAILED. The provider must not invent an error to
// describe an outcome.
func TestGetExecution_Failed_NoPayload_IsResolvedSuccess(t *testing.T) {
	p := apa42Provider(t, http.StatusOK,
		`{"name":"`+apa42FullName+`","state":"FAILED"}`)

	state, _, err := p.GetExecution(context.Background(), apa42FullName)
	if err != nil {
		t.Fatalf("FAILED with no payload: err = %v, want nil (lookup succeeded)", err)
	}
	if state != "FAILED" {
		t.Fatalf("state = %q, want FAILED", state)
	}
}

// AREA 3c: when a FAILED execution reports no result of its own, the error
// payload it DOES report must still reach the caller. Otherwise fixing the
// classification would silently drop the only diagnostic the provider had.
func TestGetExecution_Failed_ErrorPayloadSurfacesWhenNoResult(t *testing.T) {
	p := apa42Provider(t, http.StatusOK,
		`{"name":"`+apa42FullName+`","state":"FAILED",`+
			`"error":{"payload":"execution timed out","context":"step=wait"}}`)

	state, result, err := p.GetExecution(context.Background(), apa42FullName)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if state != "FAILED" {
		t.Fatalf("state = %q, want FAILED", state)
	}
	if !strings.Contains(string(result), "execution timed out") {
		t.Fatalf("result = %s, want the reported error payload surfaced to the caller", string(result))
	}
}

// AREA 3d: classification keys on the LOOKUP, never on the presence of an
// error payload. An execution that still carries one is resolved, not absent
// and not an outage.
func TestGetExecution_ActiveWithErrorPayload_IsStillResolvedSuccess(t *testing.T) {
	p := apa42Provider(t, http.StatusOK,
		`{"name":"`+apa42FullName+`","state":"ACTIVE","error":{"payload":"stale"}}`)

	state, _, err := p.GetExecution(context.Background(), apa42FullName)
	if err != nil {
		t.Fatalf("err = %v, want nil: a resolved execution is a resolved execution "+
			"whatever payload it carries", err)
	}
	if state != "ACTIVE" {
		t.Fatalf("state = %q, want ACTIVE", state)
	}
}

// AREA 4: a true lookup failure (404) stays the typed absence the reconciler
// treats as "safe to start" — unchanged.
func TestGetExecution_NotFound_IsTypedAbsence(t *testing.T) {
	p := apa42Provider(t, http.StatusNotFound, `not found`)

	state, _, err := p.GetExecution(context.Background(), apa42FullName)
	if !errors.Is(err, ErrExecutionNotFound) {
		t.Fatalf("404 err = %v, want it to wrap ErrExecutionNotFound", err)
	}
	if state != "" {
		t.Fatalf("state = %q, want empty for an absent execution", state)
	}
}

// AREA 4b: the structured NOT_FOUND body a real GCW returns for a missing
// resource is the same typed absence.
func TestGetExecution_StructuredNotFound_IsTypedAbsence(t *testing.T) {
	p := apa42Provider(t, http.StatusNotFound,
		`{"error":{"code":404,"message":"Execution not found.","status":"NOT_FOUND"}}`)

	_, _, err := p.GetExecution(context.Background(), apa42FullName)
	if !errors.Is(err, ErrExecutionNotFound) {
		t.Fatalf("structured NOT_FOUND err = %v, want it to wrap ErrExecutionNotFound", err)
	}
}

// AREA 5: a 5xx is a genuine lookup failure and must NOT be typed absence —
// the reconciler has to keep failing closed (no start) so an outage can never
// read as "safe to start" and duplicate an execution.
func TestGetExecution_ServerError_IsNotAbsence(t *testing.T) {
	p := apa42Provider(t, http.StatusServiceUnavailable, `upstream unavailable`)

	state, _, err := p.GetExecution(context.Background(), apa42FullName)
	if err == nil {
		t.Fatal("5xx: want an error")
	}
	if errors.Is(err, ErrExecutionNotFound) {
		t.Fatalf("5xx must never be ErrExecutionNotFound, got %v", err)
	}
	if state != "" {
		t.Fatalf("state = %q, want empty when the lookup failed", state)
	}
}

// AREA 5b: a transport failure (nothing listening) is likewise NOT absence.
func TestGetExecution_TransportFailure_IsNotAbsence(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
	client := srv.Client()
	p := NewGCWProviderWithClient(srv.URL, apa42Project, apa42Location, client)
	srv.Close() // nothing is listening any more: the probe cannot reach GCW at all

	_, _, err := p.GetExecution(context.Background(), apa42FullName)
	if err == nil {
		t.Fatal("transport failure: want an error")
	}
	if errors.Is(err, ErrExecutionNotFound) {
		t.Fatalf("transport failure must never be ErrExecutionNotFound, got %v", err)
	}
}

// A malformed body is a provider-contract failure, not an absence: treating it
// as "safe to start" would duplicate a possibly-live execution.
func TestGetExecution_MalformedBody_IsNotAbsence(t *testing.T) {
	p := apa42Provider(t, http.StatusOK, `{"state":`)

	_, _, err := p.GetExecution(context.Background(), apa42FullName)
	if err == nil {
		t.Fatal("malformed body: want an error")
	}
	if errors.Is(err, ErrExecutionNotFound) {
		t.Fatalf("malformed body must never be ErrExecutionNotFound, got %v", err)
	}
}

// Input validation is unchanged and is never classified as a resolved lookup.
func TestGetExecution_EmptyName_IsNotAbsence(t *testing.T) {
	p := apa42Provider(t, http.StatusOK, `{"state":"ACTIVE"}`)

	_, _, err := p.GetExecution(context.Background(), "")
	if err == nil {
		t.Fatal("empty execution name: want an error")
	}
	if errors.Is(err, ErrExecutionNotFound) {
		t.Fatalf("empty name must never be ErrExecutionNotFound, got %v", err)
	}
}

// Flexible decoding is preserved: an uppercase State key is still a resolved
// execution, and its classification is the same.
func TestGetExecution_UppercaseStateKey_IsResolvedSuccess(t *testing.T) {
	p := apa42Provider(t, http.StatusOK, `{"name":"`+apa42FullName+`","State":"SUCCEEDED"}`)

	state, _, err := p.GetExecution(context.Background(), apa42FullName)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if state != "SUCCEEDED" {
		t.Fatalf("state = %q, want SUCCEEDED", state)
	}
}

// The result channel stays JSON bytes for a resolved execution, so callers can
// decode it exactly as before.
func TestGetExecution_ResultStaysRawJSON(t *testing.T) {
	p := apa42Provider(t, http.StatusOK,
		`{"name":"`+apa42FullName+`","state":"FAILED","result":{"detail":"boom"}}`)

	_, result, err := p.GetExecution(context.Background(), apa42FullName)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	var decoded map[string]string
	if err := json.Unmarshal(result, &decoded); err != nil {
		t.Fatalf("result is not valid JSON (%v): %s", err, string(result))
	}
	if decoded["detail"] != "boom" {
		t.Fatalf("result = %s, want the execution's own result object", string(result))
	}
}
