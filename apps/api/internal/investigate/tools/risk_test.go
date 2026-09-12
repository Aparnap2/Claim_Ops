package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"claimops-api/internal/ports"
)

func validRiskRequest() RiskRequest {
	return RiskRequest{
		TenantID:        toolsTenant,
		ClaimID:         toolsClaim,
		InvestigationID: toolsInv,
		RequestID:       toolsReq,
		SubjectID:       "subj-54",
	}
}

func TestRiskValid(t *testing.T) {
	ctx := context.Background()
	port := &fakeRiskPort{items: validSignals(toolsTenant)}
	pin := &fakePinner{id: "ev-risk-01"}
	tool := NewRiskTool(port, pin)

	got, err := tool(ctx, validRiskRequest())
	if err != nil {
		t.Fatalf("valid risk call: %v", err)
	}
	if got.Truncated {
		t.Fatalf("Truncated must be false for short list")
	}
	if len(got.Signals) != 2 {
		t.Fatalf("Signals len = %d, want 2", len(got.Signals))
	}
	if got.Signals[0].SignalType != "RECENT_SIMILAR_CLAIM" || got.Signals[0].SourceRef != "src-r1" {
		t.Fatalf("signal[0] not verbatim: %+v", got.Signals[0])
	}
	if got.Signals[1].SignalType != "ROUND_AMOUNT" || got.Signals[1].SourceRef != "src-r2" {
		t.Fatalf("signal[1] not verbatim: %+v", got.Signals[1])
	}
	if got.EvidenceID != "ev-risk-01" {
		t.Fatalf("EvidenceID = %q, want ev-risk-01", got.EvidenceID)
	}
	if got.ContentHash == "" {
		t.Fatalf("ContentHash must be set")
	}
	if pin.calls != 1 {
		t.Fatalf("pinner calls = %d, want 1", pin.calls)
	}
	if pin.gotTenant != toolsTenant || pin.gotClaim != toolsClaim {
		t.Fatalf("pinner tenant/claim = %q/%q", pin.gotTenant, pin.gotClaim)
	}
	if pin.gotType != "risk" || pin.gotSource != "subj-54" {
		t.Fatalf("pinner type/source = %q/%q, want risk/subj-54", pin.gotType, pin.gotSource)
	}
}

func TestRiskPerItemDriftMismatch(t *testing.T) {
	ctx := context.Background()
	items := validSignals(toolsTenant)
	items = append(items, ports.RiskSignal{SignalType: "FOREIGN", TenantID: "tnt-other", Severity: "HIGH", SourceRef: "src-x"})
	tool := NewRiskTool(&fakeRiskPort{items: items}, &fakePinner{id: "ev-unused"})

	_, err := tool(ctx, validRiskRequest())
	if err == nil {
		t.Fatalf("want TenantMismatch on drifted item, got nil")
	}
	requireToolsSentinel(t, err, ports.ErrTenantMismatch)
}

func TestRiskBlankSignalContract(t *testing.T) {
	ctx := context.Background()
	// Blank signal type.
	tool := NewRiskTool(&fakeRiskPort{items: []ports.RiskSignal{
		{SignalType: "", TenantID: toolsTenant, Severity: "HIGH", SourceRef: "s"},
	}}, &fakePinner{id: "ev-unused"})
	if _, err := tool(ctx, validRiskRequest()); err == nil {
		t.Fatalf("want Contract for blank signal type, got nil")
	} else {
		requireToolsSentinel(t, err, ports.ErrContract)
	}

	// Blank severity.
	tool = NewRiskTool(&fakeRiskPort{items: []ports.RiskSignal{
		{SignalType: "ROUND_AMOUNT", TenantID: toolsTenant, Severity: "", SourceRef: "s"},
	}}, &fakePinner{id: "ev-unused"})
	if _, err := tool(ctx, validRiskRequest()); err == nil {
		t.Fatalf("want Contract for blank severity, got nil")
	} else {
		requireToolsSentinel(t, err, ports.ErrContract)
	}

	// Blank tenant marker.
	tool = NewRiskTool(&fakeRiskPort{items: []ports.RiskSignal{
		{SignalType: "ROUND_AMOUNT", TenantID: "", Severity: "LOW", SourceRef: "s"},
	}}, &fakePinner{id: "ev-unused"})
	if _, err := tool(ctx, validRiskRequest()); err == nil {
		t.Fatalf("want Contract for blank tenant marker, got nil")
	} else {
		requireToolsSentinel(t, err, ports.ErrContract)
	}
}

func TestRiskTruncateFlag(t *testing.T) {
	ctx := context.Background()
	var items []ports.RiskSignal
	for i := 0; i < MaxRiskRows+5; i++ {
		items = append(items, ports.RiskSignal{
			SignalType: string(rune('A'+i%26)) + "-sig",
			TenantID:   toolsTenant,
			Severity:   "LOW",
			SourceRef:  "src",
		})
	}
	// Unique signal types to preserve order check.
	for i := range items {
		items[i].SignalType = "SIG-" + string(rune('0'+i/26)) + string(rune('0'+i%26)) + "-" + string(rune('a'+i%26))
	}
	pin := &fakePinner{id: "ev-risk-cap"}
	tool := NewRiskTool(&fakeRiskPort{items: items}, pin)

	got, err := tool(ctx, validRiskRequest())
	if err != nil {
		t.Fatalf("cap call: %v", err)
	}
	if !got.Truncated {
		t.Fatalf("Truncated must be true when upstream exceeds cap")
	}
	if len(got.Signals) != MaxRiskRows {
		t.Fatalf("Signals len = %d, want cap %d", len(got.Signals), MaxRiskRows)
	}
	if len(got.Signals) > 20 {
		t.Fatalf("Signals len = %d, must be <= 20", len(got.Signals))
	}
	for i := 0; i < MaxRiskRows; i++ {
		if got.Signals[i].SignalType != items[i].SignalType {
			t.Fatalf("order drift at %d: got %q want %q", i, got.Signals[i].SignalType, items[i].SignalType)
		}
	}
	// Pinned bytes are the FULL pre-truncation list.
	var pinned []ports.RiskSignal
	if err := json.Unmarshal(pin.gotBytes, &pinned); err != nil {
		t.Fatalf("unmarshal pinned: %v", err)
	}
	if len(pinned) != len(items) {
		t.Fatalf("pinned len = %d, want full %d", len(pinned), len(items))
	}
}

func TestRiskSeverityVerbatim(t *testing.T) {
	ctx := context.Background()
	items := []ports.RiskSignal{
		{SignalType: "A", TenantID: toolsTenant, Severity: "high", SourceRef: "s1"},
		{SignalType: "B", TenantID: toolsTenant, Severity: "CRITICAL", SourceRef: "s2"},
		{SignalType: "C", TenantID: toolsTenant, Severity: "P3", SourceRef: "s3"},
	}
	tool := NewRiskTool(&fakeRiskPort{items: items}, &fakePinner{id: "ev-risk-sev"})

	got, err := tool(ctx, validRiskRequest())
	if err != nil {
		t.Fatalf("severity call: %v", err)
	}
	if len(got.Signals) != len(items) {
		t.Fatalf("Signals len = %d, want %d", len(got.Signals), len(items))
	}
	for i := range items {
		// String compare: verbatim, no case folding or invest-term mapping.
		if got.Signals[i].Severity != items[i].Severity {
			t.Fatalf("signal %d severity = %q, want verbatim %q", i, got.Signals[i].Severity, items[i].Severity)
		}
	}
	// Lowercase input must NOT be normalized to invest HIGH/MEDIUM/LOW terms.
	if got.Signals[0].Severity == "HIGH" {
		t.Fatalf("severity was mapped to HIGH invest term; must stay verbatim %q", items[0].Severity)
	}
	if got.Signals[0].Severity != "high" {
		t.Fatalf("severity = %q, want exact string %q", got.Signals[0].Severity, "high")
	}
}

func TestRiskDeterministic(t *testing.T) {
	ctx := context.Background()
	items := validSignals(toolsTenant)
	call := func() RiskResponse {
		tool := NewRiskTool(&fakeRiskPort{items: items}, &fakePinner{id: "ev-risk-det"})
		got, err := tool(ctx, validRiskRequest())
		if err != nil {
			t.Fatalf("deterministic call: %v", err)
		}
		return got
	}
	a, b := call(), call()
	if a.ContentHash != b.ContentHash {
		t.Fatalf("ContentHash not deterministic:\n a %s\n b %s", a.ContentHash, b.ContentHash)
	}
	rawA, _ := json.Marshal(a.Signals)
	rawB, _ := json.Marshal(b.Signals)
	if string(rawA) != string(rawB) {
		t.Fatalf("signals not deterministic")
	}
	// Hash must be sha256 of the full upstream canonical bytes.
	canonical, _ := json.Marshal(items)
	sum := sha256.Sum256(canonical)
	if a.ContentHash != hex.EncodeToString(sum[:]) {
		t.Fatalf("ContentHash = %q, want sha256 of canonical", a.ContentHash)
	}
}
