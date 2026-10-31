// APA-37 part 1: repo-side conformance gate for the GCW emulator
// step-level `retry:` defect. TEST-ONLY: no production file is changed
// (workflows/claim-investigation.yaml is untouched and stays frozen with
// its `retry:` blocks), the emulator is never patched or forked, and no
// assert in this file may be weakened to make it pass.
//
// DEFECT (reproduced 2026-09-27 against ghcr.io/lemonberrylabs/gcw-emulator
// :latest = v0.5.0, commit d803aa3aa626e9a0129c24c1421e51913d9f98bd): the
// emulator mis-parses a step-level `retry:` block as a `try` (parser.go
// 366-374 synthesizes an empty TryExpr), so the engine reads the resulting
// empty step list as FlowEnd (engine.go:145-147) and the execution
// terminates SUCCEEDED with result null AT that step instead of continuing;
// >=400 responses additionally never retry.
//
// UPSTREAM STATUS: no newer emulator release, no upstream issue, and no
// config flag to work around it (env vars are HOST/PORT/PROJECT/LOCATION/
// BASE_URL/WORKFLOWS_DIR only), so the fix is upstream — these tests are a
// conformance gate that is EXPECTED TO FAIL until the emulator is bumped or
// fixed, added alongside PR #101.
//
// WHAT IS ASSERTED (three purpose-built conformance fixtures, rendered into
// the test's OWN temp dir — never the repo's production workflows/ dir and
// never presented as production qualification):
//
//  1. TestGCWLive_RetryStepContinuesAfterSuccess — a successful http.post
//     carrying `retry: predicate: ${http.default_retry_predicate}` must
//     CONTINUE to the next step and return that step's value. This is the
//     gate that bites today: v0.5.0 ends the run SUCCEEDED/null.
//  2. TestGCWLive_RetryStepControlWithoutRetryBlock — the identical
//     fixture MINUS the `retry:` block must reach the following step even
//     on v0.5.0. It is the control that proves the harness and fixture are
//     sound, so a failure of (1) is attributable to `retry:` alone.
//  3. TestGCWLive_RetryStepErrorResponseIsHonest — a >=400 response on a
//     `retry:` step must either be retried or fail explicitly; it must
//     never be reported as success.
//
// The workflow backend is an in-process stub, not a real service, so the
// only thing under test is emulator retry/continuation semantics.
//
// Infra (docker run only; repo rule forbids compose). WORKFLOWS_DIR points
// at a scratch dir so nothing from the repo's production workflows/ is
// pre-loaded, and --add-host host-gateway lets the emulator container reach
// this test's stub server by name:
//
//	docker network create claimops-net
//	mkdir -p /tmp/apa37-wf
//	docker run -d --name gcw-emulator --network claimops-net \
//	  -p 8787:8787 -p 8788:8788 \
//	  --add-host host.docker.internal:host-gateway \
//	  -v /tmp/apa37-wf:/workflows \
//	  -e WORKFLOWS_DIR=/workflows -e PROJECT=my-project -e LOCATION=us-central1 \
//	  ghcr.io/lemonberrylabs/gcw-emulator:latest
//
// Run live (all three):
//
//	WORKFLOWS_EMULATOR_HOST=localhost:8787 go test ./internal/workflow/ \
//	  -run 'TestGCWLive_RetryStep' -v -count=1
//
// If the emulator is instead run with --network host, set
// GCW_FIXTURE_BASE_HOST=127.0.0.1 so the fixture URLs resolve inside the
// container. Stop afterwards (leave no stray containers):
//
//	docker rm -f gcw-emulator
//
// The tests skip when WORKFLOWS_EMULATOR_HOST is unset or the emulator is
// unreachable (same live-gated skip pattern as gcw_live_test.go), so a
// repo-wide `go test ./...` stays green without the emulator.
package workflow

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	// conformanceEchoToken is returned by the stub and is what the
	// post-retry step returns. Its presence in the execution result is the
	// only proof that the workflow continued PAST the retried step.
	conformanceEchoToken = "apa37-conformance-continued"
	// conformanceOKPath answers 200 with conformanceEchoToken.
	conformanceOKPath = "/apa37/ok"
	// conformanceFailPath answers 503 permanently, so any SUCCEEDED
	// terminal state on it is provably a swallowed error.
	conformanceFailPath = "/apa37/always-503"
	// conformanceBasePlaceholder is substituted with the stub base URL.
	conformanceBasePlaceholder = "__CONFORMANCE_BASE__"
)

// successWithRetryFixture is the APA-37 success fixture. The shape of the
// `retry:` block is copied verbatim from the frozen
// workflows/claim-investigation.yaml `investigate` step so the gate tracks
// production semantics without being a copy of the production workflow:
// this is a 20-line probe with one stub URL, not a claim investigation.
const successWithRetryFixture = `# APA-37 conformance fixture (test-only probe; not production).
main:
  params: [args]
  steps:
    - post_ok:
        call: http.post
        args:
          url: "` + conformanceBasePlaceholder + conformanceOKPath + `"
          body:
            case: apa37-retry-success
          timeout: 10
        retry:
          predicate: ${http.default_retry_predicate}
          max_retries: 3
          backoff:
            initial_delay: 1
            max_delay: 10
            multiplier: 2
        result: posted
    - after_ok:
        return: ${posted.body.echo}
`

// successWithoutRetryFixture is the control: byte-identical to the fixture
// above with the `retry:` block removed. It must reach `after_ok` on
// v0.5.0; if it does not, the harness is broken and the sibling assert
// cannot be trusted to blame `retry:`.
const successWithoutRetryFixture = `# APA-37 conformance fixture (test-only probe; not production).
main:
  params: [args]
  steps:
    - post_ok:
        call: http.post
        args:
          url: "` + conformanceBasePlaceholder + conformanceOKPath + `"
          body:
            case: apa37-no-retry-control
          timeout: 10
        result: posted
    - after_ok:
        return: ${posted.body.echo}
`

// errorWithRetryFixture is the APA-37 honesty fixture: the same `retry:`
// step against an endpoint that answers 503 on every call.
const errorWithRetryFixture = `# APA-37 conformance fixture (test-only probe; not production).
main:
  params: [args]
  steps:
    - post_boom:
        call: http.post
        args:
          url: "` + conformanceBasePlaceholder + conformanceFailPath + `"
          body:
            case: apa37-retry-error
          timeout: 10
        retry:
          predicate: ${http.default_retry_predicate}
          max_retries: 2
          backoff:
            initial_delay: 1
            max_delay: 2
            multiplier: 2
        result: posted
    - after_boom:
        return: ${posted.body.echo}
`

// conformanceStub is the in-process workflow backend. Hits are counted so a
// test can prove the http.post step actually executed (and, for the error
// case, whether the `retry:` block was honored at all) instead of trusting
// an execution that may have short-circuited before the call.
type conformanceStub struct {
	okHits   atomic.Int64
	failHits atomic.Int64
}

// handler routes the two conformance endpoints. /ok always answers 200
// carrying the echo token; /always-503 always answers 503, so no honest
// implementation can ever report success for it.
func (s *conformanceStub) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(conformanceOKPath, func(w http.ResponseWriter, _ *http.Request) {
		s.okHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"echo":"`+conformanceEchoToken+`","status":200}`)
	})
	mux.HandleFunc(conformanceFailPath, func(w http.ResponseWriter, _ *http.Request) {
		s.failHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":"apa37-conformance-permanent-503"}`)
	})
	return mux
}

// startConformanceStub starts the stub on 0.0.0.0 (not 127.0.0.1) so the
// emulator container can reach it through host.docker.internal, and returns
// the base URL to bake into the fixture.
func startConformanceStub(t *testing.T) (*conformanceStub, string) {
	t.Helper()
	stub := &conformanceStub{}
	// Bind explicitly rather than via httptest.NewUnstartedServer, which
	// always opens a 127.0.0.1 listener the container could not reach.
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("listen conformance stub: %v", err)
	}
	srv := &http.Server{Handler: stub.handler(), ReadHeaderTimeout: 5 * time.Second}
	served := make(chan struct{})
	go func() {
		close(served)
		_ = srv.Serve(ln)
	}()
	<-served
	t.Cleanup(func() { _ = srv.Close() })

	host := strings.TrimSpace(os.Getenv("GCW_FIXTURE_BASE_HOST"))
	if host == "" {
		host = "host.docker.internal"
	}
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("conformance stub listener address %q is not TCP", ln.Addr())
	}
	base := "http://" + host + ":" + strconv.Itoa(addr.Port)
	t.Logf("conformance stub base as seen from the emulator container: %s", base)
	return stub, base
}

// conformanceFixture renders a fixture into the test's OWN temp dir and
// returns the file path. The guard fails if that dir ever lands inside the
// repo working tree: a conformance fixture must never be mistaken for, or
// deployed as, production qualification.
func conformanceFixture(t *testing.T, name, body string) string {
	t.Helper()
	dir := t.TempDir()
	if cwd, err := os.Getwd(); err == nil && strings.HasPrefix(dir, cwd) {
		t.Fatalf("conformance fixture dir %q is inside the repo working tree %q: fixtures must never live in the production workflows/ dir", dir, cwd)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write conformance fixture %s: %v", path, err)
	}
	return path
}

// conformanceDeploy deploys a fixture from disk. It deliberately does NOT
// tolerate 409 ALREADY_EXISTS the way deployTolerant does: workflow IDs here
// are run-unique, so a 409 would mean a stale body was registered under
// that ID and the execution would not prove what this test deployed.
func conformanceDeploy(ctx context.Context, t *testing.T, p *GCWProvider, workflowID, fixturePath string) {
	t.Helper()
	src, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read conformance fixture %s: %v", fixturePath, err)
	}
	if err := p.DeployWorkflow(ctx, workflowID, string(src)); err != nil {
		t.Fatalf("DeployWorkflow %s (live): %v", workflowID, err)
	}
}

// conformanceRandHex returns n random bytes as hex, for run-unique workflow
// IDs so repeated runs against one emulator instance never collide.
func conformanceRandHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(b)
}

// conformanceAwaitTerminal polls until SUCCEEDED or FAILED and returns the
// terminal state, result, and any emulator-provided error payload.
func conformanceAwaitTerminal(ctx context.Context, t *testing.T, p *GCWProvider, execName string, wait time.Duration) (string, json.RawMessage, error) {
	t.Helper()
	deadline := time.Now().Add(wait)
	for {
		state, result, execErr := p.GetExecution(ctx, execName)
		switch state {
		case "SUCCEEDED", "FAILED":
			return state, result, execErr
		case "ACTIVE", "":
			// Still running; keep polling.
		default:
			t.Fatalf("unexpected execution state %q: result=%s execErr=%v", state, string(result), execErr)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for a terminal state (last state %q)", wait, state)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// conformanceIsNullResult reports whether an execution result is the null
// payload the emulator's FlowEnd short-circuit produces.
func conformanceIsNullResult(result json.RawMessage) bool {
	trimmed := strings.Trim(strings.TrimSpace(string(result)), `"`)
	return trimmed == "" || trimmed == "null"
}

// conformanceRunFixture deploys, starts, and drives one fixture to a
// terminal state, returning the terminal state, result, error payload, and
// the number of times the stub was hit.
func conformanceRunFixture(ctx context.Context, t *testing.T, p *GCWProvider, stub *conformanceStub, idPrefix, body, base string, hits *atomic.Int64) (string, json.RawMessage, error, int64) {
	t.Helper()
	workflowID := idPrefix + "-" + conformanceRandHex(t, 6)
	fixturePath := conformanceFixture(t, workflowID+".yaml",
		strings.ReplaceAll(body, conformanceBasePlaceholder, base))
	t.Logf("conformance fixture: %s", fixturePath)
	conformanceDeploy(ctx, t, p, workflowID, fixturePath)

	execName, err := p.StartExecution(ctx, workflowID, map[string]string{})
	if err != nil {
		t.Fatalf("StartExecution %s (live): %v", workflowID, err)
	}
	if execName == "" {
		t.Fatal("StartExecution returned empty execution name")
	}
	t.Logf("execution: %s", execName)

	state, result, execErr := conformanceAwaitTerminal(ctx, t, p, execName, 60*time.Second)
	n := hits.Load()
	t.Logf("terminal: state=%s result=%s execErr=%v stub_hits=%d", state, string(result), execErr, n)
	if n < 1 {
		t.Fatalf("conformance stub was never called (hits=%d): the http.post step did not run, so this execution proves nothing about retry semantics", n)
	}
	return state, result, execErr, n
}

// TestGCWLive_RetryStepContinuesAfterSuccess is the APA-37 gate proper: a
// successful http.post carrying `retry:` must continue to the next step and
// return that step's value. Against emulator v0.5.0 this FAILS, ending
// SUCCEEDED/null at the retried step — the documented defect.
func TestGCWLive_RetryStepContinuesAfterSuccess(t *testing.T) {
	host := liveGCWHost(t)
	stub, base := startConformanceStub(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	provider := NewGCWProvider(host, "my-project", "us-central1")
	state, result, execErr, hits := conformanceRunFixture(ctx, t, provider, stub,
		"apa37-retry-success", successWithRetryFixture, base, &stub.okHits)

	if state == "SUCCEEDED" && conformanceIsNullResult(result) {
		t.Fatalf("emulator step-level `retry:` defect: http.post returned 200 (stub hits=%d) yet the execution terminated SUCCEEDED with result null AT that step — the following step never ran. The emulator mis-parses a step-level `retry:` block as an empty `try`, so the engine reads the empty step list as FlowEnd. The fix is upstream (emulator bump/fix); do NOT weaken this assert and do NOT remove the `retry:` blocks from workflows/claim-investigation.yaml", hits)
	}
	if state != "SUCCEEDED" {
		t.Fatalf("state = %s, want SUCCEEDED (result=%s execErr=%v)", state, string(result), execErr)
	}
	if !strings.Contains(string(result), conformanceEchoToken) {
		t.Fatalf("result = %s, want it to contain %q returned by the step AFTER the retried http.post — the workflow did not continue past the `retry:` step", string(result), conformanceEchoToken)
	}
	t.Logf("retry step continued correctly: %s", string(result))
}

// TestGCWLive_RetryStepControlWithoutRetryBlock is the control for
// TestGCWLive_RetryStepContinuesAfterSuccess: the same fixture with no
// `retry:` block must reach the following step on any emulator version,
// including v0.5.0. It passes today; if it ever fails, the fixture or the
// harness is at fault and the sibling assert is not a valid gate.
func TestGCWLive_RetryStepControlWithoutRetryBlock(t *testing.T) {
	host := liveGCWHost(t)
	stub, base := startConformanceStub(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	provider := NewGCWProvider(host, "my-project", "us-central1")
	state, result, execErr, _ := conformanceRunFixture(ctx, t, provider, stub,
		"apa37-noretry-control", successWithoutRetryFixture, base, &stub.okHits)

	if state != "SUCCEEDED" {
		t.Fatalf("control state = %s, want SUCCEEDED (result=%s execErr=%v): without a `retry:` block the emulator must traverse both steps, so a failure here indicts the fixture or harness, not the defect", state, string(result), execErr)
	}
	if !strings.Contains(string(result), conformanceEchoToken) {
		t.Fatalf("control result = %s, want it to contain %q from the step after http.post", string(result), conformanceEchoToken)
	}
	t.Logf("control ok (no `retry:` block traverses both steps): %s", string(result))
}

// TestGCWLive_RetryStepErrorResponseIsHonest asserts the error path of the
// same `retry:` shape: against a permanently-503 endpoint the step must
// either be retried (more than one call) or fail explicitly. The dishonesty
// this gate exists to catch is a >=400 response being reported as success —
// including the SUCCEEDED/null short-circuit the defect produces.
func TestGCWLive_RetryStepErrorResponseIsHonest(t *testing.T) {
	host := liveGCWHost(t)
	stub, base := startConformanceStub(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	provider := NewGCWProvider(host, "my-project", "us-central1")
	state, result, execErr, hits := conformanceRunFixture(ctx, t, provider, stub,
		"apa37-retry-error", errorWithRetryFixture, base, &stub.failHits)

	retried := hits >= 2
	failedExplicitly := state == "FAILED"
	if !retried && !failedExplicitly {
		t.Fatalf("dishonest error handling: the 503 endpoint was called %d time(s) and the execution ended %s with result %s (execErr=%v) — a >=400 response on a `retry:` step must either be retried (>=2 calls) or fail explicitly, never succeed", hits, state, string(result), execErr)
	}
	if !failedExplicitly {
		t.Fatalf("dishonest error handling: the `retry:` step hit a permanently-503 endpoint %d time(s) yet the execution ended %s with result %s — a >=400 response can never legitimately be reported as success", hits, state, string(result))
	}
	if execErr == nil {
		t.Fatalf("execution FAILED with no emulator-provided error payload (result=%s): the failure must be explicit, not a bare state flip", string(result))
	}
	t.Logf("error path honest: %d call(s) against a permanent 503, terminal %s with error payload", hits, state)
}
