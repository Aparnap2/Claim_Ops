package tools

// APA-43, Phase-3 plan step 9 remainder, CASE 4: Mockoon unavailable,
// upstream semantics — qualified against a REAL dead origin.
//
// ALREADY PROVEN (not repeated here)
// ---------------------------------
//
//	Mockoon-UP tool matrix ... internal/investigate/tools/mockoon_test.go
//	                        (TestLiveToolMatrix, T2/T8/T9/T10)
//	ErrUpstream wrapping .... internal/adapters/http/client.go:84
//	                        (asserted with doubles at
//	                         internal/investigate/integration_test.go:122)
//
// WHAT ONLY A REAL DEAD ORIGIN SHOWS
// ---------------------------------
//
// A double returns an error the test itself wrote, so it proves nothing
// about classification: the test author chose the sentinel. Here the
// error is produced by the production transport (net/http) against a
// real closed TCP port, and the assertion is that the REAL tool path
// classifies that un-authored error as the RETRYABLE class
// (ports.ErrUpstream) and never as a permanent one
// (ports.ErrContract / ErrTenantMismatch / ErrNotFound).
//
// The consequence that makes this a failure-matrix case rather than an
// error-wrapping tautology: a permanent classification here would park
// the investigation in the poison path (the outbox dispatcher and the
// orchestrator both treat contract failures as non-retryable), so a dead
// upstream would strand work instead of retrying it. The classification
// IS the behaviour under test.
//
// NO SERVICE REQUIRED, AND THAT IS THE POINT
// ------------------------------------------
//
// This case needs nothing running: a closed local port IS the dead
// origin, so it never skips. It is still a real-transport case because
// every component under test is production code over a real socket —
// httpadapter.Client, the four real HTTP clients, and the real tools —
// and the only thing absent is the peer, which is precisely the
// condition being qualified.

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	httpadapter "claimops-api/internal/adapters/http"
	"claimops-api/internal/ports"
)

// deadOriginURL returns a loopback URL whose port is closed at the moment
// of the call. Binding :0 lets the OS pick a free port; closing the
// listener releases it, so a connection there is refused. The bind is
// retried because a just-released port can be handed to another process
// in the same instant, which would make the "dead" origin live.
func deadOriginURL(t *testing.T) string {
	t.Helper()
	for attempt := 0; attempt < 5; attempt++ {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("reserve a closed port (attempt %d): %v", attempt+1, err)
		}
		addr := l.Addr().String()
		if err := l.Close(); err != nil {
			t.Fatalf("release the reserved port %s: %v", addr, err)
		}
		c, derr := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if derr == nil {
			// Something else grabbed it between Close and Dial: that port
			// is not dead, so take another.
			_ = c.Close()
			continue
		}
		return "http://" + addr
	}
	t.Fatal("could not obtain a port that stayed closed across 5 attempts")
	return ""
}

// assertRetryableUpstream fails unless err is classified retryable: it
// must wrap ports.ErrUpstream and must NOT wrap any permanent sentinel.
// Both halves are required — an error wrapping everything would satisfy
// the first and defeat the retry policy.
func assertRetryableUpstream(t *testing.T, label string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: dead origin returned NO error: an unreachable peer must never look like success", label)
	}
	if !errors.Is(err, ports.ErrUpstream) {
		t.Errorf("%s: error = %v, want one wrapping ports.ErrUpstream (the retryable class)", label, err)
	}
	for _, permanent := range []struct {
		name string
		sent error
	}{
		{"ErrContract", ports.ErrContract},
		{"ErrTenantMismatch", ports.ErrTenantMismatch},
		{"ErrNotFound", ports.ErrNotFound},
	} {
		if errors.Is(err, permanent.sent) {
			t.Errorf("%s: error = %v, also wraps %s: a dead origin is TRANSIENT and must never be classified %s (it would strand the investigation instead of retrying it)",
				label, err, permanent.name, permanent.name)
		}
	}
}

// deadOriginToolTimeout is the per-call budget. A refused dial fails far
// inside it, so a pass proves the transport was reached and classified,
// not merely that some deadline fired.
const deadOriginToolTimeout = 3 * time.Second

// TestLiveInfraMatrix_MockoonDown_IsRetryableUpstream is CASE 4 of the
// infrastructure failure matrix: a dead origin must surface as the
// retryable ports.ErrUpstream through the REAL tool path, never as a
// permanent contract failure, and must pin no evidence.
//
// Every tool is driven through its REAL HTTP adapter (the same
// httpadapter.New*Client the worker wires) with only the pinner faked, so
// the classification under test is production behaviour. The zero-pins
// assertion matters as much as the sentinel: an upstream failure that
// still pinned evidence would fabricate provenance for bytes that were
// never received.
func TestLiveInfraMatrix_MockoonDown_IsRetryableUpstream(t *testing.T) {
	origin := deadOriginURL(t)
	t.Logf("dead origin: %s (bound :0, released, connection refused)", origin)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	probes := []struct {
		name string
		run  func(t *testing.T, pin *fakePinner)
	}{
		{
			name: "T2_policy",
			run: func(t *testing.T, pin *fakePinner) {
				var port ports.PolicyPort = httpadapter.NewPolicyClient(httpadapter.New(origin, deadOriginToolTimeout))
				resp, err := NewPolicyTool(port, pin)(ctx, PolicyRequest{
					TenantID: toolsTenant, ClaimID: toolsClaim,
					InvestigationID: toolsInv, RequestID: toolsReq,
					PolicyID: "POL-001",
				})
				assertRetryableUpstream(t, "T2_policy", err)
				if resp.Policy.PolicyID != "" || resp.EvidenceID != "" {
					t.Errorf("T2_policy: response carries data from a dead origin: %+v", resp)
				}
			},
		},
		{
			name: "T8_tpa",
			run: func(t *testing.T, pin *fakePinner) {
				var port ports.ClaimsPort = httpadapter.NewTPAClient(httpadapter.New(origin, deadOriginToolTimeout))
				resp, err := NewTPATool(port, pin)(ctx, TPARequest{
					TenantID: toolsTenant, ClaimID: toolsClaim,
					InvestigationID: toolsInv, RequestID: toolsReq,
					PolicyID: "POL-001",
				})
				assertRetryableUpstream(t, "T8_tpa", err)
				if len(resp.Claims) != 0 || resp.EvidenceID != "" {
					t.Errorf("T8_tpa: response carries data from a dead origin: %+v", resp)
				}
			},
		},
		{
			name: "T9_provider",
			run: func(t *testing.T, pin *fakePinner) {
				var port ports.ProviderPort = httpadapter.NewProviderClient(httpadapter.New(origin, deadOriginToolTimeout))
				resp, err := NewProviderTool(port, pin)(ctx, ProviderRequest{
					TenantID: toolsTenant, ClaimID: toolsClaim,
					InvestigationID: toolsInv, RequestID: toolsReq,
					EncounterID: "ENC-100", AllowedEncounters: []string{"ENC-100"},
				})
				assertRetryableUpstream(t, "T9_provider", err)
				if resp.Encounter.EncounterID != "" || resp.EvidenceID != "" {
					t.Errorf("T9_provider: response carries data from a dead origin: %+v", resp)
				}
			},
		},
		{
			name: "T10_risk",
			run: func(t *testing.T, pin *fakePinner) {
				var port ports.RiskPort = httpadapter.NewRiskClient(httpadapter.New(origin, deadOriginToolTimeout))
				resp, err := NewRiskTool(port, pin)(ctx, RiskRequest{
					TenantID: toolsTenant, ClaimID: toolsClaim,
					InvestigationID: toolsInv, RequestID: toolsReq,
					SubjectID: "PAT-001",
				})
				assertRetryableUpstream(t, "T10_risk", err)
				if resp.EvidenceID != "" {
					t.Errorf("T10_risk: response carries data from a dead origin: %+v", resp)
				}
			},
		},
	}

	for _, p := range probes {
		t.Run(p.name, func(t *testing.T) {
			pin := &fakePinner{id: "ev-apa43-dead"}
			p.run(t, pin)
			if pin.calls != 0 {
				t.Errorf("%s: pinner called %d time(s) for a failed upstream read: evidence must never be pinned for bytes that were not received", p.name, pin.calls)
			}
		})
	}
}

// TestLiveInfraMatrix_MockoonDown_ClientClassifiesStatusAndTransport is the
// adapter-level half of CASE 4, kept separate so the classification is
// pinned at BOTH layers: the shared port reader every outbound tool uses,
// and the tools above.
//
// It contrasts the failure shapes a real Mockoon outage can produce,
// because only contrasting them shows the split is deliberate rather than
// an artefact of one code path:
//
//	dead origin                 -> ports.ErrUpstream  (transport, RETRYABLE)
//	HTTP 503                    -> ports.ErrUpstream  (5xx, RETRYABLE)
//	HTTP 200 with a bad payload -> permanent          (PERMANENT, not retryable)
//
// The 200-but-invalid case is served over a real socket by httptest, so
// the permanent half is proven by running production classification
// rather than asserted by reading the source.
func TestLiveInfraMatrix_MockoonDown_ClientClassifiesStatusAndTransport(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	t.Run("dead_origin_is_upstream", func(t *testing.T) {
		origin := deadOriginURL(t)
		var port ports.PolicyPort = httpadapter.NewPolicyClient(httpadapter.New(origin, deadOriginToolTimeout))
		_, err := NewPolicyTool(port, &fakePinner{id: "ev-apa43-dead"})(ctx, PolicyRequest{
			TenantID: toolsTenant, ClaimID: toolsClaim,
			InvestigationID: toolsInv, RequestID: toolsReq, PolicyID: "POL-001",
		})
		assertRetryableUpstream(t, "dead_origin", err)
		t.Logf("dead_origin classified: %v", err)
	})

	t.Run("http_503_is_upstream", func(t *testing.T) {
		srv := fixedResponseServer(http.StatusServiceUnavailable, `{"error":"mockoon down"}`)
		defer srv.Close()
		var port ports.PolicyPort = httpadapter.NewPolicyClient(httpadapter.New(srv.URL, deadOriginToolTimeout))
		pin := &fakePinner{id: "ev-apa43-503"}
		_, err := NewPolicyTool(port, pin)(ctx, PolicyRequest{
			TenantID: toolsTenant, ClaimID: toolsClaim,
			InvestigationID: toolsInv, RequestID: toolsReq, PolicyID: "POL-001",
		})
		assertRetryableUpstream(t, "http_503", err)
		if pin.calls != 0 {
			t.Errorf("http_503: pinner called %d time(s) for a 5xx response: evidence must not be pinned for an error body", pin.calls)
		}
		t.Logf("http_503 classified: %v", err)
	})

	t.Run("http_200_wrong_shape_is_permanent", func(t *testing.T) {
		// The contrast that makes the split meaningful: a reachable origin
		// answering 200 with a body the contract rejects is PERMANENT, so
		// the ErrUpstream results above cannot be every error taking one
		// path.
		srv := fixedResponseServer(http.StatusOK, `{"totally":"not a policy"}`)
		defer srv.Close()
		var port ports.PolicyPort = httpadapter.NewPolicyClient(httpadapter.New(srv.URL, deadOriginToolTimeout))
		pin := &fakePinner{id: "ev-apa43-contract"}
		_, err := NewPolicyTool(port, pin)(ctx, PolicyRequest{
			TenantID: toolsTenant, ClaimID: toolsClaim,
			InvestigationID: toolsInv, RequestID: toolsReq, PolicyID: "POL-001",
		})
		if err == nil {
			t.Fatal("http_200_wrong_shape: a contract-violating payload must be rejected, not accepted")
		}
		if errors.Is(err, ports.ErrUpstream) {
			t.Errorf("http_200_wrong_shape: err = %v, classified ErrUpstream; a reachable origin serving a contract-violating payload is PERMANENT and must not be retryable", err)
		}
		// Assert the PERMANENT class, not one exact sentinel: which
		// validation fires first (decode vs projection vs canonicalize)
		// is an implementation choice, but all three are permanent and the
		// retry policy only cares about that. MEASURED here: ErrContract.
		permanent := false
		for _, sent := range []error{ports.ErrContract, ports.ErrNotFound, ports.ErrTenantMismatch} {
			if errors.Is(err, sent) {
				permanent = true
				break
			}
		}
		if !permanent {
			t.Errorf("http_200_wrong_shape: err = %v wraps no permanent sentinel (ErrContract/ErrNotFound/ErrTenantMismatch); a reachable-but-invalid origin must classify as permanent", err)
		}
		if pin.calls != 0 {
			t.Errorf("http_200_wrong_shape: pinner called %d time(s) for a rejected payload: evidence must not be pinned for bytes the contract refused", pin.calls)
		}
		t.Logf("http_200_wrong_shape classified as permanent: %v", err)
	})
}

// fixedResponseServer serves one fixed status and body on every request,
// over a real loopback socket.
func fixedResponseServer(status int, body string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	return srv
}
