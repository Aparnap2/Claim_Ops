package workflow

// APA-41 at the provider boundary: execution ID != execution resource name.
//
// PR #105 proved each half in isolation (execution_name -> executionId on
// create; ExecutionResourceName builds the full name). What was NOT proven is
// that the two halves compose without collapsing into each other. These tests
// drive the REAL GCWProvider over httptest and pin both directions of the
// invariant:
//
//	argument execution_name (BARE)  -> forwarded verbatim as executionId
//	ExecutionResourceName (FULL)    -> the only input GetExecution resolves
//
// The inverse mistake — passing the FULL resource name as the argument's
// execution_name, so it becomes a nested, slash-bearing executionId — would
// break duplicate-start protection (PR #105) while looking correct. These
// tests are the guard against it.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// apa41BareID is a realistic deterministic bare execution id.
const apa41BareID = "exec-c3c21e923d3b7851a6aa98cb76ca7377"

const (
	apa41Project  = "claimops-live"
	apa41Location = "us-central1"
	apa41Workflow = "claim-investigation"
)

// TestAPA41_StartExecution_ForwardsBareID_Verbatim drives StartExecution
// with a BARE execution_name and asserts the provider forwards it VERBATIM as
// executionId in BOTH the query parameter and the body — and that no
// collection path leaked into the identifier.
func TestAPA41_StartExecution_ForwardsBareID_Verbatim(t *testing.T) {
	var gotQuery, gotBody, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		b := make([]byte, 8192)
		n, _ := r.Body.Read(b)
		gotBody = string(b[:n])
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"projects/` + apa41Project + `/locations/` + apa41Location +
			`/workflows/` + apa41Workflow + `/executions/` + apa41BareID + `"}`))
	}))
	defer srv.Close()

	p := NewGCWProviderWithClient(srv.URL, apa41Project, apa41Location, srv.Client())
	arg := map[string]string{
		"tenant_id": "t1", "claim_id": "c1",
		"investigation_id": "inv-1", "execution_name": apa41BareID,
	}
	if _, err := p.StartExecution(context.Background(), apa41Workflow, arg); err != nil {
		t.Fatalf("StartExecution: %v", err)
	}

	// Query parameter carries the bare id verbatim.
	if got := gotQuery; got != "executionId="+apa41BareID {
		t.Fatalf("query = %q, want exactly executionId=%s", got, apa41BareID)
	}

	// Body carries the same bare id.
	var payload map[string]string
	if err := json.Unmarshal([]byte(gotBody), &payload); err != nil {
		t.Fatalf("payload unmarshal: %v body=%q", err, gotBody)
	}
	if payload["executionId"] != apa41BareID {
		t.Fatalf("payload executionId = %q, want %q", payload["executionId"], apa41BareID)
	}

	// The argument itself still travels verbatim, and its execution_name is
	// still BARE — the reconciler's id is not rewritten into a name.
	var argDecoded map[string]string
	if err := json.Unmarshal([]byte(payload["argument"]), &argDecoded); err != nil {
		t.Fatalf("argument json string %q: %v", payload["argument"], err)
	}
	if argDecoded["execution_name"] != apa41BareID {
		t.Fatalf("argument execution_name = %q, want the bare id %q verbatim", argDecoded["execution_name"], apa41BareID)
	}
	if strings.Contains(argDecoded["execution_name"], "projects/") {
		t.Fatalf("argument execution_name %q contains a collection path; it must stay a bare id or duplicate-start protection breaks",
			argDecoded["execution_name"])
	}

	// The CREATE path itself is the bare-id collection; the id is not
	// substituted into the path.
	if want := "/v1/projects/" + apa41Project + "/locations/" + apa41Location + "/workflows/" + apa41Workflow + "/executions"; gotPath != want {
		t.Fatalf("create path = %q, want %q", gotPath, want)
	}
}

// TestAPA41_ExecutionResourceName_ResolvesViaGetExecution pins area 2
// against the wire: the value ExecutionResourceName returns is EXACTLY what
// GetExecution accepts, and it round-trips to a real execution. This is what
// makes the reconciler's probe addressable.
func TestAPA41_ExecutionResourceName_ResolvesViaGetExecution(t *testing.T) {
	full := "projects/" + apa41Project + "/locations/" + apa41Location +
		"/workflows/" + apa41Workflow + "/executions/" + apa41BareID

	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		// Only the full resource name resolves; anything else 404s, like
		// the emulator.
		if r.URL.Path != "/v1/"+full {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":5,"message":"not found"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"` + full + `","state":"ACTIVE"}`))
	}))
	defer srv.Close()

	p := NewGCWProviderWithClient(srv.URL, apa41Project, apa41Location, srv.Client())

	// The provider's own naming authority produces the probe value.
	probe := p.ExecutionResourceName(apa41Workflow, apa41BareID)
	if probe != full {
		t.Fatalf("ExecutionResourceName = %q, want %q", probe, full)
	}

	// And that value resolves.
	state, _, err := p.GetExecution(context.Background(), probe)
	if err != nil {
		t.Fatalf("GetExecution(%q): %v", probe, err)
	}
	if state != "ACTIVE" {
		t.Fatalf("state = %q, want ACTIVE", state)
	}
	if want := "/v1/" + full; gotPath != want {
		t.Fatalf("GET path = %q, want %q", gotPath, want)
	}
}

// TestAPA41_GetExecution_BareID_IsTypedAbsence pins the ROOT CAUSE at the
// provider boundary: a bare execution id is not addressable. GetExecution
// must report it as the typed absence the reconciler treats as "safe to
// start" — which is precisely why probing a bare id silently duplicates an
// execution that does exist.
func TestAPA41_GetExecution_BareID_IsTypedAbsence(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A real GCW emulator 404s a bare-id path; nothing resolves.
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":5,"message":"not found"}}`))
	}))
	defer srv.Close()

	p := NewGCWProviderWithClient(srv.URL, apa41Project, apa41Location, srv.Client())

	_, _, err := p.GetExecution(context.Background(), apa41BareID)
	if err == nil {
		t.Fatal("GetExecution(bare id): want typed absence, got success")
	}
	if !errors.Is(err, ErrExecutionNotFound) {
		t.Fatalf("GetExecution(bare id) error = %v, want it to wrap ErrExecutionNotFound", err)
	}
}

// TestNoopProvider_ExecutionResourceName pins the additive NoopProvider
// implementation: it is addressable (satisfies the interface) and inert —
// its GetExecution remains typed-absent, so a noop provider can never be
// mistaken for an adopted execution.
func TestNoopProvider_ExecutionResourceName(t *testing.T) {
	n := NewNoopProvider()
	got := n.ExecutionResourceName(apa41Workflow, apa41BareID)
	if !strings.HasPrefix(got, "projects/") || !strings.HasSuffix(got, "/executions/"+apa41BareID) {
		t.Fatalf("ExecutionResourceName = %q, want a projects/... resource name ending in the bare id", got)
	}
	if _, _, err := n.GetExecution(context.Background(), got); !errors.Is(err, ErrExecutionNotFound) {
		t.Fatalf("NoopProvider.GetExecution error = %v, want typed absence", err)
	}
}

// TestGCWProvider_ExecutionResourceName_RoundTripsThroughConflict pins that
// the create-or-return path (409) hands back the SAME full name the
// reconciler probes, so an adopted identity and a started identity are the
// same string.
func TestGCWProvider_ExecutionResourceName_RoundTripsThroughConflict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":6,"message":"already exists"}}`))
	}))
	defer srv.Close()

	p := NewGCWProviderWithClient(srv.URL, apa41Project, apa41Location, srv.Client())
	arg := map[string]string{"execution_name": apa41BareID}
	got, err := p.StartExecution(context.Background(), apa41Workflow, arg)
	if err != nil {
		t.Fatalf("StartExecution on 409: %v", err)
	}
	if want := p.ExecutionResourceName(apa41Workflow, apa41BareID); got != want {
		t.Fatalf("409 returned name = %q, want the probe name %q", got, want)
	}
	// Sanity: the identifier that produced it is still bare.
	if strings.Contains(got, "executions/projects") {
		t.Fatalf("name %q looks like a nested identifier", got)
	}
}
