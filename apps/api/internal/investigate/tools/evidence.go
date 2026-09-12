// T4 (get_evidence) list and T5 (search_evidence) backed by
// investigate.EvidenceReader.
//
// Logging: none (see policy.go). Rows carry IDs, hashes, and provenance
// only, and nothing here is logged regardless.
//
// Envelope: both tools implement investigate.ToolFunc via the tool.go New*
// constructors only. T4 pages evidence rows with an optional source-type
// filter ("" = all; otherwise one of the six closed source types). T5
// searches field/value/anchor metadata: the query must be 1..200 runes
// (invest.SearchMaxQueryLen) and wildcard-only queries are rejected fail
// closed (a bare "%" would match the whole table — that is a contract
// error, not a search).
//
// Snippets: investigate.EvidenceReader returns hits whose snippets are
// ALREADY truncated rune-aware to investigate.MaxSnippetRunes at the
// reader (see readers.go truncateSnippet). This file carries them into
// its own SearchResponse verbatim and never expands them; the generic
// envelope (SearchEvidenceResponse.ToResponse) cites evidence IDs only,
// so snippets cannot leak past the typed response by construction. The
// tool additionally re-truncates defensively so a non-conforming reader
// can never widen the window.
package tools

import (
	"context"
	"fmt"
	"strings"

	"claimops-api/internal/claims"
	"claimops-api/internal/invest"
	"claimops-api/internal/investigate"
	"claimops-api/internal/ports"
)

// maxSearchSnippetRunes mirrors investigate.MaxSnippetRunes for the
// defensive re-truncation below. The reader is the primary truncation
// point; this cap is a backstop only (identical value by design).
const maxSearchSnippetRunes = 200

// ---------------------------------------------------------------------------
// T4: get_evidence list
// ---------------------------------------------------------------------------

// EvidenceListRequest is the T4 input: envelope identity plus paging and
// the optional source-type filter ("" = all).
type EvidenceListRequest struct {
	TenantID        string
	ClaimID         string
	InvestigationID string
	RequestID       string
	Limit           int
	Cursor          string
	SourceType      string
}

// validate rejects blank identity, malformed investigation IDs, untrimmed
// cursors, and unknown source types, and clamps the limit into
// 1..MaxRowsGetEvidence. Every rejection wraps ports.ErrContract.
func (r *EvidenceListRequest) validate() error {
	if err := claims.TenantID(r.TenantID).Validate(); err != nil {
		return fmt.Errorf("%w: evidence: %v", ports.ErrContract, err)
	}
	if err := claims.ClaimID(r.ClaimID).Validate(); err != nil {
		return fmt.Errorf("%w: evidence: %v", ports.ErrContract, err)
	}
	if err := invest.ValidateID(invest.InvestigationIDPrefix, r.InvestigationID); err != nil {
		return fmt.Errorf("%w: evidence: %v", ports.ErrContract, err)
	}
	if strings.TrimSpace(r.RequestID) == "" {
		return fmt.Errorf("%w: evidence: blank request id", ports.ErrContract)
	}
	if r.Limit <= 0 {
		r.Limit = investigate.MaxRowsGetEvidence
	}
	if r.Limit > investigate.MaxRowsGetEvidence {
		r.Limit = investigate.MaxRowsGetEvidence
	}
	if r.Cursor != "" && r.Cursor != strings.TrimSpace(r.Cursor) {
		return fmt.Errorf("%w: evidence: cursor must be trimmed", ports.ErrContract)
	}
	if r.SourceType != "" {
		switch invest.EvidenceSourceType(r.SourceType) {
		case invest.EvidenceSourceDocument, invest.EvidenceSourceField,
			invest.EvidenceSourcePolicy, invest.EvidenceSourceTPA,
			invest.EvidenceSourceProvider, invest.EvidenceSourceRisk:
		default:
			return fmt.Errorf("%w: evidence: unknown source type %q", ports.ErrContract, r.SourceType)
		}
	}
	return nil
}

// NewEvidenceTool returns the T4 ToolFunc: re-validate the generic
// envelope, clamp paging, page rows via the reader, and respond with the
// IDs/hashes/provenance page. Reader errors propagate untouched.
func NewEvidenceTool(reader investigate.EvidenceReader) investigate.ToolFunc {
	return func(ctx context.Context, req investigate.Request) (investigate.Response, error) {
		if req.Tool != invest.ToolGetEvidence {
			return investigate.Response{}, fmt.Errorf("%w: evidence: dispatched %q, want get_evidence", ports.ErrContract, string(req.Tool))
		}
		if err := req.Validate(); err != nil {
			return investigate.Response{}, err
		}
		in := EvidenceListRequest{
			TenantID:        req.TenantID,
			ClaimID:         req.ClaimID,
			InvestigationID: req.InvestigationID,
			RequestID:       req.RequestID,
			Limit:           req.Limit,
			Cursor:          req.Cursor,
			SourceType:      req.SourceType,
		}
		if err := in.validate(); err != nil {
			return investigate.Response{}, err
		}
		if reader == nil {
			return investigate.Response{}, fmt.Errorf("%w: evidence: nil reader", ports.ErrContract)
		}
		page, err := reader.ListEvidence(ctx, req.TenantID, req.ClaimID, in.Limit, in.Cursor, in.SourceType)
		if err != nil {
			return investigate.Response{}, err
		}
		resp, err := investigate.NewGetEvidenceResponse(page.Rows, page.Truncated, page.NextCursor)
		if err != nil {
			return investigate.Response{}, err
		}
		return resp.ToResponse(), nil
	}
}

// ---------------------------------------------------------------------------
// T5: search_evidence
// ---------------------------------------------------------------------------

// EvidenceSearchRequest is the T5 input: envelope identity plus the
// lexical query. Limit <= 0 selects MaxRowsSearchEvidence; above-cap
// clamps.
type EvidenceSearchRequest struct {
	TenantID        string
	ClaimID         string
	InvestigationID string
	RequestID       string
	Query           string
	Limit           int
}

// isWildcardOnly reports whether q carries no literal content: after
// trimming, every rune is a LIKE wildcard (%, _), a user-typed glob star,
// or the LIKE escape character. Such a query would match the whole table,
// so it is a contract error, not a search.
func isWildcardOnly(q string) bool {
	t := strings.TrimSpace(q)
	if t == "" {
		return true
	}
	for _, r := range t {
		switch r {
		case '%', '_', '*', '\\':
		default:
			return false
		}
	}
	return true
}

// validate rejects blank identity, malformed investigation IDs, blank or
// overlong queries, and wildcard-only queries, and clamps the limit into
// 1..MaxRowsSearchEvidence. Every rejection wraps ports.ErrContract.
func (r *EvidenceSearchRequest) validate() error {
	if err := claims.TenantID(r.TenantID).Validate(); err != nil {
		return fmt.Errorf("%w: search: %v", ports.ErrContract, err)
	}
	if err := claims.ClaimID(r.ClaimID).Validate(); err != nil {
		return fmt.Errorf("%w: search: %v", ports.ErrContract, err)
	}
	if err := invest.ValidateID(invest.InvestigationIDPrefix, r.InvestigationID); err != nil {
		return fmt.Errorf("%w: search: %v", ports.ErrContract, err)
	}
	if strings.TrimSpace(r.RequestID) == "" {
		return fmt.Errorf("%w: search: blank request id", ports.ErrContract)
	}
	if strings.TrimSpace(r.Query) == "" || r.Query != strings.TrimSpace(r.Query) {
		return fmt.Errorf("%w: search: query must be trimmed non-blank", ports.ErrContract)
	}
	if len([]rune(r.Query)) > invest.SearchMaxQueryLen {
		return fmt.Errorf("%w: search: query exceeds %d runes", ports.ErrContract, invest.SearchMaxQueryLen)
	}
	if isWildcardOnly(r.Query) {
		return fmt.Errorf("%w: search: wildcard-only query", ports.ErrContract)
	}
	if r.Limit <= 0 {
		r.Limit = investigate.MaxRowsSearchEvidence
	}
	if r.Limit > investigate.MaxRowsSearchEvidence {
		r.Limit = investigate.MaxRowsSearchEvidence
	}
	return nil
}

// SearchHit is one T5 hit: locators plus the snippet. The snippet is
// reader-truncated already (investigate.MaxSnippetRunes); NewSearchResponse
// re-truncates defensively so a non-conforming reader can never widen
// the window.
type SearchHit struct {
	EvidenceID string
	FieldKey   string
	Anchor     string
	Snippet    string
}

// SearchResponse is the T5 output: up to MaxRowsSearchEvidence hits in
// reader order (rank, id — never re-sorted here). Truncated is always
// false (the reader returns at most one bounded page; there is no second
// page to report).
type SearchResponse struct {
	Hits      []SearchHit
	Truncated bool
}

// NewSearchResponse narrows reader hits to the typed T5 shape: pins
// validated (fail closed, ErrContract), snippets re-truncated to the
// reader cap, order preserved verbatim.
func NewSearchResponse(raw []investigate.EvidenceHit) (SearchResponse, error) {
	if len(raw) > investigate.MaxRowsSearchEvidence {
		return SearchResponse{}, fmt.Errorf("%w: search: %d hits exceed cap %d", ports.ErrContract, len(raw), investigate.MaxRowsSearchEvidence)
	}
	out := make([]SearchHit, 0, len(raw))
	for i := range raw {
		h := &raw[i]
		if strings.TrimSpace(h.EvidenceID) == "" || strings.TrimSpace(h.FieldKey) == "" {
			return SearchResponse{}, fmt.Errorf("%w: search: hit[%d] needs evidence id and field key", ports.ErrContract, i)
		}
		out = append(out, SearchHit{
			EvidenceID: h.EvidenceID,
			FieldKey:   h.FieldKey,
			Anchor:     h.Anchor,
			Snippet:    truncateSearchSnippet(h.Snippet),
		})
	}
	return SearchResponse{Hits: out}, nil
}

// Locators narrows the typed hits to envelope locators (evidence + field
// + anchor — never matched text), preserving reader order.
func (s SearchResponse) Locators() []investigate.SearchHit {
	out := make([]investigate.SearchHit, 0, len(s.Hits))
	for i := range s.Hits {
		h := &s.Hits[i]
		out = append(out, investigate.SearchHit{
			EvidenceID: h.EvidenceID,
			FieldKey:   h.FieldKey,
			Anchor:     h.Anchor,
		})
	}
	return out
}

// truncateSearchSnippet re-truncates a snippet rune-aware to the reader
// cap. Primary truncation happens in the reader; this is a backstop so a
// non-conforming reader can never widen the window.
func truncateSearchSnippet(s string) string {
	if len([]rune(s)) <= maxSearchSnippetRunes {
		return s
	}
	return string([]rune(s)[:maxSearchSnippetRunes])
}

// NewSearchEvidenceTool returns the T5 ToolFunc: re-validate the generic
// envelope, validate the query (length + wildcard-only), search via the
// reader, and respond with locator hits. Reader errors propagate
// untouched.
func NewSearchEvidenceTool(reader investigate.EvidenceReader) investigate.ToolFunc {
	return func(ctx context.Context, req investigate.Request) (investigate.Response, error) {
		if req.Tool != invest.ToolSearchEvidence {
			return investigate.Response{}, fmt.Errorf("%w: search: dispatched %q, want search_evidence", ports.ErrContract, string(req.Tool))
		}
		if err := req.Validate(); err != nil {
			return investigate.Response{}, err
		}
		in := EvidenceSearchRequest{
			TenantID:        req.TenantID,
			ClaimID:         req.ClaimID,
			InvestigationID: req.InvestigationID,
			RequestID:       req.RequestID,
			Query:           req.Query,
			Limit:           req.Limit,
		}
		if err := in.validate(); err != nil {
			return investigate.Response{}, err
		}
		if reader == nil {
			return investigativeNilReaderErr()
		}
		raw, err := reader.SearchEvidence(ctx, req.TenantID, req.ClaimID, in.Query, in.Limit)
		if err != nil {
			return investigate.Response{}, err
		}
		// Locators only: snippets are reader-truncated already (and
		// re-truncated by truncateSearchSnippet inside toSearchHits when
		// the typed shape is needed); the generic envelope cites
		// evidence IDs, so snippet bytes cannot cross it.
		locators := toSearchLocators(raw)
		resp, err := investigate.NewSearchEvidenceResponse(locators)
		if err != nil {
			return investigate.Response{}, err
		}
		return resp.ToResponse(), nil
	}
}

// toSearchLocators narrows reader hits to envelope locators (evidence +
// field + anchor — never matched text). Reader order (rank, id) is
// preserved verbatim; the constructor validates the pins.
func toSearchLocators(raw []investigate.EvidenceHit) []investigate.SearchHit {
	out := make([]investigate.SearchHit, 0, len(raw))
	for i := range raw {
		h := &raw[i]
		out = append(out, investigate.SearchHit{
			EvidenceID: h.EvidenceID,
			FieldKey:   h.FieldKey,
			Anchor:     h.Anchor,
		})
	}
	return out
}

// investigativeNilReaderErr keeps the nil-reader failure on the ports
// contract taxonomy like every other tool-owned rejection.
func investigativeNilReaderErr() (investigate.Response, error) {
	return investigate.Response{}, fmt.Errorf("%w: search: nil reader", ports.ErrContract)
}
