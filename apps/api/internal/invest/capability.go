package invest

import (
	"fmt"
	"slices"
	"strings"
)

// ToolName is one allowlisted investigation capability. The set is closed:
// the executor must reject any name outside this block, so the agent can
// never request an undeclared capability (no arbitrary HTTP/SQL/shell,
// no blob-byte reads, no state mutators).
//
// The 11 names bind spec §3.2, which maps the 9 Build Plan §9 capabilities
// onto existing backends and adds the two remaining port-backed reads
// (provider encounters, risk signals) as separately named tools so traces
// distinguish them. No tool is implemented here.
type ToolName string

// Allowlisted investigation tools (T1-T11 per spec §3.2).
const (
	// ToolGetClaim reads one claim header (status, version, reference,
	// amount paise, dates). Backend: claims store. 1 row, read-only.
	ToolGetClaim ToolName = "get_claim"
	// ToolGetPolicyContext reads a policy projection + pins an evidence
	// row on first fetch. Backend: PolicyPort. 3s timeout.
	ToolGetPolicyContext ToolName = "get_policy_context"
	// ToolGetDocuments lists document metadata (id, type, sha256,
	// status), never bytes. Backend: documents repository. Max 50 rows.
	ToolGetDocuments ToolName = "get_documents"
	// ToolGetEvidence pages evidence rows with IDs + hashes +
	// provenance. Backend: evidence tables. Max 50/page, IDs-only.
	ToolGetEvidence ToolName = "get_evidence"
	// ToolSearchEvidence is lexical search over field/value/anchor
	// metadata (no vector search in MVP). Max 20 hits, query cap
	// SearchMaxQueryLen.
	ToolSearchEvidence ToolName = "search_evidence"
	// ToolGetVerificationFindings re-fetches the stored envelope
	// (deterministic replay, no model). Backend: verify/verifywrap.
	ToolGetVerificationFindings ToolName = "get_verification_findings"
	// ToolGetExternalPolicyStatus is the status-only policy projection
	// (separate name so traces distinguish status checks from full
	// context). Backend: PolicyPort. 3s timeout.
	ToolGetExternalPolicyStatus ToolName = "get_external_policy_status"
	// ToolGetTPACase reads prior-claim projections with per-item tenant
	// checks. Backend: ClaimsPort. Max 20.
	ToolGetTPACase ToolName = "get_tpa_case"
	// ToolGetProviderEncounter reads an encounter projection.
	// Backend: ProviderPort. 3s timeout.
	ToolGetProviderEncounter ToolName = "get_provider_encounter"
	// ToolGetRiskSignals reads risk-signal projections with per-item
	// tenant checks. Backend: RiskPort. Max 20.
	ToolGetRiskSignals ToolName = "get_risk_signals"
	// ToolCreateInvestigationReport is the sole writer: it validates and
	// stores one report per InvestigationID (write-once, replay returns
	// stored). Backend: NEW report store (gap G2, future migration).
	ToolCreateInvestigationReport ToolName = "create_investigation_report"
)

// allowlist is the closed tool set in canonical (sorted) order.
var allowlist = []ToolName{
	ToolCreateInvestigationReport,
	ToolGetClaim,
	ToolGetDocuments,
	ToolGetEvidence,
	ToolGetExternalPolicyStatus,
	ToolGetPolicyContext,
	ToolGetProviderEncounter,
	ToolGetRiskSignals,
	ToolGetTPACase,
	ToolGetVerificationFindings,
	ToolSearchEvidence,
}

// Allowlist returns the closed tool set in canonical order. Callers must
// not cache-and-extend it; IsAllowlisted is the only membership test.
func Allowlist() []ToolName {
	return append([]ToolName(nil), allowlist...)
}

// IsAllowlisted reports whether name is one of the 11 declared tools.
func IsAllowlisted(name ToolName) bool {
	return slices.Contains(allowlist, name)
}

// Tool timeout precedent: 3s default per ADR-004 / http adapter behavior.
const defaultToolTimeoutMs = int64(3000)

// SearchMaxQueryLen caps T5 queries (spec §3.2: 200 runes).
const SearchMaxQueryLen = 200

// ToolBound declares one tool's typed execution bounds. Bounds are data,
// not behavior: the future executor enforces them, the model never sets
// them. Every tool is read-only against authoritative state except
// create_investigation_report, which appends new report rows only
// (AppendOnly marks that distinction).
type ToolBound struct {
	Name       ToolName
	TimeoutMs  int64
	MaxRows    int
	AppendOnly bool
}

// toolBounds is the per-tool bound table (spec §3.2 Bounds column).
var toolBounds = map[ToolName]ToolBound{
	ToolGetClaim:                {Name: ToolGetClaim, TimeoutMs: defaultToolTimeoutMs, MaxRows: 1},
	ToolGetPolicyContext:        {Name: ToolGetPolicyContext, TimeoutMs: defaultToolTimeoutMs, MaxRows: 1},
	ToolGetDocuments:            {Name: ToolGetDocuments, TimeoutMs: defaultToolTimeoutMs, MaxRows: 50},
	ToolGetEvidence:             {Name: ToolGetEvidence, TimeoutMs: defaultToolTimeoutMs, MaxRows: 50},
	ToolSearchEvidence:          {Name: ToolSearchEvidence, TimeoutMs: defaultToolTimeoutMs, MaxRows: 20},
	ToolGetVerificationFindings: {Name: ToolGetVerificationFindings, TimeoutMs: defaultToolTimeoutMs, MaxRows: 1},
	ToolGetExternalPolicyStatus: {Name: ToolGetExternalPolicyStatus, TimeoutMs: defaultToolTimeoutMs, MaxRows: 1},
	ToolGetTPACase:              {Name: ToolGetTPACase, TimeoutMs: defaultToolTimeoutMs, MaxRows: 20},
	ToolGetProviderEncounter:    {Name: ToolGetProviderEncounter, TimeoutMs: defaultToolTimeoutMs, MaxRows: 1},
	ToolGetRiskSignals:          {Name: ToolGetRiskSignals, TimeoutMs: defaultToolTimeoutMs, MaxRows: 20},
	ToolCreateInvestigationReport: {
		Name: ToolCreateInvestigationReport, TimeoutMs: defaultToolTimeoutMs, MaxRows: 1, AppendOnly: true,
	},
}

// DefaultBound returns the declared bounds for an allowlisted tool.
// Unknown names are rejected (fail closed); there are no default bounds
// for undeclared capabilities.
func DefaultBound(name ToolName) (ToolBound, error) {
	b, ok := toolBounds[name]
	if !ok {
		return ToolBound{}, fmt.Errorf("invest: unknown tool %q (not in 11-tool allowlist)", string(name))
	}
	return b, nil
}

// validateToolName rejects blank names and names outside the allowlist.
func validateToolName(name ToolName) error {
	if strings.TrimSpace(string(name)) == "" {
		return fmt.Errorf("invest: blank tool name")
	}
	if !IsAllowlisted(name) {
		return fmt.Errorf("invest: tool %q not in 11-tool allowlist", string(name))
	}
	return nil
}
