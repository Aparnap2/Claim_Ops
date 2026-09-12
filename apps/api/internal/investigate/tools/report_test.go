package tools

import (
	"context"
	"testing"

	"claimops-api/internal/invest"
	"claimops-api/internal/ports"
)

func validReportSetup() (ReportRequest, *fakeReportStore, Resolver) {
	exc := validReportException()
	req := validReportRequest(exc)
	store := newFakeReportStore()
	resolve := staticResolver(map[string][2]string{
		"ev-01": {toolsTenant, toolsClaim},
	})
	return req, store, resolve
}

func TestReportValidStore(t *testing.T) {
	ctx := context.Background()
	req, store, resolve := validReportSetup()

	got, err := NewReportTool(store, resolve)(ctx, req)
	if err != nil {
		t.Fatalf("valid report call: %v", err)
	}
	if got.Replayed {
		t.Fatalf("first insert must have replayed=false")
	}
	if got.ReportID != toolsInv {
		t.Fatalf("ReportID = %q, want %q", got.ReportID, toolsInv)
	}
	if got.Version != 1 {
		t.Fatalf("Version = %d, want 1", got.Version)
	}
	if got.ContentHash == "" {
		t.Fatalf("ContentHash must be set")
	}
	row, ok := store.rows[toolsInv]
	if !ok {
		t.Fatalf("store missing row %q", toolsInv)
	}
	if row.ID != toolsInv || row.InvestigationID != toolsInv {
		t.Fatalf("row identity = %q/%q, want %q", row.ID, row.InvestigationID, toolsInv)
	}
	if row.ContentHash != got.ContentHash {
		t.Fatalf("row hash = %q, want %q", row.ContentHash, got.ContentHash)
	}
}

func TestReportDuplicateReplay(t *testing.T) {
	ctx := context.Background()
	req, store, resolve := validReportSetup()
	tool := NewReportTool(store, resolve)

	first, err := tool(ctx, req)
	if err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if first.Replayed {
		t.Fatalf("first insert must have replayed=false")
	}
	second, err := tool(ctx, req)
	if err != nil {
		t.Fatalf("duplicate insert: %v", err)
	}
	if !second.Replayed {
		t.Fatalf("duplicate insert must have replayed=true")
	}
	if second.ReportID != first.ReportID || second.ContentHash != first.ContentHash {
		t.Fatalf("replay identity drift: %+v vs %+v", second, first)
	}
	if len(store.rows) != 1 {
		t.Fatalf("store rows = %d, want 1 (write-once)", len(store.rows))
	}
}

func TestReportUnknownEvidenceContract(t *testing.T) {
	ctx := context.Background()
	req, store, _ := validReportSetup()
	// Empty resolver: ev-01 is unknown.
	resolve := staticResolver(map[string][2]string{})

	_, err := NewReportTool(store, resolve)(ctx, req)
	if err == nil {
		t.Fatalf("want Contract for unknown evidence, got nil")
	}
	requireToolsSentinel(t, err, ports.ErrContract)
}

func TestReportAdditiveViolation(t *testing.T) {
	ctx := context.Background()
	exc := validReportException()
	exc.MissingEvidence = []invest.MissingItem{
		{Kind: invest.MissingField, Key: "bill_lines", Detail: "line amounts unavailable"},
		{Kind: invest.MissingField, Key: "extra_item", Detail: "extra detail"},
	}
	req := validReportRequest(exc)
	// req.MissingEvidence carries only bill_lines, dropping extra_item.
	store := newFakeReportStore()
	resolve := staticResolver(map[string][2]string{
		"ev-01": {toolsTenant, toolsClaim},
	})

	_, err := NewReportTool(store, resolve)(ctx, req)
	if err == nil {
		t.Fatalf("want Contract for dropped baseline item, got nil")
	}
	requireToolsSentinel(t, err, ports.ErrContract)
}

func TestReportFindingDanglingContract(t *testing.T) {
	ctx := context.Background()
	req, store, resolve := validReportSetup()
	req.Findings[0].HypothesisID = "hyp-dangling"

	_, err := NewReportTool(store, resolve)(ctx, req)
	if err == nil {
		t.Fatalf("want Contract for dangling finding->hypothesis, got nil")
	}
	requireToolsSentinel(t, err, ports.ErrContract)
}

func TestReportRecommendationDanglingContract(t *testing.T) {
	ctx := context.Background()
	req, store, resolve := validReportSetup()
	req.Recommendations[0].FindingIDs = []string{"find-dangling"}

	_, err := NewReportTool(store, resolve)(ctx, req)
	if err == nil {
		t.Fatalf("want Contract for dangling recommendation->finding, got nil")
	}
	requireToolsSentinel(t, err, ports.ErrContract)
}

func TestReportDeterministic(t *testing.T) {
	ctx := context.Background()
	req, _, resolve := validReportSetup()

	storeA := newFakeReportStore()
	a, err := NewReportTool(storeA, resolve)(ctx, req)
	if err != nil {
		t.Fatalf("first store call: %v", err)
	}
	storeB := newFakeReportStore()
	b, err := NewReportTool(storeB, resolve)(ctx, req)
	if err != nil {
		t.Fatalf("second store call: %v", err)
	}
	if a.ContentHash != b.ContentHash {
		t.Fatalf("ContentHash not deterministic:\n a %s\n b %s", a.ContentHash, b.ContentHash)
	}
	if a.ReportID != b.ReportID || a.Version != b.Version {
		t.Fatalf("receipt not deterministic: %+v vs %+v", a, b)
	}
	// Replay on the same store is hash-stable too.
	c, err := NewReportTool(storeA, resolve)(ctx, req)
	if err != nil {
		t.Fatalf("replay call: %v", err)
	}
	if !c.Replayed {
		t.Fatalf("repeat insert must replay")
	}
	if c.ContentHash != a.ContentHash {
		t.Fatalf("replay hash drift: %q vs %q", c.ContentHash, a.ContentHash)
	}
}
