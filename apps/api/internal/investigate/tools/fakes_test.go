// Shared fakes and fixtures for the tools package tests (issue #54).
//
// Every fake here is scripted in-memory: success values plus one injected
// error, propagated untouched so errors.Is classification survives exactly
// as with the production adapters. The fake pinner records its call so
// tests can prove provenance (tenant/claim echo, source type/id, and that
// the pinned bytes are the full pre-truncation payload).
package tools

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"claimops-api/internal/invest"
	"claimops-api/internal/investigate"
	"claimops-api/internal/ports"
)

const (
	toolsTenant = "tnt-54-tools"
	toolsClaim  = "clm-54-tools"
	toolsInv    = "inv-0123456789abcdef0123456789abcdef"
	toolsReq    = "req-54-tools"
	toolsEx     = "ex-0123456789abcdef0123456789abcdef"
)

// requireToolsSentinel fails unless err wraps sentinel via errors.Is.
func requireToolsSentinel(t *testing.T, err error, sentinel error) {
	t.Helper()
	if !errors.Is(err, sentinel) {
		t.Fatalf("want error wrapping %v, got %v", sentinel, err)
	}
}

// toolsErr returns a ports-taxonomy error for fake injection.
func toolsErr(sentinel error, what string) error {
	return fmt.Errorf("%w: fake %s", sentinel, what)
}

// ---------------------------------------------------------------------------
// Readers.
// ---------------------------------------------------------------------------

type fakeClaimReader struct {
	header    investigate.ClaimHeader
	err       error
	gotTenant string
	gotClaim  string
	calls     int
}

func (f *fakeClaimReader) LoadClaim(_ context.Context, tenant, claim string) (investigate.ClaimHeader, error) {
	f.gotTenant, f.gotClaim = tenant, claim
	f.calls++
	if f.err != nil {
		return investigate.ClaimHeader{}, f.err
	}
	return f.header, nil
}

type fakeDocReader struct {
	page investigate.DocumentPage
	err  error
	got  struct {
		tenant, claim string
		limit         int
		cursor        string
	}
}

func (f *fakeDocReader) ListDocuments(_ context.Context, tenant, claim string, limit int, cursor string) (investigate.DocumentPage, error) {
	f.got.tenant, f.got.claim, f.got.limit, f.got.cursor = tenant, claim, limit, cursor
	if f.err != nil {
		return investigate.DocumentPage{}, f.err
	}
	return f.page, nil
}

type fakeEvidenceReader struct {
	page investigate.EvidencePage
	hits []investigate.EvidenceHit
	err  error
	got  struct {
		tenant, claim string
		limit         int
		cursor        string
		source        string
		query         string
	}
}

func (f *fakeEvidenceReader) ListEvidence(_ context.Context, tenant, claim string, limit int, cursor, source string) (investigate.EvidencePage, error) {
	f.got.tenant, f.got.claim, f.got.limit, f.got.cursor, f.got.source = tenant, claim, limit, cursor, source
	if f.err != nil {
		return investigate.EvidencePage{}, f.err
	}
	return f.page, nil
}

func (f *fakeEvidenceReader) SearchEvidence(_ context.Context, tenant, claim, query string, limit int) ([]investigate.EvidenceHit, error) {
	f.got.tenant, f.got.claim, f.got.query, f.got.limit = tenant, claim, query, limit
	if f.err != nil {
		return nil, f.err
	}
	return f.hits, nil
}

// ---------------------------------------------------------------------------
// Ports.
// ---------------------------------------------------------------------------

type fakePolicyPort struct {
	info ports.PolicyInfo
	err  error
}

func (f *fakePolicyPort) GetPolicy(_ context.Context, _, _ string) (ports.PolicyInfo, error) {
	if f.err != nil {
		return ports.PolicyInfo{}, f.err
	}
	return f.info, nil
}

type fakeClaimsPort struct {
	items []ports.PriorClaim
	err   error
}

func (f *fakeClaimsPort) ListClaims(_ context.Context, _, _ string) ([]ports.PriorClaim, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.items, nil
}

type fakeProviderPort struct {
	encounter ports.Encounter
	err       error
}

func (f *fakeProviderPort) GetEncounter(_ context.Context, _, _ string) (ports.Encounter, error) {
	if f.err != nil {
		return ports.Encounter{}, f.err
	}
	return f.encounter, nil
}

type fakeRiskPort struct {
	items []ports.RiskSignal
	err   error
}

func (f *fakeRiskPort) GetSignals(_ context.Context, _, _ string) ([]ports.RiskSignal, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.items, nil
}

// ---------------------------------------------------------------------------
// Pinner + report store.
// ---------------------------------------------------------------------------

type fakePinner struct {
	id                  string
	err                 error
	calls               int
	gotTenant, gotClaim string
	gotType, gotSource  string
	gotBytes            []byte
}

func (f *fakePinner) Pin(_ context.Context, tenant, claim, sourceType, sourceID string, b []byte) (string, error) {
	f.calls++
	f.gotTenant, f.gotClaim, f.gotType, f.gotSource = tenant, claim, sourceType, sourceID
	f.gotBytes = append([]byte(nil), b...)
	if f.err != nil {
		return "", f.err
	}
	return f.id, nil
}

type fakeReportStore struct {
	rows map[string]ReportRow
	err  error
}

func newFakeReportStore() *fakeReportStore { return &fakeReportStore{rows: map[string]ReportRow{}} }

func (f *fakeReportStore) Insert(_ context.Context, row ReportRow) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	if _, dup := f.rows[row.ID]; dup {
		return true, nil
	}
	f.rows[row.ID] = row
	return false, nil
}

// staticResolver maps evidence IDs to (tenant, claim); unknown IDs fail.
func staticResolver(m map[string][2]string) Resolver {
	return func(_ context.Context, id string) (string, string, error) {
		if v, ok := m[id]; ok {
			return v[0], v[1], nil
		}
		return "", "", fmt.Errorf("fake resolver: unknown evidence %q", id)
	}
}

// ---------------------------------------------------------------------------
// Fixtures.
// ---------------------------------------------------------------------------

func validPolicyInfo(tenant string) ports.PolicyInfo {
	return ports.PolicyInfo{
		PolicyID:        "pol-54-01",
		TenantID:        tenant,
		Status:          "ACTIVE",
		EffectiveFrom:   time.Date(2025, time.January, 1, 0, 0, 0, 0, time.UTC),
		EffectiveTo:     time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC),
		SumInsuredPaise: 500000,
		AvailablePaise:  400000,
		SourceRef:       "src-54-01",
	}
}

func validEncounter(id string) ports.Encounter {
	return ports.Encounter{
		EncounterID: id,
		PatientRef:  "pat-54",
		HospitalID:  "hosp-54",
		AdmissionAt: time.Date(2025, time.May, 1, 10, 0, 0, 0, time.UTC),
		DischargeAt: time.Date(2025, time.May, 3, 10, 0, 0, 0, time.UTC),
		Diagnosis:   []ports.Diagnosis{{Code: "A01", Description: "test diagnosis"}},
		Documents:   []ports.ProviderDoc{{DocumentID: "doc-54-1", Type: "bill", SHA256: "ab"}},
	}
}

func validSignals(tenant string) []ports.RiskSignal {
	return []ports.RiskSignal{
		{SignalType: "RECENT_SIMILAR_CLAIM", TenantID: tenant, Severity: "HIGH", SourceRef: "src-r1"},
		{SignalType: "ROUND_AMOUNT", TenantID: tenant, Severity: "LOW", SourceRef: "src-r2"},
	}
}

func validPriorClaims(tenant string) []ports.PriorClaim {
	return []ports.PriorClaim{
		{ClaimID: "clm-old-01", TenantID: tenant, Status: "SETTLED", ApprovedPaise: 12000},
		{ClaimID: "clm-old-02", TenantID: tenant, Status: "REJECTED", ApprovedPaise: 0},
	}
}

// validReportException is the minimal invest envelope that passes
// invest.Validate: one R1 finding, one document evidence ref, a
// single-tool scope, no unresolved/agreed/missing entries.
func validReportException() invest.UnresolvedException {
	return invest.UnresolvedException{
		TenantID:        toolsTenant,
		ClaimID:         toolsClaim,
		ExceptionID:     toolsEx,
		InvestigationID: toolsInv,
		RuleFindings: []invest.RuleFinding{{
			Code:           invest.RulePolicyNumberConflict,
			Severity:       invest.SeverityHigh,
			Message:        "policy number conflict",
			EvidenceIDs:    []string{"ev-01"},
			AffectedFields: []string{"policy_number"},
		}},
		Scope: invest.ScopeConstraints{
			TenantID:     toolsTenant,
			ClaimID:      toolsClaim,
			AllowTools:   []invest.ToolName{invest.ToolGetClaim},
			MaxToolCalls: 5,
			DeadlineMs:   5000,
			RequestID:    toolsReq,
		},
		EvidenceRefs: []invest.EvidenceRef{{
			EvidenceID: "ev-01",
			SourceType: invest.EvidenceSourceDocument,
			SourceID:   "doc-54-1",
			TenantID:   toolsTenant,
			ClaimID:    toolsClaim,
			DocumentID: "doc-54-1",
			Page:       1,
		}},
	}
}

func validReportRequest(exc invest.UnresolvedException) ReportRequest {
	return ReportRequest{
		TenantID:        toolsTenant,
		ClaimID:         toolsClaim,
		InvestigationID: toolsInv,
		RequestID:       toolsReq,
		ExceptionID:     toolsEx,
		Exception:       exc,
		Hypotheses: []invest.Hypothesis{{
			ID: "hyp-01", Statement: "bill inflated", Falsifier: "itemized bill matches",
			Status: invest.HypothesisOpen, EvidenceIDs: []string{"ev-01"},
		}},
		Findings: []invest.Finding{{
			ID: "find-01", HypothesisID: "hyp-01", Summary: "total exceeds bill",
			EvidenceIDs: []string{"ev-01"},
		}},
		Recommendations: []invest.Recommendation{{
			Action: invest.RecommendRequestEvidence, Rationale: "need itemized bill",
			FindingIDs: []string{"find-01"},
		}},
		MissingEvidence: []invest.MissingItem{
			{Kind: invest.MissingField, Key: "bill_lines", Detail: "line amounts unavailable"},
		},
	}
}
