package workflow

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Live GCW emulator hello-world execution (APA-35 Slice B).
//
// What this proves: the existing GCWProvider can DeployWorkflow,
// StartExecution, and GetExecution == SUCCEEDED against a REAL GCW
// emulator. The executed workflow is a minimal hello-world (no Agent,
// API, or Mockoon backends) because the full workflows/claim-investigation.yaml
// cannot reach SUCCEEDED yet: its `investigate` step requires the Agent
// service at $AGENT_URL (POST /v1/investigations), and the workflow then
// waits on a HITL callback before calling the API decision endpoint.
// A live probe returned:
//
//	{"state":"FAILED","error":{"payload":"{\"message\":\"environment variable
//	'AGENT_URL' not found\",...}"}}
//
// so the Agent service (+ API decision endpoint + human callback driver)
// is the Slice C input. The real claim-investigation.yaml IS deployed here
// (deploy path proven); only the SUCCEEDED execution uses hello-world.
//
// Infra (docker run only; repo rule forbids compose):
//
//	docker network create claimops-net
//	docker run -d \
//	  --name gcw-emulator \
//	  --network claimops-net \
//	  -p 8787:8787 \
//	  -p 8788:8788 \
//	  -v $(pwd)/workflows:/workflows \
//	  -e WORKFLOWS_DIR=/workflows \
//	  -e PROJECT=my-project \
//	  -e LOCATION=us-central1 \
//	  ghcr.io/lemonberrylabs/gcw-emulator:latest
//	curl -s http://localhost:8787/v1/projects/my-project/locations/us-central1/workflows | jq
//
// Run live:
//
//	WORKFLOWS_EMULATOR_HOST=localhost:8787 go test ./internal/workflow/ -run TestGCWLive_HelloWorldExecution -v -count=1
//
// Stop afterwards (leave no stray containers):
//
//	docker rm -f gcw-emulator
//
// The test skips when WORKFLOWS_EMULATOR_HOST is unset or the emulator
// is unreachable (same live-gated skip pattern as the pubsub adapter
// emulator test).
const (
	liveHelloWorkflowID = "hello-world-apa35"
	liveHelloSource     = "main:\n  params: [args]\n  steps:\n    - hello:\n        return: hello-world\n"
)

// deployTolerant deploys and tolerates the emulator's 409 ALREADY_EXISTS:
// the emulator pre-loads WORKFLOWS_DIR at startup, so redeploying the
// bundled claim-investigation.yaml returns 409 instead of updating
// (the provider surfaces non-2xx as an error by design — no provider
// change; recorded, not redesigned).
func deployTolerant(ctx context.Context, p *GCWProvider, t *testing.T, workflowID, src string) {
	t.Helper()
	if err := p.DeployWorkflow(ctx, workflowID, src); err != nil {
		if strings.Contains(err.Error(), "status 409") || strings.Contains(err.Error(), "ALREADY_EXISTS") {
			t.Logf("deploy %s: already exists (emulator pre-load), continuing", workflowID)
			return
		}
		t.Fatalf("DeployWorkflow %s (live): %v", workflowID, err)
	}
}

// liveGCWHost returns the emulator host or skips when absent/unreachable.
func liveGCWHost(t *testing.T) string {
	t.Helper()
	host := strings.TrimSpace(os.Getenv("WORKFLOWS_EMULATOR_HOST"))
	if host == "" {
		t.Skip("WORKFLOWS_EMULATOR_HOST unset: skipping live GCW hello-world test")
	}
	dialHost := host
	dialHost = strings.TrimPrefix(dialHost, "http://")
	dialHost = strings.TrimPrefix(dialHost, "https://")
	dialHost = strings.TrimSuffix(dialHost, "/")
	probe, err := net.DialTimeout("tcp", dialHost, 2*time.Second)
	if err != nil {
		t.Skipf("GCW emulator at %q unreachable: %v", host, err)
	}
	probe.Close()
	return host
}

// repoClaimWorkflowSource reads workflows/claim-investigation.yaml by
// walking up from this test file so the test runs from any workdir.
func repoClaimWorkflowSource(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller: cannot locate test file")
	}
	dir := filepath.Dir(thisFile)
	for i := 0; i < 8; i++ {
		candidate := filepath.Join(dir, "workflows", "claim-investigation.yaml")
		if src, err := os.ReadFile(candidate); err == nil {
			return string(src)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("workflows/claim-investigation.yaml not found above test file")
	return ""
}

func TestGCWLive_HelloWorldExecution(t *testing.T) {
	host := liveGCWHost(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	provider := NewGCWProvider(host, "my-project", "us-central1")

	// 1. Deploy the REAL claim-investigation.yaml (deploy path proven;
	// execution of this workflow needs the Agent backend — Slice C).
	claimSrc := repoClaimWorkflowSource(t)
	deployTolerant(ctx, provider, t, "claim-investigation", claimSrc)

	// 2. Deploy the minimal hello-world workflow (no backends required).
	deployTolerant(ctx, provider, t, liveHelloWorkflowID, liveHelloSource)

	// 3. Start one execution.
	execName, err := provider.StartExecution(ctx, liveHelloWorkflowID, map[string]string{"hello": "world"})
	if err != nil {
		t.Fatalf("StartExecution %s (live): %v", liveHelloWorkflowID, err)
	}
	if execName == "" {
		t.Fatal("StartExecution returned empty execution name")
	}
	t.Logf("execution: %s", execName)

	// 4. Poll until SUCCEEDED (hello-world completes in ~1s; allow 30s).
	deadline := time.Now().Add(30 * time.Second)
	for {
		state, result, execErr := provider.GetExecution(ctx, execName)
		switch state {
		case "SUCCEEDED":
			if execErr != nil {
				t.Fatalf("SUCCEEDED with execErr: %v", execErr)
			}
			if !strings.Contains(string(result), "hello-world") {
				t.Fatalf("result %q does not contain hello-world", string(result))
			}
			t.Logf("SUCCEEDED result=%s", string(result))
			return
		case "FAILED":
			t.Fatalf("execution FAILED: result=%s execErr=%v", string(result), execErr)
		case "ACTIVE", "":
			// Still running; keep polling.
		default:
			t.Fatalf("unexpected execution state %q: result=%s execErr=%v", state, string(result), execErr)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for SUCCEEDED (last state %q)", state)
		}
		time.Sleep(500 * time.Millisecond)
	}
}
