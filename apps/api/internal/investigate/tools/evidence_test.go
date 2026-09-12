package tools

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"claimops-api/internal/invest"
	"claimops-api/internal/investigate"
	"claimops-api/internal/ports"
)

func toolsEvidenceReq(limit int, cursor, source string) investigate.Request {
	return investigate.Request{
		Tool:            invest.ToolGetEvidence,
		TenantID:        toolsTenant,
		ClaimID:         toolsClaim,
		InvestigationID: toolsInv,
		RequestID:       toolsReq,
		Limit:           limit,
		Cursor:          cursor,
		SourceType:      source,
	}
}

func TestEvidenceValid(t *testing.T) {
	rows := []investigate.EvidenceRow{
		{EvidenceID: "ev-01", SourceType: "document", SourceID: "doc-54-1", ContentHash: "h1"},
		{EvidenceID: "ev-02", SourceType: "field", SourceID: "doc-54-1", ContentHash: ""},
	}
	r := &fakeEvidenceReader{page: investigate.EvidencePage{Rows: rows}}
	tool := NewEvidenceTool(r)
	resp, err := tool(context.Background(), toolsEvidenceReq(10, "", ""))
	if err != nil {
		t.Fatalf("valid list: %v", err)
	}
	if resp.RowCount != len(rows) {
		t.Fatalf("RowCount = %d, want %d", resp.RowCount, len(rows))
	}
	if !reflect.DeepEqual(resp.IDs, []string{"ev-01", "ev-02"}) {
		t.Fatalf("IDs = %v", resp.IDs)
	}
	if r.got.tenant != toolsTenant || r.got.claim != toolsClaim {
		t.Fatalf("reader echo tenant=%q claim=%q", r.got.tenant, r.got.claim)
	}
	if r.got.limit != 10 {
		t.Fatalf("reader limit = %d, want 10", r.got.limit)
	}
}

func TestEvidenceInvalidSourceType(t *testing.T) {
	r := &fakeEvidenceReader{}
	tool := NewEvidenceTool(r)
	_, err := tool(context.Background(), toolsEvidenceReq(10, "", "bogus"))
	requireToolsSentinel(t, err, ports.ErrContract)
}

func TestEvidenceInvalidTenant(t *testing.T) {
	r := &fakeEvidenceReader{}
	tool := NewEvidenceTool(r)
	req := toolsEvidenceReq(10, "", "")
	req.TenantID = "  "
	_, err := tool(context.Background(), req)
	if !errors.Is(err, ports.ErrContract) {
		t.Fatalf("want ErrContract, got %v", err)
	}
}

func TestEvidenceTenantMismatchPropagates(t *testing.T) {
	r := &fakeEvidenceReader{err: toolsErr(ports.ErrTenantMismatch, "drift")}
	tool := NewEvidenceTool(r)
	_, err := tool(context.Background(), toolsEvidenceReq(10, "", ""))
	requireToolsSentinel(t, err, ports.ErrTenantMismatch)
}

func TestEvidenceBoundsClamp(t *testing.T) {
	// Bounds are enforced fail-closed at the generic envelope: a limit
	// outside 1..MaxRows never reaches the reader.
	for _, limit := range []int{0, -1, 9999} {
		_, err := NewEvidenceTool(&fakeEvidenceReader{})(context.Background(), toolsEvidenceReq(limit, "", ""))
		requireToolsSentinel(t, err, ports.ErrContract)
	}
	// Boundary limits pass through verbatim.
	for _, limit := range []int{1, 50} {
		r := &fakeEvidenceReader{}
		if _, err := NewEvidenceTool(r)(context.Background(), toolsEvidenceReq(limit, "", "")); err != nil {
			t.Fatalf("limit %d: %v", limit, err)
		}
		if r.got.limit != limit {
			t.Fatalf("reader limit = %d, want %d", r.got.limit, limit)
		}
	}
	// The tool-level validate clamps defense-in-depth (direct use,
	// bypassing the envelope): <=0 defaults, over-cap clamps.
	lo := EvidenceListRequest{TenantID: toolsTenant, ClaimID: toolsClaim, InvestigationID: toolsInv, RequestID: toolsReq}
	if err := lo.validate(); err != nil {
		t.Fatalf("zero limit validate: %v", err)
	}
	if lo.Limit != investigate.MaxRowsGetEvidence {
		t.Fatalf("default limit = %d, want %d", lo.Limit, investigate.MaxRowsGetEvidence)
	}
	hi := EvidenceListRequest{TenantID: toolsTenant, ClaimID: toolsClaim, InvestigationID: toolsInv, RequestID: toolsReq, Limit: 9999}
	if err := hi.validate(); err != nil {
		t.Fatalf("over-cap validate: %v", err)
	}
	if hi.Limit != investigate.MaxRowsGetEvidence {
		t.Fatalf("clamped limit = %d, want %d", hi.Limit, investigate.MaxRowsGetEvidence)
	}
}

func TestEvidenceEmpty(t *testing.T) {
	r := &fakeEvidenceReader{page: investigate.EvidencePage{}}
	tool := NewEvidenceTool(r)
	resp, err := tool(context.Background(), toolsEvidenceReq(10, "", ""))
	if err != nil {
		t.Fatalf("empty page: %v", err)
	}
	if resp.RowCount != 0 {
		t.Fatalf("RowCount = %d, want 0", resp.RowCount)
	}
}

func TestEvidenceDeterministic(t *testing.T) {
	rows := []investigate.EvidenceRow{
		{EvidenceID: "ev-01", SourceType: "document", SourceID: "doc-54-1", ContentHash: "h1"},
	}
	mk := func() (investigate.Response, error) {
		r := &fakeEvidenceReader{page: investigate.EvidencePage{Rows: rows}}
		return NewEvidenceTool(r)(context.Background(), toolsEvidenceReq(10, "", ""))
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

// ---------------------------------------------------------------------------
// T5: search_evidence
// ---------------------------------------------------------------------------

func TestSearchWildcardOnlyRejected(t *testing.T) {
	for _, q := range []string{"%", "%%", "___", "***", `%_\*`, "  %  "} {
		req := investigate.Request{
			Tool:            invest.ToolSearchEvidence,
			TenantID:        toolsTenant,
			ClaimID:         toolsClaim,
			InvestigationID: toolsInv,
			RequestID:       toolsReq,
			Query:           q,
			Limit:           10,
		}
		_, err := NewSearchEvidenceTool(&fakeEvidenceReader{})(context.Background(), req)
		requireToolsSentinel(t, err, ports.ErrContract)
	}
}

func TestSearchOverlongRejected(t *testing.T) {
	q := strings.Repeat("a", invest.SearchMaxQueryLen+1)
	req := investigate.Request{
		Tool:            invest.ToolSearchEvidence,
		TenantID:        toolsTenant,
		ClaimID:         toolsClaim,
		InvestigationID: toolsInv,
		RequestID:       toolsReq,
		Query:           q,
		Limit:           10,
	}
	_, err := NewSearchEvidenceTool(&fakeEvidenceReader{})(context.Background(), req)
	requireToolsSentinel(t, err, ports.ErrContract)
}

func TestSearchBlankRejected(t *testing.T) {
	for _, q := range []string{"", "   ", " bill", "bill "} {
		req := investigate.Request{
			Tool:            invest.ToolSearchEvidence,
			TenantID:        toolsTenant,
			ClaimID:         toolsClaim,
			InvestigationID: toolsInv,
			RequestID:       toolsReq,
			Query:           q,
			Limit:           10,
		}
		_, err := NewSearchEvidenceTool(&fakeEvidenceReader{})(context.Background(), req)
		requireToolsSentinel(t, err, ports.ErrContract)
	}
}

func TestSearchSnippetTruncation(t *testing.T) {
	long := strings.Repeat("s", investigate.MaxSnippetRunes+100)
	raw := []investigate.EvidenceHit{
		{EvidenceID: "ev-01", FieldKey: "total", Anchor: "a1", Snippet: long},
	}
	out, err := NewSearchResponse(raw)
	if err != nil {
		t.Fatalf("NewSearchResponse: %v", err)
	}
	if got := len([]rune(out.Hits[0].Snippet)); got != investigate.MaxSnippetRunes {
		t.Fatalf("snippet runes = %d, want %d", got, investigate.MaxSnippetRunes)
	}
	if want := string([]rune(long)[:investigate.MaxSnippetRunes]); out.Hits[0].Snippet != want {
		t.Fatalf("snippet not rune-prefix truncated")
	}
}

func TestSearchValid(t *testing.T) {
	r := &fakeEvidenceReader{hits: []investigate.EvidenceHit{
		{EvidenceID: "ev-01", FieldKey: "total", Anchor: "a1", Snippet: "sum 120"},
	}}
	req := investigate.Request{
		Tool:            invest.ToolSearchEvidence,
		TenantID:        toolsTenant,
		ClaimID:         toolsClaim,
		InvestigationID: toolsInv,
		RequestID:       toolsReq,
		Query:           "total",
		Limit:           10,
	}
	resp, err := NewSearchEvidenceTool(r)(context.Background(), req)
	if err != nil {
		t.Fatalf("valid search: %v", err)
	}
	if r.got.query != "total" {
		t.Fatalf("reader query = %q, want %q", r.got.query, "total")
	}
	if resp.RowCount != 1 {
		t.Fatalf("RowCount = %d, want 1", resp.RowCount)
	}
}
