package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"claimops-api/internal/ports"
)

func toolsTPAReq() TPARequest {
	return TPARequest{
		TenantID:        toolsTenant,
		ClaimID:         toolsClaim,
		InvestigationID: toolsInv,
		RequestID:       toolsReq,
		PolicyID:        "pol-54-01",
	}
}

func TestTPAValid(t *testing.T) {
	port := &fakeClaimsPort{items: validPriorClaims(toolsTenant)}
	pin := &fakePinner{id: "ev-tpa-01"}
	resp, err := NewTPATool(port, pin)(context.Background(), toolsTPAReq())
	if err != nil {
		t.Fatalf("valid tpa: %v", err)
	}
	if len(resp.Claims) != 2 {
		t.Fatalf("claims = %d, want 2", len(resp.Claims))
	}
	if resp.Truncated {
		t.Fatalf("Truncated = true, want false")
	}
	if resp.EvidenceID != "ev-tpa-01" {
		t.Fatalf("EvidenceID = %q", resp.EvidenceID)
	}
	sum := sha256.Sum256(pin.gotBytes)
	if want := hex.EncodeToString(sum[:]); resp.ContentHash != want {
		t.Fatalf("ContentHash = %q, want %q", resp.ContentHash, want)
	}
	if pin.gotType != "tpa" || pin.gotSource != "pol-54-01" {
		t.Fatalf("pin source type=%q id=%q", pin.gotType, pin.gotSource)
	}
	if pin.gotTenant != toolsTenant || pin.gotClaim != toolsClaim {
		t.Fatalf("pin echo tenant=%q claim=%q", pin.gotTenant, pin.gotClaim)
	}
}

func TestTPATenantDriftFailsWholeCall(t *testing.T) {
	items := validPriorClaims(toolsTenant)
	items[1].TenantID = "tnt-other"
	port := &fakeClaimsPort{items: items}
	_, err := NewTPATool(port, &fakePinner{id: "ev-x"})(context.Background(), toolsTPAReq())
	requireToolsSentinel(t, err, ports.ErrTenantMismatch)
}

func TestTPATruncateFlagAt20(t *testing.T) {
	items := make([]ports.PriorClaim, 0, 21)
	for i := range 21 {
		items = append(items, ports.PriorClaim{
			ClaimID:       fmt.Sprintf("clm-old-%02d", i),
			TenantID:      toolsTenant,
			Status:        "SETTLED",
			ApprovedPaise: int64(1000 + i),
		})
	}
	pin := &fakePinner{id: "ev-tpa-21"}
	resp, err := NewTPATool(&fakeClaimsPort{items: items}, pin)(context.Background(), toolsTPAReq())
	if err != nil {
		t.Fatalf("21 items: %v", err)
	}
	if len(resp.Claims) != MaxTPARows {
		t.Fatalf("claims = %d, want %d", len(resp.Claims), MaxTPARows)
	}
	if !resp.Truncated {
		t.Fatalf("Truncated = false, want true for 21 > 20")
	}
	for i := range MaxTPARows {
		if resp.Claims[i].ClaimID != items[i].ClaimID {
			t.Fatalf("claims[%d] = %q, want %q (verbatim prefix)", i, resp.Claims[i].ClaimID, items[i].ClaimID)
		}
	}
	// Pinned bytes are the FULL pre-truncation payload (21 items).
	var pinned []ports.PriorClaim
	if err := json.Unmarshal(pin.gotBytes, &pinned); err != nil {
		t.Fatalf("pinned bytes not JSON: %v", err)
	}
	if len(pinned) != 21 {
		t.Fatalf("pinned items = %d, want 21 (full payload)", len(pinned))
	}
}

func TestTPAEmptySuccess(t *testing.T) {
	pin := &fakePinner{id: "ev-tpa-empty"}
	resp, err := NewTPATool(&fakeClaimsPort{items: nil}, pin)(context.Background(), toolsTPAReq())
	if err != nil {
		t.Fatalf("empty list: %v", err)
	}
	if resp.Truncated {
		t.Fatalf("Truncated = true, want false for empty")
	}
	if len(resp.Claims) != 0 {
		t.Fatalf("claims = %d, want 0", len(resp.Claims))
	}
	if resp.Claims == nil {
		t.Fatalf("claims nil, want empty non-nil slice")
	}
	if resp.EvidenceID != "ev-tpa-empty" {
		t.Fatalf("EvidenceID = %q", resp.EvidenceID)
	}
}

func TestTPADeterministic(t *testing.T) {
	mk := func() (TPAResponse, error) {
		return NewTPATool(
			&fakeClaimsPort{items: validPriorClaims(toolsTenant)},
			&fakePinner{id: "ev-tpa-01"},
		)(context.Background(), toolsTPAReq())
	}
	a, err := mk()
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	b, err := mk()
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("non-deterministic:\n%#v\n%#v", a, b)
	}
}

func TestTPAErrorMapping(t *testing.T) {
	for _, sentinel := range []error{ports.ErrContract, ports.ErrUpstream, ports.ErrNotFound} {
		port := &fakeClaimsPort{err: toolsErr(sentinel, "tpa")}
		_, err := NewTPATool(port, &fakePinner{id: "ev-x"})(context.Background(), toolsTPAReq())
		requireToolsSentinel(t, err, sentinel)
	}
}
