package tools

import (
	"context"
	"reflect"
	"testing"

	"claimops-api/internal/invest"
	"claimops-api/internal/investigate"
	"claimops-api/internal/ports"
)

func verifyFindingsFixture() []Finding {
	return []Finding{
		{Code: "R1", Severity: "HIGH", Message: "policy number conflict", EvidenceIDs: []string{"ev-01", "ev-02"}},
		{Code: "R2", Severity: "LOW", Message: "round amount", EvidenceIDs: []string{"ev-02", "ev-03"}},
	}
}

func verifyRequest(t *testing.T) investigate.Request {
	t.Helper()
	req, err := investigate.NewGetVerificationFindingsRequest(toolsTenant, toolsClaim, toolsInv, toolsReq)
	if err != nil {
		t.Fatalf("build verify request: %v", err)
	}
	return req.ToRequest()
}

func TestVerifyReplayVerbatim(t *testing.T) {
	findings := verifyFindingsFixture()
	typed, err := NewVerifyResponse(findings)
	if err != nil {
		t.Fatalf("NewVerifyResponse: %v", err)
	}
	if !reflect.DeepEqual(typed.Findings, findings) {
		t.Fatalf("replay mismatch:\n got %+v\nwant %+v", typed.Findings, findings)
	}

	fn := NewVerifyTool(findings)
	got, err := fn(context.Background(), verifyRequest(t))
	if err != nil {
		t.Fatalf("verify tool: %v", err)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("response Validate: %v", err)
	}
	if got.Tool != invest.ToolGetVerificationFindings {
		t.Fatalf("tool echo = %q, want get_verification_findings", string(got.Tool))
	}
	wantIDs := []string{"ev-01", "ev-02", "ev-03"}
	if !reflect.DeepEqual(got.IDs, wantIDs) {
		t.Fatalf("envelope IDs = %v, want %v", got.IDs, wantIDs)
	}
}

func TestVerifyEchoChecks(t *testing.T) {
	findings := verifyFindingsFixture()
	fn := NewVerifyTool(findings)
	base := verifyRequest(t)

	cases := map[string]investigate.Request{
		"wrong tool":        mustVerifySwapTool(t, base, invest.ToolGetClaim),
		"blank tenant":      mustVerifySwapTenant(t, base, ""),
		"untrimmed tenant":  mustVerifySwapTenant(t, base, " "+toolsTenant+" "),
		"blank claim":       mustVerifySwapClaim(t, base, ""),
		"bad claim":         mustVerifySwapClaim(t, base, " "+toolsClaim+" "),
		"bad investigation": mustVerifySwapInv(t, base, "bad-id"),
		"blank request":     mustVerifySwapReq(t, base, ""),
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := fn(context.Background(), req)
			if err == nil {
				t.Fatalf("want contract error, got nil")
			}
			requireToolsSentinel(t, err, ports.ErrContract)
		})
	}
}

func mustVerifySwapTool(t *testing.T, base investigate.Request, tool invest.ToolName) investigate.Request {
	t.Helper()
	out := base
	out.Tool = tool
	return out
}

func mustVerifySwapTenant(t *testing.T, base investigate.Request, tenant string) investigate.Request {
	t.Helper()
	out := base
	out.TenantID = tenant
	return out
}

func mustVerifySwapClaim(t *testing.T, base investigate.Request, claim string) investigate.Request {
	t.Helper()
	out := base
	out.ClaimID = claim
	return out
}

func mustVerifySwapInv(t *testing.T, base investigate.Request, inv string) investigate.Request {
	t.Helper()
	out := base
	out.InvestigationID = inv
	return out
}

func mustVerifySwapReq(t *testing.T, base investigate.Request, reqID string) investigate.Request {
	t.Helper()
	out := base
	out.RequestID = reqID
	return out
}

func TestVerifyNoRecompute(t *testing.T) {
	findings := verifyFindingsFixture()
	fn := NewVerifyTool(findings)
	req := verifyRequest(t)

	first, err := fn(context.Background(), req)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}

	// Mutate the source slice and its inner evidence lists after
	// construction: the replay must not change (snapshot at construction).
	findings[0].Message = "mutated"
	findings[0].EvidenceIDs[0] = "mutated"
	findings[1].Code = "MUT"
	findings = append(findings, Finding{Code: "R9", Severity: "HIGH", Message: "late", EvidenceIDs: []string{"ev-09"}})

	second, err := fn(context.Background(), req)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("replay changed after source mutation:\nfirst %+v\nsecond %+v", first, second)
	}
	if !reflect.DeepEqual(second.IDs, []string{"ev-01", "ev-02", "ev-03"}) {
		t.Fatalf("IDs after mutation = %v, want original union", second.IDs)
	}

	// NewVerifyResponse deep-copies too: mutating the caller slice must
	// not rewrite the typed replay.
	src := verifyFindingsFixture()
	typed, err := NewVerifyResponse(src)
	if err != nil {
		t.Fatalf("NewVerifyResponse: %v", err)
	}
	src[0].EvidenceIDs[0] = "mutated"
	if typed.Findings[0].EvidenceIDs[0] == "mutated" {
		t.Fatalf("typed replay aliases caller evidence slice")
	}
}

func TestVerifyInvalidFindingFailsClosed(t *testing.T) {
	bad := []Finding{{Code: "", Severity: "HIGH", Message: "x", EvidenceIDs: []string{"ev-01"}}}
	if _, err := NewVerifyResponse(bad); err == nil {
		t.Fatalf("want contract error for blank code")
	} else {
		requireToolsSentinel(t, err, ports.ErrContract)
	}
	fn := NewVerifyTool(bad)
	if _, err := fn(context.Background(), verifyRequest(t)); err == nil {
		t.Fatalf("want contract error on every call for bad snapshot")
	} else {
		requireToolsSentinel(t, err, ports.ErrContract)
	}
}
