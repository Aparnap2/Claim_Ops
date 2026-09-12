// Package investigate owns the Chunk A executor seam for issue #54:
// typed per-tool request/response shapes, the generic Request/Response
// envelope plus ToolFunc registry signature that sibling agents program
// to, local error sentinels, and execution bounds.
//
// SEAM (sibling contract — investigate/tools/* implement it, readers/pin
// consume it; this file is the authority, siblings program to it):
//
//	type ToolFunc func(ctx context.Context, req Request) (Response, error)
//
// A sibling tool implementation receives one validated generic Request,
// performs exactly one bounded backend read (or, for T11, one write-once
// report insert), and returns one validated Response carrying IDs,
// hashes, and counts only — never raw values, document bytes, or floats.
// The executor (executor.go) re-validates both sides, clamps Limit to
// MaxRows(tool), applies ToolTimeout per attempt, and retries Upstream
// failures only (at most MaxRetries attempts total).
//
// Per-tool knob mapping onto the generic envelope (fail closed: a knob
// set on a tool that does not own it is a contract error):
//
//	Tool (T-code)                  Limit  Cursor  Query  SubjectID   SourceType  Hash/Payload
//	get_claim (T1)                 1      -       -      -           -           -
//	get_policy_context (T2)        1      -       -      policy id   -           -
//	get_documents (T3)             <=50   page    -      -           -           -
//	get_evidence (T4)              <=50   page    -      -           filter      -
//	search_evidence (T5)           <=20   -       req    -           -           -
//	get_verification_findings (T6) 1      -       -      -           -           -
//	get_external_policy_status     1      -       -      policy id   -           -
//	  (T7)
//	get_tpa_case (T8)              <=20   -       -      case id     -           -
//	get_provider_encounter (T9)    1      -       -      encounter   -           -
//	get_risk_signals (T10)         <=20   -       -      -           -           -
//	create_investigation_report    1      -       -      exception   -           req/req
//	  (T11)                                                        id
//
// Construction path: use the per-tool New* constructors (they validate
// and convert via ToRequest/ToResponse). Hand-built generic envelopes are
// accepted but must pass Validate. Never model-aware: no prompt, no model
// client, no confidence floats. encoding/json v1 only.
package investigate

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"claimops-api/internal/invest"
	"claimops-api/internal/ports"
)

// Local sentinels. Each wraps the ports taxonomy with %w so callers can
// classify with errors.Is against either layer (investigate or ports)
// without depending on adapter internals.
var (
	// ErrNotFound wraps ports.ErrNotFound: the backend has no record.
	ErrNotFound = fmt.Errorf("investigate: not found: %w", ports.ErrNotFound)
	// ErrContract wraps ports.ErrContract: malformed shape, bad knob, or
	// any fail-closed validation the tool owns.
	ErrContract = fmt.Errorf("investigate: contract failure: %w", ports.ErrContract)
	// ErrTenantMismatch wraps ports.ErrTenantMismatch: a backend row, or
	// the request echo, names a different tenant than the scope.
	ErrTenantMismatch = fmt.Errorf("investigate: tenant mismatch: %w", ports.ErrTenantMismatch)
	// ErrUpstream wraps ports.ErrUpstream: transient failure (HTTP 5xx,
	// timeout, network). The ONLY retried class.
	ErrUpstream = fmt.Errorf("investigate: upstream failure: %w", ports.ErrUpstream)
)

// Execution bounds (spec §3.2 Bounds column, mirrored from
// invest.DefaultBound so the executor enforces data it does not set).
const (
	// ToolTimeout bounds one backend attempt (ADR-004 3s precedent).
	ToolTimeout = 3 * time.Second
	// MaxRetries caps TOTAL attempts per Execute (1 initial + retries);
	// only ErrUpstream is retried, all other classes return immediately.
	MaxRetries = 3
	// MaxReportBytes caps the T11 canonical report payload (1 MiB).
	MaxReportBytes = 1 << 20
	// MaxResponseIDs caps any Response.IDs list (covers 1 subject row +
	// the T9 50-doc-ref cap with headroom; per-tool caps are tighter).
	MaxResponseIDs = 64
)

// Per-tool MaxRows (T1:1, T3:50, T4:50, T5:20, T8:20, T9:1, T10:20; the
// remaining single-row tools are 1 by contract).
const (
	MaxRowsGetClaim                = 1
	MaxRowsGetPolicyContext        = 1
	MaxRowsGetDocuments            = 50
	MaxRowsGetEvidence             = 50
	MaxRowsSearchEvidence          = 20
	MaxRowsGetVerificationFindings = 1
	MaxRowsGetExternalPolicyStatus = 1
	MaxRowsGetTPACase              = 20
	MaxRowsGetProviderEncounter    = 1
	MaxRowsGetRiskSignals          = 20
	MaxRowsCreateReport            = 1
	MaxProviderDocRefs             = 50
)

// MaxRows returns the declared row cap for an allowlisted tool.
// Unknown names fail closed (wraps ErrContract).
func MaxRows(tool invest.ToolName) (int, error) {
	switch tool {
	case invest.ToolGetClaim:
		return MaxRowsGetClaim, nil
	case invest.ToolGetPolicyContext:
		return MaxRowsGetPolicyContext, nil
	case invest.ToolGetDocuments:
		return MaxRowsGetDocuments, nil
	case invest.ToolGetEvidence:
		return MaxRowsGetEvidence, nil
	case invest.ToolSearchEvidence:
		return MaxRowsSearchEvidence, nil
	case invest.ToolGetVerificationFindings:
		return MaxRowsGetVerificationFindings, nil
	case invest.ToolGetExternalPolicyStatus:
		return MaxRowsGetExternalPolicyStatus, nil
	case invest.ToolGetTPACase:
		return MaxRowsGetTPACase, nil
	case invest.ToolGetProviderEncounter:
		return MaxRowsGetProviderEncounter, nil
	case invest.ToolGetRiskSignals:
		return MaxRowsGetRiskSignals, nil
	case invest.ToolCreateInvestigationReport:
		return MaxRowsCreateReport, nil
	default:
		return 0, fmt.Errorf("investigate: unknown tool %q: %w", string(tool), ErrContract)
	}
}

// ToolFunc is THE seam: one bounded backend call. Implementations live in
// investigate/tools/* (sibling-owned), are injected into the Executor
// registry, and must be pure backend reads (T11: one write-once insert)
// with no model access. ctx already carries ToolTimeout; req and the
// returned Response are re-validated by the executor.
type ToolFunc func(ctx context.Context, req Request) (Response, error)

// Request is the generic tool envelope. Prefer the per-tool New*
// constructors (they set exactly the knobs their tool owns); a hand-built
// Request must still pass Validate.
type Request struct {
	Tool            invest.ToolName
	TenantID        string
	ClaimID         string
	InvestigationID string
	RequestID       string
	Limit           int
	Cursor          string
	Query           string
	SubjectID       string
	SourceType      string
	Hash            string
	Payload         []byte
}

// NewRequest builds a generic envelope with full validation. limit <= 0
// selects the tool's MaxRows default; limit above MaxRows is rejected
// (fail closed at construction; the executor additionally clamps).
func NewRequest(tool invest.ToolName, tenantID, claimID, investigationID, requestID string, limit int) (Request, error) {
	max, err := MaxRows(tool)
	if err != nil {
		return Request{}, err
	}
	if limit <= 0 {
		limit = max
	}
	r := Request{
		Tool: tool, TenantID: tenantID, ClaimID: claimID,
		InvestigationID: investigationID, RequestID: requestID, Limit: limit,
	}
	if err := r.Validate(); err != nil {
		return Request{}, err
	}
	return r, nil
}

// Validate checks the envelope standalone: allowlisted tool, trimmed
// non-blank identity echo, Limit within 1..MaxRows(tool), and the
// per-tool knob ownership table from the package doc.
func (r Request) Validate() error {
	if !invest.IsAllowlisted(r.Tool) {
		return fmt.Errorf("investigate: request tool %q not allowlisted: %w", string(r.Tool), ErrContract)
	}
	for _, id := range []struct {
		name, val string
	}{
		{"tenant_id", r.TenantID}, {"claim_id", r.ClaimID},
		{"investigation_id", r.InvestigationID}, {"request_id", r.RequestID},
	} {
		if strings.TrimSpace(id.val) == "" {
			return fmt.Errorf("investigate: request has blank %s: %w", id.name, ErrContract)
		}
		if id.val != strings.TrimSpace(id.val) {
			return fmt.Errorf("investigate: request %s must be trimmed: %w", id.name, ErrContract)
		}
	}
	max, err := MaxRows(r.Tool)
	if err != nil {
		return err
	}
	if r.Limit < 1 || r.Limit > max {
		return fmt.Errorf("investigate: request limit %d outside 1..%d for %s: %w", r.Limit, max, string(r.Tool), ErrContract)
	}
	if err := checkKnob(r.Cursor, "cursor", r.Tool, invest.ToolGetDocuments, invest.ToolGetEvidence); err != nil {
		return err
	}
	if err := checkKnob(r.Query, "query", r.Tool, invest.ToolSearchEvidence); err != nil {
		return err
	}
	if err := checkKnob(r.SubjectID, "subject_id", r.Tool,
		invest.ToolGetPolicyContext, invest.ToolGetExternalPolicyStatus,
		invest.ToolGetTPACase, invest.ToolGetProviderEncounter,
		invest.ToolCreateInvestigationReport); err != nil {
		return err
	}
	if err := checkKnob(r.SourceType, "source_type", r.Tool, invest.ToolGetEvidence); err != nil {
		return err
	}
	if err := checkKnob(r.Hash, "hash", r.Tool, invest.ToolCreateInvestigationReport); err != nil {
		return err
	}
	if len(r.Payload) != 0 && r.Tool != invest.ToolCreateInvestigationReport {
		return fmt.Errorf("investigate: payload is T11-only: %w", ErrContract)
	}
	if len(r.Payload) > MaxReportBytes {
		return fmt.Errorf("investigate: payload %d bytes exceeds %d: %w", len(r.Payload), MaxReportBytes, ErrContract)
	}
	if r.Tool == invest.ToolGetEvidence && strings.TrimSpace(r.SourceType) != "" {
		switch invest.EvidenceSourceType(r.SourceType) {
		case invest.EvidenceSourceDocument, invest.EvidenceSourceField,
			invest.EvidenceSourcePolicy, invest.EvidenceSourceTPA,
			invest.EvidenceSourceProvider, invest.EvidenceSourceRisk:
		default:
			return fmt.Errorf("investigate: unknown evidence source type %q: %w", r.SourceType, ErrContract)
		}
	}
	if r.Query != "" && len([]rune(r.Query)) > invest.SearchMaxQueryLen {
		return fmt.Errorf("investigate: query exceeds %d runes: %w", invest.SearchMaxQueryLen, ErrContract)
	}
	return nil
}

// checkKnob rejects a knob set on a tool that does not own it.
func checkKnob(val, name string, tool invest.ToolName, owners ...invest.ToolName) error {
	if val == "" {
		return nil
	}
	if !slices.Contains(owners, tool) {
		return fmt.Errorf("investigate: %s is not owned by %s: %w", name, string(tool), ErrContract)
	}
	if val != strings.TrimSpace(val) {
		return fmt.Errorf("investigate: %s must be trimmed: %w", name, ErrContract)
	}
	return nil
}

// Response is the generic tool result: IDs, hashes, and counts only.
// Never values, bytes, or floats.
type Response struct {
	Tool      invest.ToolName
	RowCount  int
	Truncated bool
	IDs       []string
	Hash      string
}

// Validate checks the result standalone: allowlisted tool echo,
// 0..MaxResponseIDs RowCount, sorted unique non-blank IDs within cap.
func (r Response) Validate() error {
	if !invest.IsAllowlisted(r.Tool) {
		return fmt.Errorf("investigate: response tool %q not allowlisted: %w", string(r.Tool), ErrContract)
	}
	if r.RowCount < 0 || r.RowCount > MaxResponseIDs {
		return fmt.Errorf("investigate: response row_count %d outside 0..%d: %w", r.RowCount, MaxResponseIDs, ErrContract)
	}
	if len(r.IDs) > MaxResponseIDs {
		return fmt.Errorf("investigate: response carries %d ids, cap %d: %w", len(r.IDs), MaxResponseIDs, ErrContract)
	}
	if err := checkSortedUnique(r.IDs, "response ids"); err != nil {
		return err
	}
	if r.Hash != "" {
		if strings.TrimSpace(r.Hash) == "" || r.Hash != strings.TrimSpace(r.Hash) {
			return fmt.Errorf("investigate: response hash must be trimmed non-blank: %w", ErrContract)
		}
	}
	return nil
}

// checkSortedUnique requires trimmed non-blank, sorted, duplicate-free IDs.
func checkSortedUnique(ids []string, where string) error {
	for i := range ids {
		if strings.TrimSpace(ids[i]) == "" {
			return fmt.Errorf("investigate: %s has blank id at index %d: %w", where, i, ErrContract)
		}
		if ids[i] != strings.TrimSpace(ids[i]) {
			return fmt.Errorf("investigate: %s id must be trimmed: %w", where, ErrContract)
		}
	}
	if !slices.IsSorted(ids) {
		return fmt.Errorf("investigate: %s not sorted: %w", where, ErrContract)
	}
	for i := 1; i < len(ids); i++ {
		if ids[i] == ids[i-1] {
			return fmt.Errorf("investigate: %s duplicates id %q: %w", where, ids[i], ErrContract)
		}
	}
	return nil
}

// checkSHA256 requires 64 lowercase-or-any-case hex chars.
func checkSHA256(h, where string) error {
	if len(h) != 64 {
		return fmt.Errorf("investigate: %s must be 64 hex chars: %w", where, ErrContract)
	}
	if _, err := hex.DecodeString(h); err != nil {
		return fmt.Errorf("investigate: %s is not hex: %w", where, ErrContract)
	}
	return nil
}

// Base carries the identity echo every per-tool request shares.
type Base struct {
	TenantID        string
	ClaimID         string
	InvestigationID string
	RequestID       string
}

// validate checks trimmed non-blank identity on all four fields.
func (b Base) validate() error {
	for _, id := range []struct {
		name, val string
	}{
		{"tenant_id", b.TenantID}, {"claim_id", b.ClaimID},
		{"investigation_id", b.InvestigationID}, {"request_id", b.RequestID},
	} {
		if strings.TrimSpace(id.val) == "" {
			return fmt.Errorf("investigate: %s is blank: %w", id.name, ErrContract)
		}
		if id.val != strings.TrimSpace(id.val) {
			return fmt.Errorf("investigate: %s must be trimmed: %w", id.name, ErrContract)
		}
	}
	return nil
}

// envelope lifts a validated Base into the generic Request for one tool.
func (b Base) envelope(tool invest.ToolName, limit int) Request {
	return Request{
		Tool: tool, TenantID: b.TenantID, ClaimID: b.ClaimID,
		InvestigationID: b.InvestigationID, RequestID: b.RequestID, Limit: limit,
	}
}

// ---------------------------------------------------------------------------
// T1 get_claim — one claim header (status, version, reference, amount).
// ---------------------------------------------------------------------------

// GetClaimRequest reads one claim header. No knobs beyond identity.
type GetClaimRequest struct{ Base }

// NewGetClaimRequest validates identity and returns the T1 request.
func NewGetClaimRequest(tenantID, claimID, investigationID, requestID string) (GetClaimRequest, error) {
	r := GetClaimRequest{Base{tenantID, claimID, investigationID, requestID}}
	if err := r.Base.validate(); err != nil {
		return GetClaimRequest{}, err
	}
	return r, nil
}

// ToRequest converts to the generic envelope (Limit 1).
func (r GetClaimRequest) ToRequest() Request {
	return r.Base.envelope(invest.ToolGetClaim, MaxRowsGetClaim)
}

// ClaimHeader is the minimal claim projection (no floats, no bytes).
type ClaimHeader struct {
	ClaimID     string
	Status      string
	Reference   string
	AmountPaise int64
	Version     int
}

// GetClaimResponse carries one claim header.
type GetClaimResponse struct {
	Header ClaimHeader
}

// NewGetClaimResponse validates the header projection.
func NewGetClaimResponse(h ClaimHeader) (GetClaimResponse, error) {
	if strings.TrimSpace(h.ClaimID) == "" || h.ClaimID != strings.TrimSpace(h.ClaimID) {
		return GetClaimResponse{}, fmt.Errorf("investigate: claim header needs a trimmed claim id: %w", ErrContract)
	}
	if strings.TrimSpace(h.Status) == "" {
		return GetClaimResponse{}, fmt.Errorf("investigate: claim header needs a status: %w", ErrContract)
	}
	if h.Version < 1 {
		return GetClaimResponse{}, fmt.Errorf("investigate: claim header version must be >= 1: %w", ErrContract)
	}
	if h.AmountPaise < 0 {
		return GetClaimResponse{}, fmt.Errorf("investigate: claim header amount must be >= 0: %w", ErrContract)
	}
	return GetClaimResponse{Header: h}, nil
}

// ToResponse converts to the generic envelope (1 row, claim ID cited).
func (r GetClaimResponse) ToResponse() Response {
	return Response{Tool: invest.ToolGetClaim, RowCount: 1, IDs: []string{r.Header.ClaimID}}
}

// ---------------------------------------------------------------------------
// T2 get_policy_context — policy projection + evidence pin.
// ---------------------------------------------------------------------------

// GetPolicyContextRequest reads one policy projection. SubjectID = policy id.
type GetPolicyContextRequest struct {
	Base
	PolicyID string
}

// NewGetPolicyContextRequest validates identity plus the policy subject.
func NewGetPolicyContextRequest(tenantID, claimID, investigationID, requestID, policyID string) (GetPolicyContextRequest, error) {
	r := GetPolicyContextRequest{Base{tenantID, claimID, investigationID, requestID}, policyID}
	if err := r.Base.validate(); err != nil {
		return GetPolicyContextRequest{}, err
	}
	if strings.TrimSpace(policyID) == "" || policyID != strings.TrimSpace(policyID) {
		return GetPolicyContextRequest{}, fmt.Errorf("investigate: policy context needs a trimmed policy id: %w", ErrContract)
	}
	return r, nil
}

// ToRequest converts to the generic envelope.
func (r GetPolicyContextRequest) ToRequest() Request {
	out := r.Base.envelope(invest.ToolGetPolicyContext, MaxRowsGetPolicyContext)
	out.SubjectID = r.PolicyID
	return out
}

// GetPolicyContextResponse carries the policy projection pin (IDs + hash).
type GetPolicyContextResponse struct {
	PolicyID    string
	Status      string
	ContentHash string
}

// NewGetPolicyContextResponse validates the projection pin.
func NewGetPolicyContextResponse(policyID, status, contentHash string) (GetPolicyContextResponse, error) {
	if strings.TrimSpace(policyID) == "" || policyID != strings.TrimSpace(policyID) {
		return GetPolicyContextResponse{}, fmt.Errorf("investigate: policy context needs a trimmed policy id: %w", ErrContract)
	}
	if strings.TrimSpace(status) == "" {
		return GetPolicyContextResponse{}, fmt.Errorf("investigate: policy context needs a status: %w", ErrContract)
	}
	if strings.TrimSpace(contentHash) == "" {
		return GetPolicyContextResponse{}, fmt.Errorf("investigate: policy context needs a content hash: %w", ErrContract)
	}
	return GetPolicyContextResponse{policyID, status, contentHash}, nil
}

// ToResponse converts to the generic envelope.
func (r GetPolicyContextResponse) ToResponse() Response {
	return Response{Tool: invest.ToolGetPolicyContext, RowCount: 1, IDs: []string{r.PolicyID}, Hash: r.ContentHash}
}

// ---------------------------------------------------------------------------
// T3 get_documents — document metadata list, never bytes. Max 50 rows.
// ---------------------------------------------------------------------------

// GetDocumentsRequest pages document metadata. Cursor is the opaque
// last-seen document ID (empty for the first page).
type GetDocumentsRequest struct {
	Base
	Limit  int
	Cursor string
}

// NewGetDocumentsRequest validates identity plus paging (limit <= 0
// selects MaxRowsGetDocuments; above-cap is rejected).
func NewGetDocumentsRequest(tenantID, claimID, investigationID, requestID string, limit int, cursor string) (GetDocumentsRequest, error) {
	if limit <= 0 {
		limit = MaxRowsGetDocuments
	}
	r := GetDocumentsRequest{Base{tenantID, claimID, investigationID, requestID}, limit, cursor}
	if err := r.Base.validate(); err != nil {
		return GetDocumentsRequest{}, err
	}
	if limit < 1 || limit > MaxRowsGetDocuments {
		return GetDocumentsRequest{}, fmt.Errorf("investigate: documents limit %d outside 1..%d: %w", limit, MaxRowsGetDocuments, ErrContract)
	}
	if cursor != "" && cursor != strings.TrimSpace(cursor) {
		return GetDocumentsRequest{}, fmt.Errorf("investigate: documents cursor must be trimmed: %w", ErrContract)
	}
	return r, nil
}

// ToRequest converts to the generic envelope.
func (r GetDocumentsRequest) ToRequest() Request {
	out := r.Base.envelope(invest.ToolGetDocuments, r.Limit)
	out.Cursor = r.Cursor
	return out
}

// DocumentMeta is one document's metadata (no bytes, no text).
type DocumentMeta struct {
	DocumentID string
	DocType    string
	SHA256     string
	Status     string
}

// GetDocumentsResponse carries up to 50 document metadata rows.
type GetDocumentsResponse struct {
	Documents  []DocumentMeta
	Truncated  bool
	NextCursor string
}

// NewGetDocumentsResponse validates the page (cap 50, non-blank fields).
func NewGetDocumentsResponse(docs []DocumentMeta, truncated bool, nextCursor string) (GetDocumentsResponse, error) {
	if len(docs) > MaxRowsGetDocuments {
		return GetDocumentsResponse{}, fmt.Errorf("investigate: documents page carries %d rows, cap %d: %w", len(docs), MaxRowsGetDocuments, ErrContract)
	}
	for i := range docs {
		d := &docs[i]
		if strings.TrimSpace(d.DocumentID) == "" || strings.TrimSpace(d.DocType) == "" ||
			strings.TrimSpace(d.SHA256) == "" || strings.TrimSpace(d.Status) == "" {
			return GetDocumentsResponse{}, fmt.Errorf("investigate: documents[%d] needs id, type, sha256, status: %w", i, ErrContract)
		}
	}
	if nextCursor != "" && nextCursor != strings.TrimSpace(nextCursor) {
		return GetDocumentsResponse{}, fmt.Errorf("investigate: documents next cursor must be trimmed: %w", ErrContract)
	}
	out := append([]DocumentMeta(nil), docs...)
	return GetDocumentsResponse{out, truncated, nextCursor}, nil
}

// ToResponse converts to the generic envelope (IDs sorted for determinism).
func (r GetDocumentsResponse) ToResponse() Response {
	ids := make([]string, 0, len(r.Documents))
	for i := range r.Documents {
		ids = append(ids, r.Documents[i].DocumentID)
	}
	slices.Sort(ids)
	return Response{Tool: invest.ToolGetDocuments, RowCount: len(r.Documents), Truncated: r.Truncated, IDs: ids}
}

// ---------------------------------------------------------------------------
// T4 get_evidence — evidence rows (IDs + hashes + provenance). Max 50/page.
// ---------------------------------------------------------------------------

// GetEvidenceRequest pages evidence rows, optionally filtered by source
// type (one of the invest evidence source types; empty = all).
type GetEvidenceRequest struct {
	Base
	Limit      int
	Cursor     string
	SourceType string
}

// NewGetEvidenceRequest validates identity, paging, and the source filter.
func NewGetEvidenceRequest(tenantID, claimID, investigationID, requestID string, limit int, cursor, sourceType string) (GetEvidenceRequest, error) {
	if limit <= 0 {
		limit = MaxRowsGetEvidence
	}
	r := GetEvidenceRequest{Base{tenantID, claimID, investigationID, requestID}, limit, cursor, sourceType}
	if err := r.Base.validate(); err != nil {
		return GetEvidenceRequest{}, err
	}
	if limit < 1 || limit > MaxRowsGetEvidence {
		return GetEvidenceRequest{}, fmt.Errorf("investigate: evidence limit %d outside 1..%d: %w", limit, MaxRowsGetEvidence, ErrContract)
	}
	if cursor != "" && cursor != strings.TrimSpace(cursor) {
		return GetEvidenceRequest{}, fmt.Errorf("investigate: evidence cursor must be trimmed: %w", ErrContract)
	}
	if sourceType != "" {
		switch invest.EvidenceSourceType(sourceType) {
		case invest.EvidenceSourceDocument, invest.EvidenceSourceField,
			invest.EvidenceSourcePolicy, invest.EvidenceSourceTPA,
			invest.EvidenceSourceProvider, invest.EvidenceSourceRisk:
		default:
			return GetEvidenceRequest{}, fmt.Errorf("investigate: unknown evidence source type %q: %w", sourceType, ErrContract)
		}
	}
	return r, nil
}

// ToRequest converts to the generic envelope.
func (r GetEvidenceRequest) ToRequest() Request {
	out := r.Base.envelope(invest.ToolGetEvidence, r.Limit)
	out.Cursor = r.Cursor
	out.SourceType = r.SourceType
	return out
}

// EvidenceRow is one evidence row pin (IDs + hash + provenance, no bytes).
type EvidenceRow struct {
	EvidenceID  string
	SourceType  string
	SourceID    string
	ContentHash string
}

// GetEvidenceResponse carries up to 50 evidence rows.
type GetEvidenceResponse struct {
	Rows       []EvidenceRow
	Truncated  bool
	NextCursor string
}

// NewGetEvidenceResponse validates the page (cap 50, non-blank pins).
func NewGetEvidenceResponse(rows []EvidenceRow, truncated bool, nextCursor string) (GetEvidenceResponse, error) {
	if len(rows) > MaxRowsGetEvidence {
		return GetEvidenceResponse{}, fmt.Errorf("investigate: evidence page carries %d rows, cap %d: %w", len(rows), MaxRowsGetEvidence, ErrContract)
	}
	for i := range rows {
		w := &rows[i]
		if strings.TrimSpace(w.EvidenceID) == "" || strings.TrimSpace(w.SourceID) == "" {
			return GetEvidenceResponse{}, fmt.Errorf("investigate: evidence[%d] needs evidence and source ids: %w", i, ErrContract)
		}
		switch invest.EvidenceSourceType(w.SourceType) {
		case invest.EvidenceSourceDocument, invest.EvidenceSourceField,
			invest.EvidenceSourcePolicy, invest.EvidenceSourceTPA,
			invest.EvidenceSourceProvider, invest.EvidenceSourceRisk:
		default:
			return GetEvidenceResponse{}, fmt.Errorf("investigate: evidence[%d] has unknown source type %q: %w", i, w.SourceType, ErrContract)
		}
	}
	if nextCursor != "" && nextCursor != strings.TrimSpace(nextCursor) {
		return GetEvidenceResponse{}, fmt.Errorf("investigate: evidence next cursor must be trimmed: %w", ErrContract)
	}
	out := append([]EvidenceRow(nil), rows...)
	return GetEvidenceResponse{out, truncated, nextCursor}, nil
}

// ToResponse converts to the generic envelope (IDs sorted for determinism).
func (r GetEvidenceResponse) ToResponse() Response {
	ids := make([]string, 0, len(r.Rows))
	for i := range r.Rows {
		ids = append(ids, r.Rows[i].EvidenceID)
	}
	slices.Sort(ids)
	return Response{Tool: invest.ToolGetEvidence, RowCount: len(r.Rows), Truncated: r.Truncated, IDs: ids}
}

// ---------------------------------------------------------------------------
// T5 search_evidence — lexical metadata search. Max 20 hits.
// ---------------------------------------------------------------------------

// SearchEvidenceRequest searches field/value/anchor metadata (no vector
// search in MVP). Query is required, capped at invest.SearchMaxQueryLen.
type SearchEvidenceRequest struct {
	Base
	Query string
	Limit int
}

// NewSearchEvidenceRequest validates identity, the required query, and
// the hit cap (limit <= 0 selects MaxRowsSearchEvidence).
func NewSearchEvidenceRequest(tenantID, claimID, investigationID, requestID, query string, limit int) (SearchEvidenceRequest, error) {
	if limit <= 0 {
		limit = MaxRowsSearchEvidence
	}
	r := SearchEvidenceRequest{Base{tenantID, claimID, investigationID, requestID}, query, limit}
	if err := r.Base.validate(); err != nil {
		return SearchEvidenceRequest{}, err
	}
	if strings.TrimSpace(query) == "" || query != strings.TrimSpace(query) {
		return SearchEvidenceRequest{}, fmt.Errorf("investigate: search needs a trimmed non-blank query: %w", ErrContract)
	}
	if len([]rune(query)) > invest.SearchMaxQueryLen {
		return SearchEvidenceRequest{}, fmt.Errorf("investigate: search query exceeds %d runes: %w", invest.SearchMaxQueryLen, ErrContract)
	}
	if limit < 1 || limit > MaxRowsSearchEvidence {
		return SearchEvidenceRequest{}, fmt.Errorf("investigate: search limit %d outside 1..%d: %w", limit, MaxRowsSearchEvidence, ErrContract)
	}
	return r, nil
}

// ToRequest converts to the generic envelope.
func (r SearchEvidenceRequest) ToRequest() Request {
	out := r.Base.envelope(invest.ToolSearchEvidence, r.Limit)
	out.Query = r.Query
	return out
}

// SearchHit is one lexical hit: locators only (evidence + field + anchor),
// never the matched text.
type SearchHit struct {
	EvidenceID string
	FieldKey   string
	Anchor     string
}

// SearchEvidenceResponse carries up to 20 hits.
type SearchEvidenceResponse struct {
	Hits []SearchHit
}

// NewSearchEvidenceResponse validates the hit list (cap 20, locator pins).
func NewSearchEvidenceResponse(hits []SearchHit) (SearchEvidenceResponse, error) {
	if len(hits) > MaxRowsSearchEvidence {
		return SearchEvidenceResponse{}, fmt.Errorf("investigate: search carries %d hits, cap %d: %w", len(hits), MaxRowsSearchEvidence, ErrContract)
	}
	for i := range hits {
		h := &hits[i]
		if strings.TrimSpace(h.EvidenceID) == "" || strings.TrimSpace(h.FieldKey) == "" {
			return SearchEvidenceResponse{}, fmt.Errorf("investigate: search hit[%d] needs evidence id and field key: %w", i, ErrContract)
		}
	}
	out := append([]SearchHit(nil), hits...)
	return SearchEvidenceResponse{out}, nil
}

// ToResponse converts to the generic envelope (IDs sorted for determinism).
func (r SearchEvidenceResponse) ToResponse() Response {
	ids := make([]string, 0, len(r.Hits))
	for i := range r.Hits {
		ids = append(ids, r.Hits[i].EvidenceID)
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	return Response{Tool: invest.ToolSearchEvidence, RowCount: len(r.Hits), IDs: ids}
}

// ---------------------------------------------------------------------------
// T6 get_verification_findings — deterministic replay, 1 row.
// ---------------------------------------------------------------------------

// GetVerificationFindingsRequest re-fetches the stored envelope. No knobs.
type GetVerificationFindingsRequest struct{ Base }

// NewGetVerificationFindingsRequest validates identity.
func NewGetVerificationFindingsRequest(tenantID, claimID, investigationID, requestID string) (GetVerificationFindingsRequest, error) {
	r := GetVerificationFindingsRequest{Base{tenantID, claimID, investigationID, requestID}}
	if err := r.Base.validate(); err != nil {
		return GetVerificationFindingsRequest{}, err
	}
	return r, nil
}

// ToRequest converts to the generic envelope.
func (r GetVerificationFindingsRequest) ToRequest() Request {
	return r.Base.envelope(invest.ToolGetVerificationFindings, MaxRowsGetVerificationFindings)
}

// GetVerificationFindingsResponse carries the replayed exception IDs.
type GetVerificationFindingsResponse struct {
	ExceptionIDs []string
	FindingCount int
}

// NewGetVerificationFindingsResponse validates the replay summary.
func NewGetVerificationFindingsResponse(exceptionIDs []string, findingCount int) (GetVerificationFindingsResponse, error) {
	if findingCount < 0 {
		return GetVerificationFindingsResponse{}, fmt.Errorf("investigate: finding count must be >= 0: %w", ErrContract)
	}
	ids := append([]string(nil), exceptionIDs...)
	slices.Sort(ids)
	if err := checkSortedUnique(ids, "verification exception ids"); err != nil {
		return GetVerificationFindingsResponse{}, err
	}
	return GetVerificationFindingsResponse{ids, findingCount}, nil
}

// ToResponse converts to the generic envelope.
func (r GetVerificationFindingsResponse) ToResponse() Response {
	return Response{Tool: invest.ToolGetVerificationFindings, RowCount: 1, IDs: append([]string(nil), r.ExceptionIDs...)}
}

// ---------------------------------------------------------------------------
// T7 get_external_policy_status — status-only projection, 1 row.
// ---------------------------------------------------------------------------

// GetExternalPolicyStatusRequest reads one status-only projection.
// SubjectID = policy id.
type GetExternalPolicyStatusRequest struct {
	Base
	PolicyID string
}

// NewGetExternalPolicyStatusRequest validates identity plus the subject.
func NewGetExternalPolicyStatusRequest(tenantID, claimID, investigationID, requestID, policyID string) (GetExternalPolicyStatusRequest, error) {
	r := GetExternalPolicyStatusRequest{Base{tenantID, claimID, investigationID, requestID}, policyID}
	if err := r.Base.validate(); err != nil {
		return GetExternalPolicyStatusRequest{}, err
	}
	if strings.TrimSpace(policyID) == "" || policyID != strings.TrimSpace(policyID) {
		return GetExternalPolicyStatusRequest{}, fmt.Errorf("investigate: policy status needs a trimmed policy id: %w", ErrContract)
	}
	return r, nil
}

// ToRequest converts to the generic envelope.
func (r GetExternalPolicyStatusRequest) ToRequest() Request {
	out := r.Base.envelope(invest.ToolGetExternalPolicyStatus, MaxRowsGetExternalPolicyStatus)
	out.SubjectID = r.PolicyID
	return out
}

// GetExternalPolicyStatusResponse carries the status projection pin.
type GetExternalPolicyStatusResponse struct {
	PolicyID    string
	Status      string
	ContentHash string
}

// NewGetExternalPolicyStatusResponse validates the projection pin.
func NewGetExternalPolicyStatusResponse(policyID, status, contentHash string) (GetExternalPolicyStatusResponse, error) {
	if strings.TrimSpace(policyID) == "" || policyID != strings.TrimSpace(policyID) {
		return GetExternalPolicyStatusResponse{}, fmt.Errorf("investigate: policy status needs a trimmed policy id: %w", ErrContract)
	}
	if strings.TrimSpace(status) == "" {
		return GetExternalPolicyStatusResponse{}, fmt.Errorf("investigate: policy status needs a status: %w", ErrContract)
	}
	if strings.TrimSpace(contentHash) == "" {
		return GetExternalPolicyStatusResponse{}, fmt.Errorf("investigate: policy status needs a content hash: %w", ErrContract)
	}
	return GetExternalPolicyStatusResponse{policyID, status, contentHash}, nil
}

// ToResponse converts to the generic envelope.
func (r GetExternalPolicyStatusResponse) ToResponse() Response {
	return Response{Tool: invest.ToolGetExternalPolicyStatus, RowCount: 1, IDs: []string{r.PolicyID}, Hash: r.ContentHash}
}

// ---------------------------------------------------------------------------
// T8 get_tpa_case — prior-claim projections, per-item tenant checks. Max 20.
// ---------------------------------------------------------------------------

// GetTPACaseRequest reads prior-claim projections. SubjectID = case id.
type GetTPACaseRequest struct {
	Base
	CaseID string
	Limit  int
}

// NewGetTPACaseRequest validates identity, subject, and cap.
func NewGetTPACaseRequest(tenantID, claimID, investigationID, requestID, caseID string, limit int) (GetTPACaseRequest, error) {
	if limit <= 0 {
		limit = MaxRowsGetTPACase
	}
	r := GetTPACaseRequest{Base{tenantID, claimID, investigationID, requestID}, caseID, limit}
	if err := r.Base.validate(); err != nil {
		return GetTPACaseRequest{}, err
	}
	if strings.TrimSpace(caseID) == "" || caseID != strings.TrimSpace(caseID) {
		return GetTPACaseRequest{}, fmt.Errorf("investigate: tpa case needs a trimmed case id: %w", ErrContract)
	}
	if limit < 1 || limit > MaxRowsGetTPACase {
		return GetTPACaseRequest{}, fmt.Errorf("investigate: tpa limit %d outside 1..%d: %w", limit, MaxRowsGetTPACase, ErrContract)
	}
	return r, nil
}

// ToRequest converts to the generic envelope.
func (r GetTPACaseRequest) ToRequest() Request {
	out := r.Base.envelope(invest.ToolGetTPACase, r.Limit)
	out.SubjectID = r.CaseID
	return out
}

// TPACaseRef is one prior-claim projection pin. TenantID echoes the
// owning tenant; the implementation MUST reject rows whose tenant
// differs from the request tenant (ErrTenantMismatch, no silent drop).
type TPACaseRef struct {
	CaseID   string
	ClaimID  string
	TenantID string
}

// GetTPACaseResponse carries up to 20 case pins.
type GetTPACaseResponse struct {
	Cases []TPACaseRef
}

// NewGetTPACaseResponse validates the pin list (cap 20, tenant echoes).
func NewGetTPACaseResponse(cases []TPACaseRef) (GetTPACaseResponse, error) {
	if len(cases) > MaxRowsGetTPACase {
		return GetTPACaseResponse{}, fmt.Errorf("investigate: tpa carries %d cases, cap %d: %w", len(cases), MaxRowsGetTPACase, ErrContract)
	}
	for i := range cases {
		c := &cases[i]
		if strings.TrimSpace(c.CaseID) == "" || strings.TrimSpace(c.ClaimID) == "" || strings.TrimSpace(c.TenantID) == "" {
			return GetTPACaseResponse{}, fmt.Errorf("investigate: tpa case[%d] needs case, claim, and tenant ids: %w", i, ErrContract)
		}
	}
	out := append([]TPACaseRef(nil), cases...)
	return GetTPACaseResponse{out}, nil
}

// ToResponse converts to the generic envelope (IDs sorted for determinism).
func (r GetTPACaseResponse) ToResponse() Response {
	ids := make([]string, 0, len(r.Cases))
	for i := range r.Cases {
		ids = append(ids, r.Cases[i].CaseID)
	}
	slices.Sort(ids)
	return Response{Tool: invest.ToolGetTPACase, RowCount: len(r.Cases), IDs: ids}
}

// ---------------------------------------------------------------------------
// T9 get_provider_encounter — one encounter projection, 1 row, doc cap 50.
// ---------------------------------------------------------------------------

// GetProviderEncounterRequest reads one encounter projection.
// SubjectID = encounter id.
type GetProviderEncounterRequest struct {
	Base
	EncounterID string
}

// NewGetProviderEncounterRequest validates identity plus the subject.
func NewGetProviderEncounterRequest(tenantID, claimID, investigationID, requestID, encounterID string) (GetProviderEncounterRequest, error) {
	r := GetProviderEncounterRequest{Base{tenantID, claimID, investigationID, requestID}, encounterID}
	if err := r.Base.validate(); err != nil {
		return GetProviderEncounterRequest{}, err
	}
	if strings.TrimSpace(encounterID) == "" || encounterID != strings.TrimSpace(encounterID) {
		return GetProviderEncounterRequest{}, fmt.Errorf("investigate: provider encounter needs a trimmed encounter id: %w", ErrContract)
	}
	return r, nil
}

// ToRequest converts to the generic envelope.
func (r GetProviderEncounterRequest) ToRequest() Request {
	out := r.Base.envelope(invest.ToolGetProviderEncounter, MaxRowsGetProviderEncounter)
	out.SubjectID = r.EncounterID
	return out
}

// GetProviderEncounterResponse carries one encounter pin plus up to 50
// supporting document refs (IDs only).
type GetProviderEncounterResponse struct {
	EncounterID string
	Status      string
	ContentHash string
	DocIDs      []string
}

// NewGetProviderEncounterResponse validates the pin and the doc-ref cap.
func NewGetProviderEncounterResponse(encounterID, status, contentHash string, docIDs []string) (GetProviderEncounterResponse, error) {
	if strings.TrimSpace(encounterID) == "" || encounterID != strings.TrimSpace(encounterID) {
		return GetProviderEncounterResponse{}, fmt.Errorf("investigate: provider encounter needs a trimmed encounter id: %w", ErrContract)
	}
	if strings.TrimSpace(status) == "" {
		return GetProviderEncounterResponse{}, fmt.Errorf("investigate: provider encounter needs a status: %w", ErrContract)
	}
	if strings.TrimSpace(contentHash) == "" {
		return GetProviderEncounterResponse{}, fmt.Errorf("investigate: provider encounter needs a content hash: %w", ErrContract)
	}
	if len(docIDs) > MaxProviderDocRefs {
		return GetProviderEncounterResponse{}, fmt.Errorf("investigate: provider encounter carries %d doc refs, cap %d: %w", len(docIDs), MaxProviderDocRefs, ErrContract)
	}
	docs := append([]string(nil), docIDs...)
	slices.Sort(docs)
	if err := checkSortedUnique(docs, "provider doc ids"); err != nil {
		return GetProviderEncounterResponse{}, err
	}
	return GetProviderEncounterResponse{encounterID, status, contentHash, docs}, nil
}

// ToResponse converts to the generic envelope (encounter + doc refs cited).
func (r GetProviderEncounterResponse) ToResponse() Response {
	ids := append([]string{r.EncounterID}, r.DocIDs...)
	slices.Sort(ids)
	return Response{Tool: invest.ToolGetProviderEncounter, RowCount: 1, IDs: ids, Hash: r.ContentHash}
}

// ---------------------------------------------------------------------------
// T10 get_risk_signals — risk-signal projections. Max 20.
// ---------------------------------------------------------------------------

// GetRiskSignalsRequest reads risk-signal projections for the claim.
type GetRiskSignalsRequest struct {
	Base
	Limit int
}

// NewGetRiskSignalsRequest validates identity and the cap.
func NewGetRiskSignalsRequest(tenantID, claimID, investigationID, requestID string, limit int) (GetRiskSignalsRequest, error) {
	if limit <= 0 {
		limit = MaxRowsGetRiskSignals
	}
	r := GetRiskSignalsRequest{Base{tenantID, claimID, investigationID, requestID}, limit}
	if err := r.Base.validate(); err != nil {
		return GetRiskSignalsRequest{}, err
	}
	if limit < 1 || limit > MaxRowsGetRiskSignals {
		return GetRiskSignalsRequest{}, fmt.Errorf("investigate: risk limit %d outside 1..%d: %w", limit, MaxRowsGetRiskSignals, ErrContract)
	}
	return r, nil
}

// ToRequest converts to the generic envelope.
func (r GetRiskSignalsRequest) ToRequest() Request {
	return r.Base.envelope(invest.ToolGetRiskSignals, r.Limit)
}

// RiskSignalRef is one risk-signal pin. TenantID echoes the owning
// tenant; cross-tenant rows are rejected, never filtered silently.
type RiskSignalRef struct {
	SignalID string
	Code     string
	TenantID string
}

// GetRiskSignalsResponse carries up to 20 signal pins.
type GetRiskSignalsResponse struct {
	Signals []RiskSignalRef
}

// NewGetRiskSignalsResponse validates the pin list (cap 20, tenant echoes).
func NewGetRiskSignalsResponse(signals []RiskSignalRef) (GetRiskSignalsResponse, error) {
	if len(signals) > MaxRowsGetRiskSignals {
		return GetRiskSignalsResponse{}, fmt.Errorf("investigate: risk carries %d signals, cap %d: %w", len(signals), MaxRowsGetRiskSignals, ErrContract)
	}
	for i := range signals {
		s := &signals[i]
		if strings.TrimSpace(s.SignalID) == "" || strings.TrimSpace(s.Code) == "" || strings.TrimSpace(s.TenantID) == "" {
			return GetRiskSignalsResponse{}, fmt.Errorf("investigate: risk signal[%d] needs signal, code, and tenant ids: %w", i, ErrContract)
		}
	}
	out := append([]RiskSignalRef(nil), signals...)
	return GetRiskSignalsResponse{out}, nil
}

// ToResponse converts to the generic envelope (IDs sorted for determinism).
func (r GetRiskSignalsResponse) ToResponse() Response {
	ids := make([]string, 0, len(r.Signals))
	for i := range r.Signals {
		ids = append(ids, r.Signals[i].SignalID)
	}
	slices.Sort(ids)
	return Response{Tool: invest.ToolGetRiskSignals, RowCount: len(r.Signals), IDs: ids}
}

// ---------------------------------------------------------------------------
// T11 create_investigation_report — the sole writer (write-once per
// InvestigationID; replay returns the stored report). 1 row.
// ---------------------------------------------------------------------------

// CreateReportRequest stores one canonical report. Payload is the canonical
// report bytes (encoding/json v1, no floats); ReportHash is its sha256 hex.
// The sibling store MUST recompute sha256 over Payload and compare it to
// ReportHash before insert, and MUST set investigation_reports.id to the
// InvestigationID so PK and UNIQUE(investigation_id) coincide.
type CreateReportRequest struct {
	Base
	ExceptionID string
	ReportJSON  []byte
	ReportHash  string
}

// NewCreateReportRequest validates identity, the ex- ID shape, the JSON
// payload (non-empty, within MaxReportBytes, valid JSON), and the 64-hex
// report hash.
func NewCreateReportRequest(tenantID, claimID, investigationID, requestID, exceptionID string, reportJSON []byte, reportHash string) (CreateReportRequest, error) {
	r := CreateReportRequest{Base{tenantID, claimID, investigationID, requestID}, exceptionID, reportJSON, reportHash}
	if err := r.Base.validate(); err != nil {
		return CreateReportRequest{}, err
	}
	if err := invest.ValidateID(invest.ExceptionIDPrefix, exceptionID); err != nil {
		return CreateReportRequest{}, fmt.Errorf("investigate: report: %v: %w", err, ErrContract)
	}
	if len(reportJSON) == 0 {
		return CreateReportRequest{}, fmt.Errorf("investigate: report needs non-empty canonical bytes: %w", ErrContract)
	}
	if len(reportJSON) > MaxReportBytes {
		return CreateReportRequest{}, fmt.Errorf("investigate: report %d bytes exceeds %d: %w", len(reportJSON), MaxReportBytes, ErrContract)
	}
	if !json.Valid(reportJSON) {
		return CreateReportRequest{}, fmt.Errorf("investigate: report bytes are not valid JSON: %w", ErrContract)
	}
	if err := checkSHA256(reportHash, "report hash"); err != nil {
		return CreateReportRequest{}, err
	}
	return r, nil
}

// ToRequest converts to the generic envelope (hash + payload ride along;
// the executor and audit log carry only hash and size, never values).
func (r CreateReportRequest) ToRequest() Request {
	out := r.Base.envelope(invest.ToolCreateInvestigationReport, MaxRowsCreateReport)
	out.SubjectID = r.ExceptionID
	out.Hash = r.ReportHash
	out.Payload = r.ReportJSON
	return out
}

// CreateReportResponse is the write-once receipt: investigation ID,
// report hash, and stored version (>= 1).
type CreateReportResponse struct {
	InvestigationID string
	ReportHash      string
	Version         int
}

// NewCreateReportResponse validates the receipt.
func NewCreateReportResponse(investigationID, reportHash string, version int) (CreateReportResponse, error) {
	if err := invest.ValidateID(invest.InvestigationIDPrefix, investigationID); err != nil {
		return CreateReportResponse{}, fmt.Errorf("investigate: report receipt: %v: %w", err, ErrContract)
	}
	if err := checkSHA256(reportHash, "report hash"); err != nil {
		return CreateReportResponse{}, err
	}
	if version < 1 {
		return CreateReportResponse{}, fmt.Errorf("investigate: report receipt version must be >= 1: %w", ErrContract)
	}
	return CreateReportResponse{investigationID, reportHash, version}, nil
}

// ToResponse converts to the generic envelope.
func (r CreateReportResponse) ToResponse() Response {
	return Response{Tool: invest.ToolCreateInvestigationReport, RowCount: 1, IDs: []string{r.InvestigationID}, Hash: r.ReportHash}
}
