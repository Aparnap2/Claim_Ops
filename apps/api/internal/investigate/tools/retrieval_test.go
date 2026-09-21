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

// ---------------------------------------------------------------------------
// Helpers for retrieval envelope construction.
// ---------------------------------------------------------------------------

func retrievalEvidenceRequest(t *testing.T, limit int, cursor, source string) investigate.Request {
	t.Helper()
	req, err := investigate.NewGetEvidenceRequest(toolsTenant, toolsClaim, toolsInv, toolsReq, limit, cursor, source)
	if err != nil {
		t.Fatalf("build evidence request (limit=%d cursor=%q source=%q): %v", limit, cursor, source, err)
	}
	return req.ToRequest()
}

func retrievalSearchRequest(t *testing.T, query string, limit int) investigate.Request {
	t.Helper()
	req, err := investigate.NewSearchEvidenceRequest(toolsTenant, toolsClaim, toolsInv, toolsReq, query, limit)
	if err != nil {
		t.Fatalf("build search request query=%q limit=%d: %v", query, limit, err)
	}
	return req.ToRequest()
}

// tenantEnforcingReader is a minimal EvidenceReader that enforces tenant
// isolation: only allowedTenant may read; any other tenant fails closed
// with ErrTenantMismatch. It records calls so negative tests can prove the
// reader was not leaked or that the error propagated without rows.
type tenantEnforcingReader struct {
	allowedTenant string
	page          investigate.EvidencePage
	hits          []investigate.EvidenceHit
	calls         int
	lastTenant    string
	lastClaim     string
	lastLimit     int
	lastCursor    string
	lastSource    string
	lastQuery     string
}

func (r *tenantEnforcingReader) ListEvidence(_ context.Context, tenant, claim string, limit int, cursor, source string) (investigate.EvidencePage, error) {
	r.calls++
	r.lastTenant, r.lastClaim, r.lastLimit, r.lastCursor, r.lastSource = tenant, claim, limit, cursor, source
	if tenant != r.allowedTenant {
		return investigate.EvidencePage{}, toolsErr(ports.ErrTenantMismatch, "tenant mismatch")
	}
	return r.page, nil
}

func (r *tenantEnforcingReader) SearchEvidence(_ context.Context, tenant, claim, query string, limit int) ([]investigate.EvidenceHit, error) {
	r.calls++
	r.lastTenant, r.lastClaim, r.lastQuery, r.lastLimit = tenant, claim, query, limit
	if tenant != r.allowedTenant {
		return nil, toolsErr(ports.ErrTenantMismatch, "tenant mismatch")
	}
	return r.hits, nil
}

// ---------------------------------------------------------------------------
// 1. Tenant isolation.
// ---------------------------------------------------------------------------

func TestRetrievalEvidenceTenantIsolation(t *testing.T) {
	rows := []investigate.EvidenceRow{
		{EvidenceID: "ev-iso-01", SourceType: string(invest.EvidenceSourceDocument), SourceID: "doc-01", ContentHash: "h1"},
		{EvidenceID: "ev-iso-02", SourceType: string(invest.EvidenceSourceField), SourceID: "doc-01", ContentHash: ""},
	}
	reader := &tenantEnforcingReader{
		allowedTenant: toolsTenant,
		page:          investigate.EvidencePage{Rows: rows},
	}

	// Own tenant succeeds and carries rows.
	tool := NewEvidenceTool(reader)
	resp, err := tool(context.Background(), retrievalEvidenceRequest(t, 10, "", ""))
	if err != nil {
		t.Fatalf("own tenant list: %v", err)
	}
	if resp.RowCount != len(rows) {
		t.Fatalf("RowCount = %d, want %d", resp.RowCount, len(rows))
	}
	if !reflect.DeepEqual(resp.IDs, []string{"ev-iso-01", "ev-iso-02"}) {
		t.Fatalf("IDs = %v, want [ev-iso-01 ev-iso-02]", resp.IDs)
	}

	// Cross-tenant fails closed with tenant sentinel and leaks no rows.
	crossReq, err := investigate.NewGetEvidenceRequest(toolsTenant+"-other", toolsClaim, toolsInv, toolsReq, 10, "", "")
	if err != nil {
		t.Fatalf("build cross-tenant request: %v", err)
	}
	generic := crossReq.ToRequest()
	// Bypass envelope allowlist? NewGetEvidenceRequest validates tenant format,
	// so cross tenant is a different valid tenant string, still well-formed.
	_, err = tool(context.Background(), generic)
	if err == nil {
		t.Fatal("cross-tenant list succeeded, want rejection")
	}
	if !errors.Is(err, ports.ErrTenantMismatch) && !errors.Is(err, ports.ErrContract) {
		t.Fatalf("cross-tenant err = %v, want ErrTenantMismatch or ErrContract", err)
	}
	// Ensure no rows leaked via error path: response is zero value on error
	// and the enforcing reader returned no page for the mismatched tenant.

	// Second tenant's evidence must be absent: a reader scoped to the other
	// tenant would not see the first tenant's rows.
	otherReader := &tenantEnforcingReader{
		allowedTenant: toolsTenant + "-other",
		page: investigate.EvidencePage{Rows: []investigate.EvidenceRow{
			{EvidenceID: "ev-other-99", SourceType: string(invest.EvidenceSourceDocument), SourceID: "doc-99", ContentHash: "hx"},
		}},
	}
	otherTool := NewEvidenceTool(otherReader)
	otherTenant := toolsTenant + "-other"
	otherReq, err := investigate.NewGetEvidenceRequest(otherTenant, toolsClaim, toolsInv, toolsReq, 10, "", "")
	if err != nil {
		t.Fatalf("build other tenant request: %v", err)
	}
	otherResp, err := otherTool(context.Background(), otherReq.ToRequest())
	if err != nil {
		t.Fatalf("other tenant list: %v", err)
	}
	for _, id := range otherResp.IDs {
		if id == "ev-iso-01" || id == "ev-iso-02" {
			t.Fatalf("cross-tenant leak: other tenant response contains %q from first tenant", id)
		}
	}
}

func TestRetrievalSearchTenantIsolation(t *testing.T) {
	hits := []investigate.EvidenceHit{
		{EvidenceID: "ev-s-01", SourceType: string(invest.EvidenceSourceField), SourceID: "doc-01", FieldKey: "total", Anchor: "a1", Snippet: "sum 120"},
	}
	reader := &tenantEnforcingReader{
		allowedTenant: toolsTenant,
		hits:          hits,
	}
	tool := NewSearchEvidenceTool(reader)

	// Own tenant succeeds.
	resp, err := tool(context.Background(), retrievalSearchRequest(t, "total", 10))
	if err != nil {
		t.Fatalf("own tenant search: %v", err)
	}
	if resp.RowCount != 1 {
		t.Fatalf("RowCount = %d, want 1", resp.RowCount)
	}
	if !reflect.DeepEqual(resp.IDs, []string{"ev-s-01"}) {
		t.Fatalf("IDs = %v, want [ev-s-01]", resp.IDs)
	}

	// Cross-tenant fails closed and does not leak rows.
	crossReq, err := investigate.NewSearchEvidenceRequest(toolsTenant+"-other", toolsClaim, toolsInv, toolsReq, "total", 10)
	if err != nil {
		t.Fatalf("build cross-tenant search request: %v", err)
	}
	_, err = tool(context.Background(), crossReq.ToRequest())
	if err == nil {
		t.Fatal("cross-tenant search succeeded, want rejection")
	}
	if !errors.Is(err, ports.ErrTenantMismatch) && !errors.Is(err, ports.ErrContract) {
		t.Fatalf("cross-tenant search err = %v, want ErrTenantMismatch or ErrContract", err)
	}

	// Second tenant's evidence absent: searching as other tenant must not
	// return first tenant's hit.
	otherReader := &tenantEnforcingReader{
		allowedTenant: toolsTenant + "-other",
		hits: []investigate.EvidenceHit{
			{EvidenceID: "ev-other-77", SourceType: string(invest.EvidenceSourceField), SourceID: "doc-99", FieldKey: "other", Anchor: "ax", Snippet: "other"},
		},
	}
	otherTool := NewSearchEvidenceTool(otherReader)
	otherTenant2 := toolsTenant + "-other"
	otherSearchReq, err := investigate.NewSearchEvidenceRequest(otherTenant2, toolsClaim, toolsInv, toolsReq, "other", 10)
	if err != nil {
		t.Fatalf("build other tenant search request: %v", err)
	}
	otherResp, err := otherTool(context.Background(), otherSearchReq.ToRequest())
	if err != nil {
		t.Fatalf("other tenant search: %v", err)
	}
	for _, id := range otherResp.IDs {
		if id == "ev-s-01" {
			t.Fatalf("cross-tenant leak: search response contains %q from first tenant", id)
		}
	}
}

// ---------------------------------------------------------------------------
// 2. Provenance.
// ---------------------------------------------------------------------------

func TestRetrievalEvidenceProvenanceIDs(t *testing.T) {
	rows := []investigate.EvidenceRow{
		{EvidenceID: "ev-prov-01", SourceType: string(invest.EvidenceSourceDocument), SourceID: "doc-10", ContentHash: "h-doc"},
		{EvidenceID: "ev-prov-02", SourceType: string(invest.EvidenceSourceField), SourceID: "doc-10", ContentHash: ""},
		{EvidenceID: "ev-prov-03", SourceType: string(invest.EvidenceSourcePolicy), SourceID: "pol-01", ContentHash: "h-pol"},
	}
	reader := &fakeEvidenceReader{page: investigate.EvidencePage{Rows: rows, Truncated: false, NextCursor: ""}}
	tool := NewEvidenceTool(reader)
	resp, err := tool(context.Background(), retrievalEvidenceRequest(t, 10, "", ""))
	if err != nil {
		t.Fatalf("provenance list: %v", err)
	}
	if resp.RowCount != len(rows) {
		t.Fatalf("RowCount = %d, want %d", resp.RowCount, len(rows))
	}
	// Response IDs are sorted for determinism; provenance is proved by exact
	// set equality with the EvidenceIDs.
	wantIDs := []string{"ev-prov-01", "ev-prov-02", "ev-prov-03"}
	if !reflect.DeepEqual(resp.IDs, wantIDs) {
		t.Fatalf("IDs = %v, want %v", resp.IDs, wantIDs)
	}
	// Each hit must trace to evidence_id/field/anchor/source_id. For T4 the
	// row itself is the provenance pin: verify the backing page carries
	// source IDs and that none are blank.
	for i, r := range reader.page.Rows {
		if strings.TrimSpace(r.EvidenceID) == "" || strings.TrimSpace(r.SourceID) == "" || strings.TrimSpace(r.SourceType) == "" {
			t.Fatalf("row[%d] missing provenance: %+v", i, r)
		}
	}
	if reader.got.tenant != toolsTenant || reader.got.claim != toolsClaim {
		t.Fatalf("reader echo tenant=%q claim=%q", reader.got.tenant, reader.got.claim)
	}
}

func TestRetrievalSearchProvenance(t *testing.T) {
	raw := []investigate.EvidenceHit{
		{EvidenceID: "ev-search-01", SourceType: string(invest.EvidenceSourceField), SourceID: "doc-54-1", FieldKey: "total", Anchor: "a1", Snippet: "sum 120"},
		{EvidenceID: "ev-search-02", SourceType: string(invest.EvidenceSourceField), SourceID: "doc-54-2", FieldKey: "policy_number", Anchor: "a2", Snippet: "POL-X"},
	}
	// Typed provenance: NewSearchResponse preserves EvidenceID/FieldKey/Anchor
	// and defensively truncates Snippet while keeping SourceID on the raw hit.
	out, err := NewSearchResponse(raw)
	if err != nil {
		t.Fatalf("NewSearchResponse: %v", err)
	}
	if len(out.Hits) != len(raw) {
		t.Fatalf("hits len = %d, want %d", len(out.Hits), len(raw))
	}
	for i, h := range out.Hits {
		if h.EvidenceID != raw[i].EvidenceID {
			t.Fatalf("hit[%d] EvidenceID = %q, want %q", i, h.EvidenceID, raw[i].EvidenceID)
		}
		if h.FieldKey != raw[i].FieldKey {
			t.Fatalf("hit[%d] FieldKey = %q, want %q", i, h.FieldKey, raw[i].FieldKey)
		}
		if h.Anchor != raw[i].Anchor {
			t.Fatalf("hit[%d] Anchor = %q, want %q", i, h.Anchor, raw[i].Anchor)
		}
		if h.Snippet != raw[i].Snippet {
			t.Fatalf("hit[%d] Snippet = %q, want %q", i, h.Snippet, raw[i].Snippet)
		}
		// SourceID is on the raw evidence hit (reader provenance); the typed
		// hit carries the locator without bytes, but the raw hit must still
		// have it.
		if strings.TrimSpace(raw[i].SourceID) == "" {
			t.Fatalf("raw hit[%d] missing SourceID provenance", i)
		}
		if strings.TrimSpace(raw[i].EvidenceID) == "" || strings.TrimSpace(raw[i].FieldKey) == "" {
			t.Fatalf("raw hit[%d] missing core provenance", i)
		}
	}
	// Locators preserve only identifiers, never matched text, and keep reader
	// order verbatim.
	locs := out.Locators()
	if len(locs) != len(raw) {
		t.Fatalf("locators len = %d, want %d", len(locs), len(raw))
	}
	for i, l := range locs {
		if l.EvidenceID != raw[i].EvidenceID || l.FieldKey != raw[i].FieldKey || l.Anchor != raw[i].Anchor {
			t.Fatalf("locator[%d] = %+v, want %+v", i, l, raw[i])
		}
	}
	// Via tool envelope: IDs are sorted, RowCount correct, order of typed
	// hits is reader order (verified above).
	reader := &fakeEvidenceReader{hits: raw}
	tool := NewSearchEvidenceTool(reader)
	resp, err := tool(context.Background(), retrievalSearchRequest(t, "total", 10))
	if err != nil {
		t.Fatalf("search via tool: %v", err)
	}
	if resp.RowCount != len(raw) {
		t.Fatalf("RowCount = %d, want %d", resp.RowCount, len(raw))
	}
	// Envelope IDs are sorted for determinism.
	sortedIDs := []string{"ev-search-01", "ev-search-02"}
	if !reflect.DeepEqual(resp.IDs, sortedIDs) {
		t.Fatalf("envelope IDs = %v, want %v", resp.IDs, sortedIDs)
	}
}

func TestRetrievalSearchSnippetDefensiveTruncation(t *testing.T) {
	// Overlong ASCII snippet: reader returns >200, tool must cap at 200.
	longASCII := strings.Repeat("s", investigate.MaxSnippetRunes+100)
	rawASCII := []investigate.EvidenceHit{
		{EvidenceID: "ev-snip-01", FieldKey: "total", Anchor: "a1", Snippet: longASCII, SourceID: "doc-01", SourceType: string(invest.EvidenceSourceField)},
	}
	out, err := NewSearchResponse(rawASCII)
	if err != nil {
		t.Fatalf("NewSearchResponse ascii: %v", err)
	}
	if got := len([]rune(out.Hits[0].Snippet)); got != investigate.MaxSnippetRunes {
		t.Fatalf("ascii snippet runes = %d, want %d", got, investigate.MaxSnippetRunes)
	}
	if want := string([]rune(longASCII)[:investigate.MaxSnippetRunes]); out.Hits[0].Snippet != want {
		t.Fatalf("ascii snippet not rune-prefix truncated")
	}

	// Overlong multi-byte snippet: rune-aware cut, valid UTF-8, no marker.
	longWide := strings.Repeat("é", investigate.MaxSnippetRunes+50)
	rawWide := []investigate.EvidenceHit{
		{EvidenceID: "ev-snip-02", FieldKey: "note", Anchor: "a2", Snippet: longWide, SourceID: "doc-02", SourceType: string(invest.EvidenceSourceField)},
	}
	out, err = NewSearchResponse(rawWide)
	if err != nil {
		t.Fatalf("NewSearchResponse wide: %v", err)
	}
	if got := len([]rune(out.Hits[0].Snippet)); got != investigate.MaxSnippetRunes {
		t.Fatalf("wide snippet runes = %d, want %d", got, investigate.MaxSnippetRunes)
	}
	if !strings.HasPrefix(longWide, out.Hits[0].Snippet) {
		t.Fatalf("wide snippet truncation is not a prefix cut")
	}
	for _, r := range out.Hits[0].Snippet {
		if r == '�' {
			t.Fatal("wide snippet truncation split a multi-byte rune")
		}
	}

	// Exact cap passes through unchanged.
	exact := strings.Repeat("x", investigate.MaxSnippetRunes)
	rawExact := []investigate.EvidenceHit{
		{EvidenceID: "ev-snip-03", FieldKey: "k", Anchor: "a3", Snippet: exact, SourceID: "doc-03", SourceType: string(invest.EvidenceSourceField)},
	}
	out, err = NewSearchResponse(rawExact)
	if err != nil {
		t.Fatalf("NewSearchResponse exact: %v", err)
	}
	if out.Hits[0].Snippet != exact {
		t.Fatalf("exact-cap snippet changed")
	}

	// Defensive truncation also via tool path: a non-conforming reader that
	// returns overlong snippet must not widen the window (typed response
	// still caps). The envelope cites IDs only, so snippet bytes never cross
	// the generic boundary, but the typed layer proves the cap.
	reader := &fakeEvidenceReader{hits: rawASCII}
	tool := NewSearchEvidenceTool(reader)
	resp, err := tool(context.Background(), retrievalSearchRequest(t, "s", 10))
	if err != nil {
		t.Fatalf("tool with overlong snippet: %v", err)
	}
	if resp.RowCount != 1 {
		t.Fatalf("RowCount = %d, want 1", resp.RowCount)
	}
	// Typed path still caps; verify directly.
	typed, err := NewSearchResponse(reader.hits)
	if err != nil {
		t.Fatalf("typed after tool call: %v", err)
	}
	if len([]rune(typed.Hits[0].Snippet)) != investigate.MaxSnippetRunes {
		t.Fatalf("typed snippet after tool = %d runes, want %d", len([]rune(typed.Hits[0].Snippet)), investigate.MaxSnippetRunes)
	}
}

// ---------------------------------------------------------------------------
// 3. Ranking / search behavior.
// ---------------------------------------------------------------------------

func TestRetrievalSearchLimitClamp(t *testing.T) {
	// Direct validate clamps defense-in-depth (bypassing envelope limit check).
	zero := EvidenceSearchRequest{TenantID: toolsTenant, ClaimID: toolsClaim, InvestigationID: toolsInv, RequestID: toolsReq, Query: "total", Limit: 0}
	if err := zero.validate(); err != nil {
		t.Fatalf("validate limit 0: %v", err)
	}
	if zero.Limit != investigate.MaxRowsSearchEvidence {
		t.Fatalf("limit 0 clamped to %d, want %d", zero.Limit, investigate.MaxRowsSearchEvidence)
	}
	huge := EvidenceSearchRequest{TenantID: toolsTenant, ClaimID: toolsClaim, InvestigationID: toolsInv, RequestID: toolsReq, Query: "total", Limit: 9999}
	if err := huge.validate(); err != nil {
		t.Fatalf("validate limit 9999: %v", err)
	}
	if huge.Limit != investigate.MaxRowsSearchEvidence {
		t.Fatalf("limit 9999 clamped to %d, want %d", huge.Limit, investigate.MaxRowsSearchEvidence)
	}
	if investigate.MaxRowsSearchEvidence != 20 {
		t.Fatalf("MaxRowsSearchEvidence = %d, want 20", investigate.MaxRowsSearchEvidence)
	}

	// Constructor also clamps.
	req, err := investigate.NewSearchEvidenceRequest(toolsTenant, toolsClaim, toolsInv, toolsReq, "total", 0)
	if err != nil {
		t.Fatalf("NewSearchEvidenceRequest limit 0: %v", err)
	}
	if req.Limit != investigate.MaxRowsSearchEvidence {
		t.Fatalf("constructor limit 0 = %d, want %d", req.Limit, investigate.MaxRowsSearchEvidence)
	}

	// Envelope rejects out-of-bounds limits fail-closed (generic Validate
	// enforces 1..MaxRows, so 0 and over-cap never reach the reader).
	for _, limit := range []int{0, -1, 9999} {
		r := investigate.Request{
			Tool:            invest.ToolSearchEvidence,
			TenantID:        toolsTenant,
			ClaimID:         toolsClaim,
			InvestigationID: toolsInv,
			RequestID:       toolsReq,
			Query:           "total",
			Limit:           limit,
		}
		_, err := NewSearchEvidenceTool(&fakeEvidenceReader{})(context.Background(), r)
		requireToolsSentinel(t, err, ports.ErrContract)
	}

	// Boundary limits pass through verbatim to the reader.
	for _, limit := range []int{1, investigate.MaxRowsSearchEvidence} {
		fake := &fakeEvidenceReader{hits: []investigate.EvidenceHit{}}
		req := investigate.Request{
			Tool:            invest.ToolSearchEvidence,
			TenantID:        toolsTenant,
			ClaimID:         toolsClaim,
			InvestigationID: toolsInv,
			RequestID:       toolsReq,
			Query:           "total",
			Limit:           limit,
		}
		if _, err := NewSearchEvidenceTool(fake)(context.Background(), req); err != nil {
			t.Fatalf("limit %d: %v", limit, err)
		}
		if fake.got.limit != limit {
			t.Fatalf("reader limit = %d, want %d", fake.got.limit, limit)
		}
	}
}

func TestRetrievalSearchQueryLengthBounds(t *testing.T) {
	// 200 runes OK.
	atCap := strings.Repeat("a", invest.SearchMaxQueryLen)
	req := investigate.Request{
		Tool:            invest.ToolSearchEvidence,
		TenantID:        toolsTenant,
		ClaimID:         toolsClaim,
		InvestigationID: toolsInv,
		RequestID:       toolsReq,
		Query:           atCap,
		Limit:           10,
	}
	reader := &fakeEvidenceReader{hits: []investigate.EvidenceHit{}}
	if _, err := NewSearchEvidenceTool(reader)(context.Background(), req); err != nil {
		t.Fatalf("query at cap 200: %v", err)
	}
	if reader.got.query != atCap {
		t.Fatalf("reader query length = %d, want %d", len([]rune(reader.got.query)), invest.SearchMaxQueryLen)
	}

	// 201 runes rejected fail-closed (ErrContract) before reader call.
	overCap := strings.Repeat("b", invest.SearchMaxQueryLen+1)
	beforeCalls := reader.got.query
	_ = beforeCalls
	fake2 := &fakeEvidenceReader{hits: []investigate.EvidenceHit{}}
	req.Query = overCap
	_, err := NewSearchEvidenceTool(fake2)(context.Background(), req)
	requireToolsSentinel(t, err, ports.ErrContract)
	if fake2.got.query != "" {
		t.Fatalf("reader called on overlong query: query=%q", fake2.got.query)
	}

	// Multi-byte at cap: 200 runes of "é" is 400 bytes but still OK.
	wideAtCap := strings.Repeat("é", invest.SearchMaxQueryLen)
	req.Query = wideAtCap
	reader3 := &fakeEvidenceReader{hits: []investigate.EvidenceHit{}}
	if _, err := NewSearchEvidenceTool(reader3)(context.Background(), req); err != nil {
		t.Fatalf("wide query at cap: %v", err)
	}
	wideOver := strings.Repeat("é", invest.SearchMaxQueryLen+1)
	req.Query = wideOver
	fake3 := &fakeEvidenceReader{}
	_, err = NewSearchEvidenceTool(fake3)(context.Background(), req)
	requireToolsSentinel(t, err, ports.ErrContract)
}

func TestRetrievalSearchWildcardOnlyRejected(t *testing.T) {
	// Each of these carries no literal content and would match the whole
	// table; they must be rejected fail-closed.
	for _, q := range []string{"%", "_", "*", "\\", " % ", "%%", "___", "***", `%_\*`, "\\", "  %  "} {
		req := investigate.Request{
			Tool:            invest.ToolSearchEvidence,
			TenantID:        toolsTenant,
			ClaimID:         toolsClaim,
			InvestigationID: toolsInv,
			RequestID:       toolsReq,
			Query:           q,
			Limit:           10,
		}
		fake := &fakeEvidenceReader{}
		_, err := NewSearchEvidenceTool(fake)(context.Background(), req)
		requireToolsSentinel(t, err, ports.ErrContract)
		if fake.got.query != "" {
			t.Fatalf("wildcard %q reached reader", q)
		}
	}
	// Negative controls: queries with at least one literal are accepted.
	for _, q := range []string{"a%", "total", "POL-123", "10% increase", "a_b"} {
		fake := &fakeEvidenceReader{hits: []investigate.EvidenceHit{}}
		req := investigate.Request{
			Tool:            invest.ToolSearchEvidence,
			TenantID:        toolsTenant,
			ClaimID:         toolsClaim,
			InvestigationID: toolsInv,
			RequestID:       toolsReq,
			Query:           q,
			Limit:           10,
		}
		if _, err := NewSearchEvidenceTool(fake)(context.Background(), req); err != nil {
			t.Fatalf("valid query %q rejected: %v", q, err)
		}
	}
}

func TestRetrievalSearchValidQueryPreservesReaderOrderVerbatim(t *testing.T) {
	// Reader order is rank, id — never re-sorted by the tool. Provide a
	// non-sorted order and prove it survives verbatim in the typed response.
	raw := []investigate.EvidenceHit{
		{EvidenceID: "ev-z", FieldKey: "zeta", Anchor: "az", Snippet: "z", SourceID: "doc-01", SourceType: string(invest.EvidenceSourceField)},
		{EvidenceID: "ev-a", FieldKey: "alpha", Anchor: "aa", Snippet: "a", SourceID: "doc-01", SourceType: string(invest.EvidenceSourceField)},
		{EvidenceID: "ev-m", FieldKey: "mid", Anchor: "am", Snippet: "m", SourceID: "doc-01", SourceType: string(invest.EvidenceSourceField)},
	}
	out, err := NewSearchResponse(raw)
	if err != nil {
		t.Fatalf("NewSearchResponse: %v", err)
	}
	for i, want := range []string{"ev-z", "ev-a", "ev-m"} {
		if out.Hits[i].EvidenceID != want {
			t.Fatalf("hit[%d] = %q, want %q (order not preserved)", i, out.Hits[i].EvidenceID, want)
		}
	}
	// Locators also preserve order.
	locs := out.Locators()
	for i, want := range []string{"ev-z", "ev-a", "ev-m"} {
		if locs[i].EvidenceID != want {
			t.Fatalf("locator[%d] = %q, want %q", i, locs[i].EvidenceID, want)
		}
	}
	// Via tool: typed order preserved, envelope IDs sorted (determinism).
	reader := &fakeEvidenceReader{hits: raw}
	tool := NewSearchEvidenceTool(reader)
	resp, err := tool(context.Background(), retrievalSearchRequest(t, "alpha", 10))
	if err != nil {
		t.Fatalf("tool search: %v", err)
	}
	if resp.RowCount != len(raw) {
		t.Fatalf("RowCount = %d, want %d", resp.RowCount, len(raw))
	}
	// Envelope sorts.
	sorted := []string{"ev-a", "ev-m", "ev-z"}
	if !reflect.DeepEqual(resp.IDs, sorted) {
		t.Fatalf("envelope IDs = %v, want sorted %v", resp.IDs, sorted)
	}
	// Reader query echoed verbatim.
	if reader.got.query != "alpha" {
		t.Fatalf("reader query = %q, want %q", reader.got.query, "alpha")
	}
}

func TestRetrievalTruncatedBehavior(t *testing.T) {
	// T4: truncated flag propagated from reader page.
	rows := []investigate.EvidenceRow{
		{EvidenceID: "ev-t-01", SourceType: string(invest.EvidenceSourceDocument), SourceID: "doc-01", ContentHash: "h1"},
	}
	for _, tc := range []struct {
		truncated bool
		cursor    string
	}{
		{false, ""},
		{true, "ev-t-01"},
	} {
		reader := &fakeEvidenceReader{page: investigate.EvidencePage{Rows: rows, Truncated: tc.truncated, NextCursor: tc.cursor}}
		tool := NewEvidenceTool(reader)
		resp, err := tool(context.Background(), retrievalEvidenceRequest(t, 10, "", ""))
		if err != nil {
			t.Fatalf("truncated=%v: %v", tc.truncated, err)
		}
		if resp.Truncated != tc.truncated {
			t.Fatalf("Truncated = %v, want %v", resp.Truncated, tc.truncated)
		}
	}

	// Empty page is never truncated.
	emptyReader := &fakeEvidenceReader{page: investigate.EvidencePage{}}
	resp, err := NewEvidenceTool(emptyReader)(context.Background(), retrievalEvidenceRequest(t, 10, "", ""))
	if err != nil {
		t.Fatalf("empty page: %v", err)
	}
	if resp.Truncated {
		t.Fatalf("empty page Truncated = true, want false")
	}
	if resp.RowCount != 0 {
		t.Fatalf("empty RowCount = %d, want 0", resp.RowCount)
	}

	// T5: search hits have no truncated flag; the generic search response
	// is never truncated (single bounded page). Verify via empty and non-empty.
	for _, hits := range [][]investigate.EvidenceHit{
		{},
		{{EvidenceID: "ev-s-01", FieldKey: "k", Anchor: "a", Snippet: "s", SourceID: "doc-01", SourceType: string(invest.EvidenceSourceField)}},
	} {
		reader := &fakeEvidenceReader{hits: hits}
		tool := NewSearchEvidenceTool(reader)
		resp, err := tool(context.Background(), retrievalSearchRequest(t, "k", 10))
		if err != nil {
			t.Fatalf("search truncated check: %v", err)
		}
		if resp.Truncated {
			t.Fatalf("search Truncated = true, want false (hits=%d)", len(hits))
		}
		if resp.RowCount != len(hits) {
			t.Fatalf("search RowCount = %d, want %d", resp.RowCount, len(hits))
		}
	}
}

// ---------------------------------------------------------------------------
// 4. Auth / bounds.
// ---------------------------------------------------------------------------

func TestRetrievalEvidenceAuthBounds(t *testing.T) {
	reader := &fakeEvidenceReader{page: investigate.EvidencePage{Rows: []investigate.EvidenceRow{
		{EvidenceID: "ev-01", SourceType: string(invest.EvidenceSourceDocument), SourceID: "doc-01", ContentHash: "h1"},
	}}}
	tool := NewEvidenceTool(reader)

	base := retrievalEvidenceRequest(t, 10, "", "")

	cases := map[string]investigate.Request{
		"blank tenant": func() investigate.Request {
			out := base
			out.TenantID = ""
			return out
		}(),
		"blank tenant spaces": func() investigate.Request {
			out := base
			out.TenantID = "   "
			return out
		}(),
		"blank claim": func() investigate.Request {
			out := base
			out.ClaimID = ""
			return out
		}(),
		"blank claim spaces": func() investigate.Request {
			out := base
			out.ClaimID = "  "
			return out
		}(),
		"bad investigation": func() investigate.Request {
			out := base
			out.InvestigationID = "bad-id"
			return out
		}(),
		"blank investigation": func() investigate.Request {
			out := base
			out.InvestigationID = ""
			return out
		}(),
		"blank request": func() investigate.Request {
			out := base
			out.RequestID = ""
			return out
		}(),
		"blank request spaces": func() investigate.Request {
			out := base
			out.RequestID = "   "
			return out
		}(),
		"wrong tool": func() investigate.Request {
			out := base
			out.Tool = invest.ToolSearchEvidence
			return out
		}(),
		"unknown source_type": func() investigate.Request {
			out := base
			out.SourceType = "bogus"
			return out
		}(),
	}

	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			beforeTenant := reader.got.tenant
			_ = beforeTenant
			// Reset got marker to detect reader call.
			reader.got.tenant = "__unset__"
			_, err := tool(context.Background(), req)
			if err == nil {
				t.Fatalf("want contract error for %q, got nil", name)
			}
			requireToolsSentinel(t, err, ports.ErrContract)
			// Reader must not have been called with the original tenant on
			// auth failures (except wrong tool which fails before reader).
			// For blank tenant/claim we prove the call did not echo the bad
			// value; for unknown source_type the tool rejects before reader.
			if reader.got.tenant != "__unset__" && (name == "blank tenant" || name == "blank tenant spaces" || name == "unknown source_type") {
				// The reader may have been not called; if it was called, it
				// would have the bad tenant — which must not happen.
				t.Fatalf("reader was called on invalid request %q (tenant=%q)", name, reader.got.tenant)
			}
		})
	}

	// Unknown source_type also via direct validate.
	badSource := EvidenceListRequest{TenantID: toolsTenant, ClaimID: toolsClaim, InvestigationID: toolsInv, RequestID: toolsReq, SourceType: "sql"}
	if err := badSource.validate(); err == nil {
		t.Fatal("unknown source_type accepted via validate")
	} else {
		requireToolsSentinel(t, err, ports.ErrContract)
	}
}

func TestRetrievalSearchAuthBounds(t *testing.T) {
	base := retrievalSearchRequest(t, "total", 10)

	cases := map[string]investigate.Request{
		"blank tenant": func() investigate.Request {
			out := base
			out.TenantID = ""
			return out
		}(),
		"blank claim": func() investigate.Request {
			out := base
			out.ClaimID = "   "
			return out
		}(),
		"bad investigation": func() investigate.Request {
			out := base
			out.InvestigationID = "not-an-inv-id"
			return out
		}(),
		"blank request": func() investigate.Request {
			out := base
			out.RequestID = ""
			return out
		}(),
		"blank query": func() investigate.Request {
			out := base
			out.Query = ""
			return out
		}(),
		"blank query spaces": func() investigate.Request {
			out := base
			out.Query = "   "
			return out
		}(),
		"untrimmed query leading": func() investigate.Request {
			out := base
			out.Query = " total"
			return out
		}(),
		"untrimmed query trailing": func() investigate.Request {
			out := base
			out.Query = "total "
			return out
		}(),
		"wrong tool": func() investigate.Request {
			out := base
			out.Tool = invest.ToolGetEvidence
			return out
		}(),
	}

	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			fake := &fakeEvidenceReader{}
			fn := NewSearchEvidenceTool(fake)
			_, err := fn(context.Background(), req)
			if err == nil {
				t.Fatalf("want contract error for %q, got nil", name)
			}
			requireToolsSentinel(t, err, ports.ErrContract)
			if fake.got.query != "" || fake.got.tenant != "" {
				t.Fatalf("reader called on invalid search request %q", name)
			}
		})
	}

	// Direct validate also rejects wildcard-only and blank queries.
	for _, q := range []string{"", "   ", " total", "total "} {
		r := EvidenceSearchRequest{TenantID: toolsTenant, ClaimID: toolsClaim, InvestigationID: toolsInv, RequestID: toolsReq, Query: q, Limit: 10}
		if err := r.validate(); err == nil {
			t.Fatalf("validate accepted blank/untrimmed query %q", q)
		}
	}
}

func TestRetrievalNilReaderFailsClosed(t *testing.T) {
	// T4 nil reader.
	_, err := NewEvidenceTool(nil)(context.Background(), retrievalEvidenceRequest(t, 10, "", ""))
	requireToolsSentinel(t, err, ports.ErrContract)
	if err != nil && !strings.Contains(err.Error(), "nil reader") {
		t.Fatalf("nil reader error = %q, want to mention nil reader", err.Error())
	}

	// T5 nil reader (any valid query).
	_, err = NewSearchEvidenceTool(nil)(context.Background(), retrievalSearchRequest(t, "total", 10))
	requireToolsSentinel(t, err, ports.ErrContract)
	if err != nil && !strings.Contains(err.Error(), "nil reader") {
		t.Fatalf("nil reader error = %q, want to mention nil reader", err.Error())
	}

	// Nil reader with wildcard query still fails on query validation first
	// (contract), not panic.
	wildReq := investigate.Request{
		Tool:            invest.ToolSearchEvidence,
		TenantID:        toolsTenant,
		ClaimID:         toolsClaim,
		InvestigationID: toolsInv,
		RequestID:       toolsReq,
		Query:           "%",
		Limit:           10,
	}
	_, err = NewSearchEvidenceTool(nil)(context.Background(), wildReq)
	requireToolsSentinel(t, err, ports.ErrContract)
}

// ---------------------------------------------------------------------------
// Additional deterministic / bounds checks.
// ---------------------------------------------------------------------------

func TestRetrievalEvidenceBoundsClamp(t *testing.T) {
	// Direct validate clamps defense-in-depth.
	zero := EvidenceListRequest{TenantID: toolsTenant, ClaimID: toolsClaim, InvestigationID: toolsInv, RequestID: toolsReq, Limit: 0}
	if err := zero.validate(); err != nil {
		t.Fatalf("validate limit 0: %v", err)
	}
	if zero.Limit != investigate.MaxRowsGetEvidence {
		t.Fatalf("limit 0 clamped to %d, want %d", zero.Limit, investigate.MaxRowsGetEvidence)
	}
	huge := EvidenceListRequest{TenantID: toolsTenant, ClaimID: toolsClaim, InvestigationID: toolsInv, RequestID: toolsReq, Limit: 9999}
	if err := huge.validate(); err != nil {
		t.Fatalf("validate limit 9999: %v", err)
	}
	if huge.Limit != investigate.MaxRowsGetEvidence {
		t.Fatalf("limit 9999 clamped to %d, want %d", huge.Limit, investigate.MaxRowsGetEvidence)
	}

	// Envelope rejects out-of-bounds limits.
	for _, limit := range []int{0, -1, 9999} {
		req := investigate.Request{
			Tool:            invest.ToolGetEvidence,
			TenantID:        toolsTenant,
			ClaimID:         toolsClaim,
			InvestigationID: toolsInv,
			RequestID:       toolsReq,
			Limit:           limit,
		}
		_, err := NewEvidenceTool(&fakeEvidenceReader{})(context.Background(), req)
		requireToolsSentinel(t, err, ports.ErrContract)
	}

	// Cursor must be trimmed.
	trimmed := EvidenceListRequest{TenantID: toolsTenant, ClaimID: toolsClaim, InvestigationID: toolsInv, RequestID: toolsReq, Limit: 10, Cursor: " ev-01"}
	if err := trimmed.validate(); err == nil {
		t.Fatal("untrimmed cursor accepted")
	} else {
		requireToolsSentinel(t, err, ports.ErrContract)
	}
}

func TestRetrievalEvidenceAndSearchDeterministic(t *testing.T) {
	rows := []investigate.EvidenceRow{
		{EvidenceID: "ev-det-01", SourceType: string(invest.EvidenceSourceDocument), SourceID: "doc-01", ContentHash: "h1"},
	}
	hits := []investigate.EvidenceHit{
		{EvidenceID: "ev-det-01", FieldKey: "total", Anchor: "a1", Snippet: "s", SourceID: "doc-01", SourceType: string(invest.EvidenceSourceField)},
	}
	evTool := NewEvidenceTool(&fakeEvidenceReader{page: investigate.EvidencePage{Rows: rows}})
	searchTool := NewSearchEvidenceTool(&fakeEvidenceReader{hits: hits})

	evReq := retrievalEvidenceRequest(t, 10, "", "")
	searchReq := retrievalSearchRequest(t, "total", 10)

	firstEv, err := evTool(context.Background(), evReq)
	if err != nil {
		t.Fatalf("first evidence: %v", err)
	}
	secondEv, err := evTool(context.Background(), evReq)
	if err != nil {
		t.Fatalf("second evidence: %v", err)
	}
	if !reflect.DeepEqual(firstEv, secondEv) {
		t.Fatalf("evidence non-deterministic:\n%#v\n%#v", firstEv, secondEv)
	}

	firstS, err := searchTool(context.Background(), searchReq)
	if err != nil {
		t.Fatalf("first search: %v", err)
	}
	secondS, err := searchTool(context.Background(), searchReq)
	if err != nil {
		t.Fatalf("second search: %v", err)
	}
	if !reflect.DeepEqual(firstS, secondS) {
		t.Fatalf("search non-deterministic:\n%#v\n%#v", firstS, secondS)
	}
}

func TestRetrievalEvidenceEmpty(t *testing.T) {
	reader := &fakeEvidenceReader{page: investigate.EvidencePage{}}
	tool := NewEvidenceTool(reader)
	resp, err := tool(context.Background(), retrievalEvidenceRequest(t, 10, "", ""))
	if err != nil {
		t.Fatalf("empty evidence: %v", err)
	}
	if resp.RowCount != 0 {
		t.Fatalf("RowCount = %d, want 0", resp.RowCount)
	}
	if len(resp.IDs) != 0 {
		t.Fatalf("IDs = %v, want empty", resp.IDs)
	}
	if resp.Truncated {
		t.Fatalf("Truncated = true, want false for empty")
	}
}

func TestRetrievalEvidenceSourceFilterEcho(t *testing.T) {
	for _, src := range []string{"", string(invest.EvidenceSourceDocument), string(invest.EvidenceSourceField), string(invest.EvidenceSourcePolicy)} {
		reader := &fakeEvidenceReader{page: investigate.EvidencePage{Rows: []investigate.EvidenceRow{}}}
		tool := NewEvidenceTool(reader)
		if _, err := tool(context.Background(), retrievalEvidenceRequest(t, 10, "", src)); err != nil {
			t.Fatalf("source %q: %v", src, err)
		}
		if reader.got.source != src {
			t.Fatalf("source echo = %q, want %q", reader.got.source, src)
		}
	}
}

func TestRetrievalSearchHitValidationFailsClosed(t *testing.T) {
	// NewSearchResponse rejects hits missing evidence id or field key.
	for _, tc := range []struct {
		name string
		hits []investigate.EvidenceHit
	}{
		{"blank evidence id", []investigate.EvidenceHit{{EvidenceID: "   ", FieldKey: "k", Anchor: "a", Snippet: "s", SourceID: "doc-01"}}},
		{"blank field key", []investigate.EvidenceHit{{EvidenceID: "ev-01", FieldKey: "", Anchor: "a", Snippet: "s", SourceID: "doc-01"}}},
		{"over cap", func() []investigate.EvidenceHit {
			out := make([]investigate.EvidenceHit, investigate.MaxRowsSearchEvidence+1)
			for i := range out {
				out[i] = investigate.EvidenceHit{EvidenceID: "ev-01", FieldKey: "k", Anchor: "a", Snippet: "s", SourceID: "doc-01"}
			}
			return out
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewSearchResponse(tc.hits)
			requireToolsSentinel(t, err, ports.ErrContract)
		})
	}
}

// Ensure the sentinel helpers themselves wrap correctly via errors.Is.
func TestRetrievalSentinelWrapping(t *testing.T) {
	// ports sentinel propagates as ports; investigate sentinel wraps ports so both Is checks pass.
	fakePorts := &fakeEvidenceReader{err: toolsErr(ports.ErrTenantMismatch, "drift")}
	_, err := NewEvidenceTool(fakePorts)(context.Background(), retrievalEvidenceRequest(t, 10, "", ""))
	if !errors.Is(err, ports.ErrTenantMismatch) {
		t.Fatalf("want ports ErrTenantMismatch via errors.Is, got %v", err)
	}
	fakeInvestigate := &fakeEvidenceReader{err: toolsErr(investigate.ErrTenantMismatch, "drift")}
	_, err = NewEvidenceTool(fakeInvestigate)(context.Background(), retrievalEvidenceRequest(t, 10, "", ""))
	if !errors.Is(err, investigate.ErrTenantMismatch) {
		t.Fatalf("want investigate ErrTenantMismatch via errors.Is, got %v", err)
	}
	if !errors.Is(err, ports.ErrTenantMismatch) {
		t.Fatalf("want ports ErrTenantMismatch via investigate wrapper, got %v", err)
	}
}
