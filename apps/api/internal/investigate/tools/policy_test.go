package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"testing"

	"claimops-api/internal/ports"
)

func toolsPolicyReq() PolicyRequest {
	return PolicyRequest{
		TenantID:        toolsTenant,
		ClaimID:         toolsClaim,
		InvestigationID: toolsInv,
		RequestID:       toolsReq,
		PolicyID:        "pol-54-01",
	}
}

func toolsPolicyStatusReq() PolicyStatusRequest {
	return PolicyStatusRequest{
		TenantID:        toolsTenant,
		ClaimID:         toolsClaim,
		InvestigationID: toolsInv,
		RequestID:       toolsReq,
		PolicyID:        "pol-54-01",
	}
}

func TestPolicyValidPinEvidenceIDProvenanceHash(t *testing.T) {
	port := &fakePolicyPort{info: validPolicyInfo(toolsTenant)}
	pin := &fakePinner{id: "ev-pol-01"}
	resp, err := NewPolicyTool(port, pin)(context.Background(), toolsPolicyReq())
	if err != nil {
		t.Fatalf("valid policy: %v", err)
	}
	if resp.EvidenceID != "ev-pol-01" {
		t.Fatalf("EvidenceID = %q, want %q", resp.EvidenceID, "ev-pol-01")
	}
	if pin.calls != 1 {
		t.Fatalf("pin calls = %d, want 1", pin.calls)
	}
	if pin.gotTenant != toolsTenant || pin.gotClaim != toolsClaim {
		t.Fatalf("pin echo tenant=%q claim=%q", pin.gotTenant, pin.gotClaim)
	}
	if pin.gotType != "policy" || pin.gotSource != "pol-54-01" {
		t.Fatalf("pin source type=%q id=%q", pin.gotType, pin.gotSource)
	}
	sum := sha256.Sum256(pin.gotBytes)
	if want := hex.EncodeToString(sum[:]); resp.ContentHash != want {
		t.Fatalf("ContentHash = %q, want sha256(pin bytes) %q", resp.ContentHash, want)
	}
	if len(pin.gotBytes) == 0 {
		t.Fatalf("pinned bytes empty")
	}
	// Pinned bytes are the full canonical payload: must decode to the projection.
	var decoded ports.PolicyInfo
	if err := json.Unmarshal(pin.gotBytes, &decoded); err != nil {
		t.Fatalf("pinned bytes not JSON: %v", err)
	}
	if !reflect.DeepEqual(decoded, validPolicyInfo(toolsTenant)) {
		t.Fatalf("pinned payload != projection:\n%#v\n%#v", decoded, validPolicyInfo(toolsTenant))
	}
	if resp.Truncated {
		t.Fatalf("Truncated = true, want false (single-row fetch)")
	}
}

func TestPolicyStatusExactFiveJSONKeys(t *testing.T) {
	port := &fakePolicyPort{info: validPolicyInfo(toolsTenant)}
	resp, err := NewPolicyStatusTool(port)(context.Background(), toolsPolicyStatusReq())
	if err != nil {
		t.Fatalf("valid status: %v", err)
	}
	raw, err := json.Marshal(resp.Status)
	if err != nil {
		t.Fatalf("marshal status: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal status: %v", err)
	}
	want := map[string]bool{
		"policy_id":      true,
		"status":         true,
		"effective_from": true,
		"effective_to":   true,
		"source_ref":     true,
	}
	if len(m) != len(want) {
		t.Fatalf("key count = %d (%v), want 5 %v", len(m), keysOf(m), keysOfBool(want))
	}
	for k := range m {
		if !want[k] {
			t.Fatalf("unexpected key %q in %v", k, keysOf(m))
		}
	}
	for k := range want {
		if _, ok := m[k]; !ok {
			t.Fatalf("missing key %q in %v", k, keysOf(m))
		}
	}
	if resp.Truncated {
		t.Fatalf("Truncated = true, want false")
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func keysOfBool(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestPolicyStatusNoPinProof(t *testing.T) {
	// By construction NewPolicyStatusTool takes no Pinner, so pinning is
	// impossible, not merely avoided: reflect over the response type proves
	// there are no evidence fields to carry a pin.
	rt := reflect.TypeOf(PolicyStatusResponse{})
	for i := range rt.NumField() {
		n := rt.Field(i).Name
		if n == "EvidenceID" || n == "ContentHash" || n == "Pin" || n == "Pinner" {
			t.Fatalf("T7 response must not carry evidence field %q", n)
		}
	}
	port := &fakePolicyPort{info: validPolicyInfo(toolsTenant)}
	resp, err := NewPolicyStatusTool(port)(context.Background(), toolsPolicyStatusReq())
	if err != nil {
		t.Fatalf("valid status: %v", err)
	}
	if resp.Status.PolicyID != "pol-54-01" {
		t.Fatalf("PolicyID = %q", resp.Status.PolicyID)
	}
}

func TestPolicyErrorMapping(t *testing.T) {
	cases := []struct {
		name     string
		sentinel error
	}{
		{"malformed-contract", ports.ErrContract},
		{"transient-upstream", ports.ErrUpstream},
		{"permanent-notfound", ports.ErrNotFound},
		{"tenant-mismatch", ports.ErrTenantMismatch},
	}
	for _, tc := range cases {
		t.Run("T2/"+tc.name, func(t *testing.T) {
			port := &fakePolicyPort{err: toolsErr(tc.sentinel, tc.name)}
			_, err := NewPolicyTool(port, &fakePinner{id: "ev-x"})(context.Background(), toolsPolicyReq())
			requireToolsSentinel(t, err, tc.sentinel)
		})
		t.Run("T7/"+tc.name, func(t *testing.T) {
			port := &fakePolicyPort{err: toolsErr(tc.sentinel, tc.name)}
			_, err := NewPolicyStatusTool(port)(context.Background(), toolsPolicyStatusReq())
			requireToolsSentinel(t, err, tc.sentinel)
		})
	}
}

func TestPolicyInvalidRequest(t *testing.T) {
	port := &fakePolicyPort{info: validPolicyInfo(toolsTenant)}
	req := toolsPolicyReq()
	req.PolicyID = "  "
	_, err := NewPolicyTool(port, &fakePinner{id: "ev-x"})(context.Background(), req)
	requireToolsSentinel(t, err, ports.ErrContract)
	sreq := toolsPolicyStatusReq()
	sreq.PolicyID = ""
	_, err = NewPolicyStatusTool(port)(context.Background(), sreq)
	requireToolsSentinel(t, err, ports.ErrContract)
}
