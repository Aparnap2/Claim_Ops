package invest

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"

	"claimops-api/internal/assemble"
	"claimops-api/internal/claims"
	"claimops-api/internal/extract"
	"claimops-api/internal/verify"
	"claimops-api/internal/verifywrap"
)

// RuleCode is a verify R1-R10 exception code. Values ARE the verify
// constants (G3: one taxonomy, referenced not copied). This package adds
// no codes of its own; unknown future codes are rejected until the
// projection table below is extended in the same change that adds the
// rule (fail closed, no wildcard).
type RuleCode string

// Rule codes, one per verify rule R1-R10, aliasing the verify constants so
// a rename upstream is a compile break here, never a silent fork.
const (
	RulePolicyNumberConflict    RuleCode = RuleCode(verify.CodePolicyNumberConflict)
	RulePatientIdentityConflict RuleCode = RuleCode(verify.CodePatientIdentityConflict)
	RuleInvalidDateRange        RuleCode = RuleCode(verify.CodeInvalidDateRange)
	RulePolicyNotActive         RuleCode = RuleCode(verify.CodePolicyNotActive)
	RuleAmountReconciliation    RuleCode = RuleCode(verify.CodeAmountReconciliationFailure)
	RuleClaimExceedsBill        RuleCode = RuleCode(verify.CodeClaimExceedsBill)
	RuleDuplicateDocument       RuleCode = RuleCode(verify.CodeDuplicateDocument)
	RuleMissingRequiredDocument RuleCode = RuleCode(verify.CodeMissingRequiredDocument)
	RuleExternalPolicyMismatch  RuleCode = RuleCode(verify.CodeExternalPolicyMismatch)
	RuleDateConflict            RuleCode = RuleCode(verify.CodeDateConflict)
)

// ruleRank fixes R1-R10 emission order for the decode-time order check.
// verify.Verify emits in this order; the envelope must preserve it.
var ruleRank = map[RuleCode]int{
	RulePolicyNumberConflict:    1,
	RulePatientIdentityConflict: 2,
	RuleInvalidDateRange:        3,
	RulePolicyNotActive:         4,
	RuleAmountReconciliation:    5,
	RuleClaimExceedsBill:        6,
	RuleDuplicateDocument:       7,
	RuleMissingRequiredDocument: 8,
	RuleExternalPolicyMismatch:  9,
	RuleDateConflict:            10,
}

// IsKnownRuleCode reports whether code is one of the 10 verify R1-R10
// codes. Legacy SQL exceptions.type strings (MISSING_DOCUMENT,
// ENTITY_MISMATCH, ...) are NOT known codes (G3 separation).
func IsKnownRuleCode(code string) bool {
	_, ok := ruleRank[RuleCode(code)]
	return ok
}

// Severity is a verify severity level. Values are the verify constants;
// the engine emits HIGH/MEDIUM today, LOW is accepted because verify
// defines it.
type Severity string

// Severity levels, aliasing verify so the taxonomy stays single-sourced.
const (
	SeverityHigh   Severity = Severity(verify.SeverityHigh)
	SeverityMedium Severity = Severity(verify.SeverityMedium)
	SeverityLow    Severity = Severity(verify.SeverityLow)
)

// AffectedFields projects a rule code to the canonical field keys the
// rule concerns (spec §1.3 closed table). The agent must never infer this
// mapping itself. Unknown codes fail closed. Results are freshly allocated
// and sorted; DUPLICATE_DOCUMENT and MISSING_REQUIRED_DOCUMENT project to
// no fields (doc identity and missing docs travel via EvidenceIDs and
// MissingEvidence respectively).
func AffectedFields(code RuleCode) ([]string, error) {
	switch code {
	case RulePolicyNumberConflict:
		return []string{"policy_number"}, nil
	case RulePatientIdentityConflict:
		return []string{"patient_name"}, nil
	case RuleInvalidDateRange:
		return []string{"admission_date", "discharge_date"}, nil
	case RulePolicyNotActive:
		return []string{"policy_number"}, nil
	case RuleAmountReconciliation:
		return []string{"total_amount_paise"}, nil
	case RuleClaimExceedsBill:
		return []string{"total_amount_paise"}, nil
	case RuleDuplicateDocument:
		return []string{}, nil
	case RuleMissingRequiredDocument:
		return []string{}, nil
	case RuleExternalPolicyMismatch:
		return []string{"policy_number"}, nil
	case RuleDateConflict:
		return []string{"admission_date"}, nil
	default:
		return nil, fmt.Errorf("invest: unknown rule code %q (fail closed)", string(code))
	}
}

// RuleFinding is one verify.Result exception carried verbatim: code,
// severity, and evidence IDs exactly as verify emitted them (finding order
// is R1-R10 emission order, never re-sorted). Message is human display
// only and must never be parsed for decisions. AffectedFields is the
// deterministic projection, not an agent assertion.
type RuleFinding struct {
	Code           RuleCode `json:"code"`
	Severity       Severity `json:"severity"`
	Message        string   `json:"message"`
	EvidenceIDs    []string `json:"evidence_ids"`
	AffectedFields []string `json:"affected_fields"`
}

// FieldSourceView is one voting observation behind a conflict, with its
// provenance flattened and its stored-evidence row resolved. It carries no
// confidence float: the presence of the entry IS the signal.
type FieldSourceView struct {
	Value            string `json:"value"`
	Normalized       string `json:"normalized"`
	EvidenceID       string `json:"evidence_id"`
	Extractor        string `json:"extractor"`
	ExtractorVersion string `json:"extractor_version"`
	DocType          string `json:"doc_type"`
	DocumentID       string `json:"document_id"`
	Page             int    `json:"page"`
	BlockID          string `json:"block_id"`
}

// CandidateView is one value of a MULTI_CANDIDATE review item, in artifact
// encounter order (never re-sorted).
type CandidateView struct {
	Value      string `json:"value"`
	Normalized string `json:"normalized"`
	EvidenceID string `json:"evidence_id"`
}

// ReviewItemView is one non-voting observation requiring human review.
// Status reuses extract.FieldStatus and is restricted to AMBIGUOUS and
// MULTI_CANDIDATE. Candidates keep encounter order.
type ReviewItemView struct {
	DocumentID string              `json:"document_id"`
	DocType    string              `json:"doc_type"`
	Status     extract.FieldStatus `json:"status"`
	Value      string              `json:"value"`
	Candidates []CandidateView     `json:"candidates"`
	EvidenceID string              `json:"evidence_id"`
}

// UnresolvedField is one assembled field that never became typed input.
// Exactly one of Conflict / Review is set, mirroring the
// verifywrap.Unresolved invariant: CONFLICT carries the conflict entry,
// NEEDS_REVIEW carries the review items. Status reuses assemble.Status
// and is restricted to those two values.
type UnresolvedField struct {
	Key      string           `json:"key"`
	Status   assemble.Status  `json:"status"`
	Conflict *ConflictView    `json:"conflict,omitempty"`
	Review   []ReviewItemView `json:"review,omitempty"`
}

// ConflictView is the deterministic disagreement record for one key:
// sorted distinct fold values plus every voting source in canonical order.
type ConflictView struct {
	Distinct []string          `json:"distinct"`
	Sources  []FieldSourceView `json:"sources"`
}

// AgreedField is agreed context the investigator may read but must not
// re-judge (display + retrieval scoping only). There is deliberately NO
// AgreedRaw field: AgreedRaw is display-only and order-unstable until #50
// lands, so it cannot cross this boundary even by accident. No confidence.
type AgreedField struct {
	Key            string   `json:"key"`
	Agreed         string   `json:"agreed"`
	SourceDocTypes []string `json:"source_doc_types"`
	EvidenceIDs    []string `json:"evidence_ids"`
}

// MissingKind is the closed kind set for derived open questions.
type MissingKind string

// Missing evidence kinds (closed; gap G12 extends Kind only together with
// the sufficiency-gate rules that need the new kind).
const (
	// MissingRequiredDocument names an absent required DocType
	// (Key is the DocType, re-derived from DocsPresent, never parsed
	// from a message per gap G6).
	MissingRequiredDocument MissingKind = "required_document"
	// MissingField names a field key with no usable value (Key is the
	// canonical field key).
	MissingField MissingKind = "field"
	// MissingExternal names an upstream source with no pinned evidence
	// (Key is one of policy|tpa|provider|risk).
	MissingExternal MissingKind = "external"
)

// MissingItem is one open question the deterministic side already knows is
// open. Detail is required display-only context (non-blank, enforced in
// Validate): every derived item carries its derivation note (R8, F11b,
// rule code). The investigator may EXTEND this
// list in its output (additive only) but never remove entries.
type MissingItem struct {
	Kind   MissingKind `json:"kind"`
	Key    string      `json:"key"`
	Detail string      `json:"detail"`
}

// EvidenceSourceType is the closed source-type set for provenance refs:
// the four upstream pin types plus the two artifact-observation types.
type EvidenceSourceType string

// Evidence source types (closed).
const (
	EvidenceSourceDocument EvidenceSourceType = "document"
	EvidenceSourceField    EvidenceSourceType = "field"
	EvidenceSourcePolicy   EvidenceSourceType = "policy"
	EvidenceSourceTPA      EvidenceSourceType = "tpa"
	EvidenceSourceProvider EvidenceSourceType = "provider"
	EvidenceSourceRisk     EvidenceSourceType = "risk"
)

// EvidenceRef cites one pinned provenance row by its stable row ID (gap
// G5: row IDs are the citation identity; content hashes ride along where
// the owning store keeps them). Tenant and ClaimID echo the envelope and
// must match it. Document/field rows carry document provenance
// (DocumentID + 1-based Page); upstream rows carry ContentHash instead and
// must not carry document provenance. No bytes, no OCR dumps, no raw
// upstream JSON — IDs, hashes, and agreed strings only.
type EvidenceRef struct {
	EvidenceID  string             `json:"evidence_id"`
	SourceType  EvidenceSourceType `json:"source_type"`
	SourceID    string             `json:"source_id"`
	ContentHash string             `json:"content_hash,omitempty"`
	TenantID    string             `json:"tenant_id"`
	ClaimID     string             `json:"claim_id"`
	DocumentID  string             `json:"document_id,omitempty"`
	Page        int                `json:"page,omitempty"`
	BlockID     string             `json:"block_id,omitempty"`
}

// ScopeConstraints is the closed world for one investigation: tenant/claim
// echo (tools reject cross-tenant reads), the per-case least-privilege
// tool subset, call/time budgets enforced by the executor (never requested
// by the model), and the propagated request ID. Together with the
// allowlist this is the capability contract: the agent cannot request an
// undeclared capability because undeclared names fail validation here.
type ScopeConstraints struct {
	TenantID     string     `json:"tenant_id"`
	ClaimID      string     `json:"claim_id"`
	AllowTools   []ToolName `json:"allow_tools"`
	MaxToolCalls int        `json:"max_tool_calls"`
	DeadlineMs   int64      `json:"deadline_ms"`
	RequestID    string     `json:"request_id"`
}

// UnresolvedException unites verify.Result exceptions and the
// verifywrap.Unresolved channel into the sole investigation input:
// the rule failure AND the ambiguous fields AND the agreed context,
// with derived open questions, closed scope, and citable provenance.
type UnresolvedException struct {
	TenantID        string            `json:"tenant_id"`
	ClaimID         string            `json:"claim_id"`
	ExceptionID     string            `json:"exception_id"`
	InvestigationID string            `json:"investigation_id"`
	RuleFindings    []RuleFinding     `json:"rule_findings"`
	Unresolved      []UnresolvedField `json:"unresolved"`
	AgreedSnapshot  []AgreedField     `json:"agreed_snapshot"`
	MissingEvidence []MissingItem     `json:"missing_evidence"`
	Scope           ScopeConstraints  `json:"scope"`
	EvidenceRefs    []EvidenceRef     `json:"evidence_refs"`
}

// ID prefixes for server-side minted identifiers.
const (
	// ExceptionIDPrefix marks deterministic-side exception identifiers.
	ExceptionIDPrefix = "ex-"
	// InvestigationIDPrefix marks investigation idempotency keys.
	InvestigationIDPrefix = "inv-"
)

// NewExceptionID mints an "ex-" + 32 hex identifier. Entropy failure is an
// explicit error (mirroring evidence.newFieldID): a silent deterministic
// fallback would risk idempotency-key collision, which is worse than a
// retried mint.
func NewExceptionID() (string, error) {
	return newPrefixedID(ExceptionIDPrefix)
}

// NewInvestigationID mints an "inv-" + 32 hex identifier, the idempotency
// key for report submission (double-submit returns the stored report).
func NewInvestigationID() (string, error) {
	return newPrefixedID(InvestigationIDPrefix)
}

// newPrefixedID mints prefix + 32 hex chars from crypto entropy.
func newPrefixedID(prefix string) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("invest: id entropy: %w", err)
	}
	return prefix + hex.EncodeToString(b[:]), nil
}

// ValidateID checks the minted-ID shape: required prefix plus exactly 32
// hex chars (16 entropy bytes). No trimming, no padding acceptance, no
// variable length: padded, short, or odd-length bodies are rejected.
func ValidateID(prefix, id string) error {
	if !strings.HasPrefix(id, prefix) {
		return fmt.Errorf("invest: id %q missing prefix %q", id, prefix)
	}
	rest := strings.TrimPrefix(id, prefix)
	if len(rest) != 32 {
		return fmt.Errorf("invest: id %q body must be exactly 32 hex chars", id)
	}
	if _, err := hex.DecodeString(rest); err != nil {
		return fmt.Errorf("invest: id %q body is not hex: %v", id, err)
	}
	return nil
}

// BuildParams assembles everything the deterministic side owns for one
// envelope build. IDs are caller-minted (NewExceptionID /
// NewInvestigationID) so Build stays pure: same params, same envelope,
// same bytes. DocsPresent and agreed context come from Claim (the same
// origin verifywrap maps); no verify.Input is needed because this package
// never re-judges typed values.
//
// Evidence carries the available provenance rows for this claim (upstream
// pins AND document/field pins, each with its stable row ID). Field-level
// pins (extract.EvidenceRef inside conflict sources and review items) are
// resolved against Evidence by (document, page, block) match; a pin with
// no matching row is a Build error (fail closed: provenance must trace to
// existing artifacts only).
type BuildParams struct {
	TenantID        string
	ClaimID         string
	ExceptionID     string
	InvestigationID string
	Result          verify.Result
	Claim           assemble.CanonicalClaim
	Unresolved      []verifywrap.Unresolved
	Evidence        []EvidenceRef
	Scope           ScopeConstraints
}

// Build constructs and validates an UnresolvedException. Rule findings
// preserve verify emission order (R1-R10, no re-sort); unresolved entries
// are sorted by key; the agreed snapshot excludes AgreedRaw, excludes
// non-AGREED-clean keys, and excludes nothing else; missing evidence is
// derived (required docs re-derived from DocsPresent per gap G6, field
// gaps from MISSING-status affected keys, external gaps from absent
// source pins, F11b line-amount note on amount reconciliation); scope and
// evidence refs are tenant-echo checked.
//
// Errors are terminal validation only, mirroring verifywrap.Map: blank
// identity, unknown rule codes, malformed unresolved entries, unresolvable
// provenance, or scope violations yield an error and no envelope.
func Build(p BuildParams) (UnresolvedException, error) {
	var out UnresolvedException
	if err := claims.TenantID(p.TenantID).Validate(); err != nil {
		return UnresolvedException{}, fmt.Errorf("invest: build: %w", err)
	}
	if err := claims.ClaimID(p.ClaimID).Validate(); err != nil {
		return UnresolvedException{}, fmt.Errorf("invest: build: %w", err)
	}
	if err := ValidateID(ExceptionIDPrefix, p.ExceptionID); err != nil {
		return UnresolvedException{}, fmt.Errorf("invest: build: %w", err)
	}
	if err := ValidateID(InvestigationIDPrefix, p.InvestigationID); err != nil {
		return UnresolvedException{}, fmt.Errorf("invest: build: %w", err)
	}
	out.TenantID = strings.TrimSpace(p.TenantID)
	out.ClaimID = strings.TrimSpace(p.ClaimID)
	out.ExceptionID = p.ExceptionID
	out.InvestigationID = p.InvestigationID

	tenant, claimID := out.TenantID, out.ClaimID

	// Evidence refs first: every later resolution cites them, and they
	// must echo tenant/claim before anything trusts them.
	refs, err := buildEvidenceRefs(p.Evidence, tenant, claimID)
	if err != nil {
		return UnresolvedException{}, err
	}
	out.EvidenceRefs = refs
	index := provenanceIndex(refs)

	// Rule findings verbatim in emission order.
	findings, err := buildRuleFindings(p.Result)
	if err != nil {
		return UnresolvedException{}, err
	}
	out.RuleFindings = findings

	// Unresolved re-homed with evidence IDs resolved.
	unresolved, err := buildUnresolved(p.Unresolved, index)
	if err != nil {
		return UnresolvedException{}, err
	}
	out.Unresolved = unresolved

	// Agreed snapshot: AGREED-clean keys only, never AgreedRaw.
	snapshot, err := buildAgreedSnapshot(p.Claim, unresolved, index)
	if err != nil {
		return UnresolvedException{}, err
	}
	out.AgreedSnapshot = snapshot

	// Missing evidence derived, never asserted.
	out.MissingEvidence = deriveMissing(p.Claim, findings, refs)

	// Scope validated against envelope identity + allowlist.
	scope, err := buildScope(p.Scope, tenant, claimID)
	if err != nil {
		return UnresolvedException{}, err
	}
	out.Scope = scope

	if err := Validate(out); err != nil {
		return UnresolvedException{}, err
	}
	return out, nil
}

// provenanceIndex keys document/field refs by (document, page, block) for
// field-pin resolution.
func provenanceIndex(refs []EvidenceRef) map[extract.EvidenceRef][]string {
	idx := make(map[extract.EvidenceRef][]string)
	for _, r := range refs {
		if r.SourceType != EvidenceSourceDocument && r.SourceType != EvidenceSourceField {
			continue
		}
		k := extract.EvidenceRef{DocumentID: r.DocumentID, Page: r.Page, BlockID: r.BlockID}
		idx[k] = append(idx[k], r.EvidenceID)
	}
	for k := range idx {
		slices.Sort(idx[k])
	}
	return idx
}

// resolvePin returns the sorted-first stored row ID for a field pin, or an
// error when no row traces to the pin.
func resolvePin(idx map[extract.EvidenceRef][]string, pin extract.EvidenceRef, where string) (string, error) {
	ids := idx[pin]
	if len(ids) == 0 {
		return "", fmt.Errorf("invest: build: %s pin (doc %q page %d block %q) traces to no stored evidence row", where, pin.DocumentID, pin.Page, pin.BlockID)
	}
	return ids[0], nil
}

// buildEvidenceRefs validates and canonically orders the provenance set.
func buildEvidenceRefs(refs []EvidenceRef, tenant, claimID string) ([]EvidenceRef, error) {
	out := make([]EvidenceRef, 0, len(refs))
	seen := map[string]struct{}{}
	for i := range refs {
		r := refs[i]
		if strings.TrimSpace(r.EvidenceID) == "" {
			return nil, fmt.Errorf("invest: build: evidence_refs[%d] has blank evidence id", i)
		}
		if _, dup := seen[r.EvidenceID]; dup {
			return nil, fmt.Errorf("invest: build: duplicate evidence id %q", r.EvidenceID)
		}
		seen[r.EvidenceID] = struct{}{}
		switch r.SourceType {
		case EvidenceSourceDocument, EvidenceSourceField, EvidenceSourcePolicy,
			EvidenceSourceTPA, EvidenceSourceProvider, EvidenceSourceRisk:
		default:
			return nil, fmt.Errorf("invest: build: evidence %q has unknown source type %q", r.EvidenceID, string(r.SourceType))
		}
		if strings.TrimSpace(r.SourceID) == "" {
			return nil, fmt.Errorf("invest: build: evidence %q has blank source id", r.EvidenceID)
		}
		if r.TenantID != tenant {
			return nil, fmt.Errorf("invest: build: evidence %q tenant %q != envelope tenant %q", r.EvidenceID, r.TenantID, tenant)
		}
		if r.ClaimID != claimID {
			return nil, fmt.Errorf("invest: build: evidence %q claim %q != envelope claim %q", r.EvidenceID, r.ClaimID, claimID)
		}
		isDoc := r.SourceType == EvidenceSourceDocument || r.SourceType == EvidenceSourceField
		if isDoc {
			if strings.TrimSpace(r.DocumentID) == "" {
				return nil, fmt.Errorf("invest: build: document/field evidence %q needs a document id", r.EvidenceID)
			}
			if r.Page < 1 {
				return nil, fmt.Errorf("invest: build: document/field evidence %q needs a 1-based page", r.EvidenceID)
			}
		} else {
			if r.DocumentID != "" || r.BlockID != "" || r.Page != 0 {
				return nil, fmt.Errorf("invest: build: upstream evidence %q must not carry document provenance", r.EvidenceID)
			}
		}
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b EvidenceRef) int {
		return strings.Compare(a.EvidenceID, b.EvidenceID)
	})
	return out, nil
}

// buildRuleFindings carries verify exceptions verbatim in emission order.
func buildRuleFindings(res verify.Result) ([]RuleFinding, error) {
	out := make([]RuleFinding, 0, len(res.Exceptions))
	for i := range res.Exceptions {
		e := &res.Exceptions[i]
		code := RuleCode(e.Code)
		if !IsKnownRuleCode(e.Code) {
			return nil, fmt.Errorf("invest: build: rule_findings[%d] has unknown code %q (fail closed)", i, e.Code)
		}
		switch Severity(e.Severity) {
		case SeverityHigh, SeverityMedium, SeverityLow:
		default:
			return nil, fmt.Errorf("invest: build: rule_findings[%d] has unknown severity %q", i, e.Severity)
		}
		if strings.TrimSpace(e.Message) == "" {
			return nil, fmt.Errorf("invest: build: rule_findings[%d] (%s) has blank message", i, e.Code)
		}
		aff, err := AffectedFields(code)
		if err != nil {
			return nil, fmt.Errorf("invest: build: %v", err)
		}
		ids := append([]string(nil), e.EvidenceIDs...)
		for _, id := range ids {
			if strings.TrimSpace(id) == "" {
				return nil, fmt.Errorf("invest: build: rule_findings[%d] (%s) has blank evidence id", i, e.Code)
			}
		}
		slices.Sort(ids)
		out = append(out, RuleFinding{
			Code:           code,
			Severity:       Severity(e.Severity),
			Message:        e.Message,
			EvidenceIDs:    ids,
			AffectedFields: aff,
		})
	}
	if out == nil {
		out = []RuleFinding{}
	}
	return out, nil
}

// buildUnresolved re-homes verifywrap entries with evidence resolved.
func buildUnresolved(in []verifywrap.Unresolved, idx map[extract.EvidenceRef][]string) ([]UnresolvedField, error) {
	out := make([]UnresolvedField, 0, len(in))
	for i := range in {
		u := &in[i]
		if strings.TrimSpace(u.Key) == "" {
			return nil, fmt.Errorf("invest: build: unresolved[%d] has blank key", i)
		}
		switch u.Status {
		case assemble.StatusConflict:
			if u.Conflict == nil {
				return nil, fmt.Errorf("invest: build: unresolved %q CONFLICT without ConflictEntry", u.Key)
			}
			if len(u.Review) != 0 {
				return nil, fmt.Errorf("invest: build: unresolved %q CONFLICT carries review items", u.Key)
			}
			cv, err := buildConflictView(u.Key, u.Conflict, idx)
			if err != nil {
				return nil, err
			}
			out = append(out, UnresolvedField{Key: u.Key, Status: u.Status, Conflict: cv})
		case assemble.StatusNeedsReview:
			if u.Conflict != nil {
				return nil, fmt.Errorf("invest: build: unresolved %q NEEDS_REVIEW carries a ConflictEntry", u.Key)
			}
			if len(u.Review) == 0 {
				return nil, fmt.Errorf("invest: build: unresolved %q NEEDS_REVIEW without review items", u.Key)
			}
			rv, err := buildReviewViews(u.Key, u.Review, idx)
			if err != nil {
				return nil, err
			}
			out = append(out, UnresolvedField{Key: u.Key, Status: u.Status, Review: rv})
		default:
			return nil, fmt.Errorf("invest: build: unresolved %q has non-unresolved status %q", u.Key, string(u.Status))
		}
	}
	slices.SortFunc(out, func(a, b UnresolvedField) int {
		return strings.Compare(a.Key, b.Key)
	})
	if out == nil {
		out = []UnresolvedField{}
	}
	return out, nil
}

// buildConflictView copies a conflict entry with provenance resolved and
// canonical ordering (distinct sorted; sources in assemble emission
// order, the (Normalized, Value, DocumentID, Page, BlockID) tuple).
func buildConflictView(key string, c *assemble.ConflictEntry, idx map[extract.EvidenceRef][]string) (*ConflictView, error) {
	if c.Key != key {
		return nil, fmt.Errorf("invest: build: conflict entry key %q != field key %q", c.Key, key)
	}
	distinct := append([]string(nil), c.Distinct...)
	slices.Sort(distinct)
	sources := make([]FieldSourceView, 0, len(c.Sources))
	for j := range c.Sources {
		s := &c.Sources[j]
		id, err := resolvePin(idx, s.Evidence, "unresolved "+key+" source")
		if err != nil {
			return nil, err
		}
		sources = append(sources, FieldSourceView{
			Value:            s.Value,
			Normalized:       s.Normalized,
			EvidenceID:       id,
			Extractor:        s.Extractor,
			ExtractorVersion: s.ExtractorVersion,
			DocType:          s.DocType,
			DocumentID:       s.Evidence.DocumentID,
			Page:             s.Evidence.Page,
			BlockID:          s.Evidence.BlockID,
		})
	}
	slices.SortFunc(sources, func(a, b FieldSourceView) int {
		for _, ab := range [][2]string{
			{a.Normalized, b.Normalized}, {a.Value, b.Value},
			{a.DocumentID, b.DocumentID}, {a.BlockID, b.BlockID},
			{a.EvidenceID, b.EvidenceID},
		} {
			if c := strings.Compare(ab[0], ab[1]); c != 0 {
				return c
			}
		}
		if a.Page != b.Page {
			return a.Page - b.Page
		}
		return 0
	})
	return &ConflictView{Distinct: distinct, Sources: sources}, nil
}

// buildReviewViews copies review items verbatim with provenance resolved.
// Item order is canonicalized by the assemble sort tuple; candidate order
// inside one item is preserved verbatim (artifact encounter order, never
// re-sorted).
func buildReviewViews(key string, items []assemble.ReviewItem, idx map[extract.EvidenceRef][]string) ([]ReviewItemView, error) {
	out := make([]ReviewItemView, 0, len(items))
	for j := range items {
		it := &items[j]
		switch it.Status {
		case extract.StatusAmbiguous, extract.StatusMultiCandidate:
		default:
			return nil, fmt.Errorf("invest: build: unresolved %q review item has non-review status %q", key, string(it.Status))
		}
		id, err := resolvePin(idx, it.Evidence, "unresolved "+key+" review")
		if err != nil {
			return nil, err
		}
		cands := make([]CandidateView, 0, len(it.Candidates))
		for k := range it.Candidates {
			c := &it.Candidates[k]
			cid, err := resolvePin(idx, c.Evidence, "unresolved "+key+" candidate")
			if err != nil {
				return nil, err
			}
			cands = append(cands, CandidateView{Value: c.Value, Normalized: c.Normalized, EvidenceID: cid})
		}
		if cands == nil {
			cands = []CandidateView{}
		}
		out = append(out, ReviewItemView{
			DocumentID: it.DocumentID,
			DocType:    it.DocType,
			Status:     it.Status,
			Value:      it.Value,
			Candidates: cands,
			EvidenceID: id,
		})
	}
	slices.SortFunc(out, func(a, b ReviewItemView) int {
		for _, ab := range [][2]string{
			{a.DocumentID, b.DocumentID}, {a.DocType, b.DocType},
			{string(a.Status), string(b.Status)}, {a.Value, b.Value},
			{a.EvidenceID, b.EvidenceID},
		} {
			if c := strings.Compare(ab[0], ab[1]); c != 0 {
				return c
			}
		}
		return 0
	})
	return out, nil
}

// buildAgreedSnapshot carries AGREED-clean values only: Status AGREED
// with no shadowing review items. Unresolved keys are excluded by
// construction (disjointness enforced in Validate). Source doc types and
// evidence IDs are sorted unique sets. AgreedRaw is never read and has no
// field to land in.
func buildAgreedSnapshot(
	claim assemble.CanonicalClaim,
	unresolved []UnresolvedField,
	idx map[extract.EvidenceRef][]string,
) ([]AgreedField, error) {
	bad := map[string]struct{}{}
	for _, u := range unresolved {
		bad[u.Key] = struct{}{}
	}
	out := []AgreedField{}
	for _, key := range sortedKeys(claim.Fields) {
		f := claim.Fields[key]
		if _, isBad := bad[key]; isBad {
			continue
		}
		if f.Status != assemble.StatusAgreed {
			continue
		}
		if len(f.NeedsReview) != 0 {
			continue
		}
		if strings.TrimSpace(f.Agreed) == "" {
			return nil, fmt.Errorf("invest: build: agreed snapshot key %q AGREED with empty Agreed value", key)
		}
		types := map[string]struct{}{}
		ids := map[string]struct{}{}
		for j := range f.Sources {
			s := &f.Sources[j]
			id, err := resolvePin(idx, s.Evidence, "agreed "+key+" source")
			if err != nil {
				return nil, err
			}
			if strings.TrimSpace(s.DocType) != "" {
				types[s.DocType] = struct{}{}
			}
			ids[id] = struct{}{}
		}
		if len(ids) == 0 {
			return nil, fmt.Errorf("invest: build: agreed snapshot key %q has no resolvable sources", key)
		}
		out = append(out, AgreedField{
			Key:            key,
			Agreed:         f.Agreed,
			SourceDocTypes: sortedSet(types),
			EvidenceIDs:    sortedSet(ids),
		})
	}
	return out, nil
}

// requiredDocs reuses the verify R8 document set (single-sourced, never
// re-spelled here).
var requiredDocs = []string{verify.DocClaimForm, verify.DocDischargeSummary, verify.DocHospitalBill}

// externalSources is the closed external-evidence vocabulary for
// MissingExternal keys (the four upstream pin types).
var externalSources = []string{"policy", "tpa", "provider", "risk"}

// deriveMissing computes the open questions deterministically:
// required docs re-derived from DocsPresent (gap G6: messages are
// display-only, never parsed); field gaps from MISSING-status affected
// keys; external gaps from absent source pins; the F11b line-amount note
// on amount reconciliation (descriptions only by contract, never a
// license to invent). Output sorted by (Kind, Key) with exact duplicates
// removed.
func deriveMissing(claim assemble.CanonicalClaim, findings []RuleFinding, refs []EvidenceRef) []MissingItem {
	var out []MissingItem
	seen := map[MissingItem]struct{}{}
	emit := func(m MissingItem) {
		if _, dup := seen[m]; dup {
			return
		}
		seen[m] = struct{}{}
		out = append(out, m)
	}

	// Required documents from DocsPresent, independent of R8 messages.
	for _, doc := range requiredDocs {
		if claim.DocsPresent == nil || !claim.DocsPresent[doc] {
			emit(MissingItem{Kind: MissingRequiredDocument, Key: doc, Detail: "R8: " + doc + " absent"})
		}
	}

	haveSource := map[string]bool{}
	for _, r := range refs {
		haveSource[string(r.SourceType)] = true
	}

	for i := range findings {
		f := &findings[i]
		for _, key := range f.AffectedFields {
			af, ok := claim.Fields[key]
			if !ok || af.Status == assemble.StatusMissing {
				emit(MissingItem{Kind: MissingField, Key: key, Detail: string(f.Code) + ": " + key + " missing"})
			}
		}
		switch f.Code {
		case RulePolicyNotActive, RuleExternalPolicyMismatch:
			if !haveSource["policy"] {
				emit(MissingItem{Kind: MissingExternal, Key: "policy", Detail: string(f.Code) + ": no pinned policy evidence"})
			}
		case RuleAmountReconciliation:
			// F11b: bill_lines_N carry descriptions only, so line
			// amounts are structurally unavailable. Surfaced as a
			// missing item so the investigator asks for evidence
			// instead of inventing amounts.
			emit(MissingItem{Kind: MissingField, Key: "bill_lines", Detail: "F11b: line amounts unavailable; descriptions only"})
		}
	}
	slices.SortFunc(out, func(a, b MissingItem) int {
		if c := strings.Compare(string(a.Kind), string(b.Kind)); c != 0 {
			return c
		}
		if c := strings.Compare(a.Key, b.Key); c != 0 {
			return c
		}
		return strings.Compare(a.Detail, b.Detail)
	})
	if out == nil {
		out = []MissingItem{}
	}
	return out
}

// buildScope validates the per-case scope against envelope identity and
// the allowlist (least privilege: non-empty subset, sorted unique).
func buildScope(s ScopeConstraints, tenant, claimID string) (ScopeConstraints, error) {
	if s.TenantID != tenant {
		return ScopeConstraints{}, fmt.Errorf("invest: build: scope tenant %q != envelope tenant %q", s.TenantID, tenant)
	}
	if s.ClaimID != claimID {
		return ScopeConstraints{}, fmt.Errorf("invest: build: scope claim %q != envelope claim %q", s.ClaimID, claimID)
	}
	if len(s.AllowTools) == 0 {
		return ScopeConstraints{}, fmt.Errorf("invest: build: scope allow_tools is empty (least privilege needs an explicit subset)")
	}
	seen := map[ToolName]struct{}{}
	tools := make([]ToolName, 0, len(s.AllowTools))
	for _, t := range s.AllowTools {
		if err := validateToolName(t); err != nil {
			return ScopeConstraints{}, fmt.Errorf("invest: build: scope: %v", err)
		}
		if _, dup := seen[t]; dup {
			return ScopeConstraints{}, fmt.Errorf("invest: build: scope duplicates tool %q", string(t))
		}
		seen[t] = struct{}{}
		tools = append(tools, t)
	}
	slices.Sort(tools)
	if s.MaxToolCalls <= 0 {
		return ScopeConstraints{}, fmt.Errorf("invest: build: scope max_tool_calls must be > 0")
	}
	if s.DeadlineMs <= 0 {
		return ScopeConstraints{}, fmt.Errorf("invest: build: scope deadline_ms must be > 0")
	}
	if strings.TrimSpace(s.RequestID) == "" {
		return ScopeConstraints{}, fmt.Errorf("invest: build: scope needs a request id (X-Request-ID propagation)")
	}
	s.AllowTools = tools
	return s, nil
}

// Validate checks an envelope standalone (decode path): identity,
// identifier shapes, known codes/severities in R-order, the deterministic
// affected-field projection, exactly-one-of conflict/review, snapshot
// disjointness from unresolved, sorted canonical order, scope echo +
// allowlist, and tenant/claim echo on every evidence ref. It does not
// touch storage: evidence ID resolution is a Build-time property.
func Validate(e UnresolvedException) error {
	tenant := strings.TrimSpace(e.TenantID)
	if err := claims.TenantID(tenant).Validate(); err != nil {
		return fmt.Errorf("invest: validate: %w", err)
	}
	claimID := strings.TrimSpace(e.ClaimID)
	if err := claims.ClaimID(claimID).Validate(); err != nil {
		return fmt.Errorf("invest: validate: %w", err)
	}
	if e.TenantID != tenant || e.ClaimID != claimID {
		return fmt.Errorf("invest: validate: tenant/claim ids must be trimmed")
	}
	if err := ValidateID(ExceptionIDPrefix, e.ExceptionID); err != nil {
		return fmt.Errorf("invest: validate: %v", err)
	}
	if err := ValidateID(InvestigationIDPrefix, e.InvestigationID); err != nil {
		return fmt.Errorf("invest: validate: %v", err)
	}
	if len(e.RuleFindings) == 0 && len(e.Unresolved) == 0 {
		return fmt.Errorf("invest: validate: empty envelope (no rule findings and no unresolved fields — nothing to investigate)")
	}

	lastRank := 0
	for i := range e.RuleFindings {
		f := &e.RuleFindings[i]
		rank, ok := ruleRank[f.Code]
		if !ok {
			return fmt.Errorf("invest: validate: rule_findings[%d] has unknown code %q", i, string(f.Code))
		}
		if rank < lastRank {
			return fmt.Errorf("invest: validate: rule_findings out of R1-R10 emission order at index %d (%s)", i, string(f.Code))
		}
		lastRank = rank
		switch f.Severity {
		case SeverityHigh, SeverityMedium, SeverityLow:
		default:
			return fmt.Errorf("invest: validate: rule_findings[%d] has unknown severity %q", i, string(f.Severity))
		}
		if strings.TrimSpace(f.Message) == "" {
			return fmt.Errorf("invest: validate: rule_findings[%d] has blank message", i)
		}
		want, err := AffectedFields(f.Code)
		if err != nil {
			return fmt.Errorf("invest: validate: %v", err)
		}
		if !slices.Equal(f.AffectedFields, want) {
			return fmt.Errorf("invest: validate: rule_findings[%d] affected_fields drift from projection", i)
		}
		if !slices.IsSorted(f.EvidenceIDs) {
			return fmt.Errorf("invest: validate: rule_findings[%d] evidence_ids not sorted", i)
		}
		for _, id := range f.EvidenceIDs {
			if strings.TrimSpace(id) == "" {
				return fmt.Errorf("invest: validate: rule_findings[%d] has blank evidence id", i)
			}
		}
	}

	unresolvedKeys := map[string]struct{}{}
	lastKey := ""
	for i := range e.Unresolved {
		u := &e.Unresolved[i]
		if strings.TrimSpace(u.Key) == "" {
			return fmt.Errorf("invest: validate: unresolved[%d] has blank key", i)
		}
		if u.Key < lastKey {
			return fmt.Errorf("invest: validate: unresolved not sorted by key at index %d", i)
		}
		lastKey = u.Key
		if _, dup := unresolvedKeys[u.Key]; dup {
			return fmt.Errorf("invest: validate: duplicate unresolved key %q", u.Key)
		}
		unresolvedKeys[u.Key] = struct{}{}
		switch u.Status {
		case assemble.StatusConflict:
			if u.Conflict == nil || len(u.Review) != 0 {
				return fmt.Errorf("invest: validate: unresolved %q CONFLICT needs exactly the conflict entry", u.Key)
			}
			if err := validateConflictView(u.Key, u.Conflict); err != nil {
				return err
			}
		case assemble.StatusNeedsReview:
			if u.Conflict != nil || len(u.Review) == 0 {
				return fmt.Errorf("invest: validate: unresolved %q NEEDS_REVIEW needs review items only", u.Key)
			}
			if err := validateReviewViews(u.Key, u.Review); err != nil {
				return err
			}
		default:
			return fmt.Errorf("invest: validate: unresolved %q has non-unresolved status %q", u.Key, string(u.Status))
		}
	}

	lastAgreed := ""
	for i := range e.AgreedSnapshot {
		a := &e.AgreedSnapshot[i]
		if strings.TrimSpace(a.Key) == "" || strings.TrimSpace(a.Agreed) == "" {
			return fmt.Errorf("invest: validate: agreed_snapshot[%d] needs key and agreed value", i)
		}
		if a.Key < lastAgreed {
			return fmt.Errorf("invest: validate: agreed_snapshot not sorted by key at index %d", i)
		}
		lastAgreed = a.Key
		if _, bad := unresolvedKeys[a.Key]; bad {
			return fmt.Errorf("invest: validate: key %q in both unresolved and agreed snapshot", a.Key)
		}
		if !slices.IsSorted(a.SourceDocTypes) || !slices.IsSorted(a.EvidenceIDs) {
			return fmt.Errorf("invest: validate: agreed_snapshot %q sets not sorted", a.Key)
		}
		if len(a.EvidenceIDs) == 0 {
			return fmt.Errorf("invest: validate: agreed_snapshot %q needs evidence ids (presence IS the signal)", a.Key)
		}
	}

	lastKind, lastMKey := "", ""
	for i := range e.MissingEvidence {
		m := &e.MissingEvidence[i]
		switch m.Kind {
		case MissingRequiredDocument, MissingField, MissingExternal:
		default:
			return fmt.Errorf("invest: validate: missing_evidence[%d] has unknown kind %q", i, string(m.Kind))
		}
		if strings.TrimSpace(m.Key) == "" {
			return fmt.Errorf("invest: validate: missing_evidence[%d] has blank key", i)
		}
		if m.Kind == MissingExternal && !slices.Contains(externalSources, m.Key) {
			return fmt.Errorf("invest: validate: missing_evidence[%d] unknown external source %q", i, m.Key)
		}
		if strings.TrimSpace(m.Detail) == "" {
			return fmt.Errorf("invest: validate: missing_evidence[%d] has blank detail (display-only context is required)", i)
		}
		pair := string(m.Kind) + "\x00" + m.Key
		prev := lastKind + "\x00" + lastMKey
		if i > 0 && pair < prev {
			return fmt.Errorf("invest: validate: missing_evidence not sorted at index %d", i)
		}
		lastKind, lastMKey = string(m.Kind), m.Key
	}

	if _, err := buildScope(e.Scope, tenant, claimID); err != nil {
		return fmt.Errorf("invest: validate: %v", err)
	}
	// buildScope sorts a copy; require the stored order canonical
	// (same pattern as evidence_refs below): unsorted allow_tools is
	// rejected, not silently repaired.
	wantScope, _ := buildScope(e.Scope, tenant, claimID)
	if !slices.Equal(wantScope.AllowTools, e.Scope.AllowTools) {
		return fmt.Errorf("invest: validate: scope allow_tools not sorted")
	}
	if _, err := buildEvidenceRefs(e.EvidenceRefs, tenant, claimID); err != nil {
		return fmt.Errorf("invest: validate: %v", err)
	}
	// buildEvidenceRefs sorts a copy; require the stored order canonical.
	want, _ := buildEvidenceRefs(e.EvidenceRefs, tenant, claimID)
	for i := range want {
		if want[i].EvidenceID != e.EvidenceRefs[i].EvidenceID {
			return fmt.Errorf("invest: validate: evidence_refs not sorted by evidence id")
		}
	}
	return nil
}

// validateConflictView checks canonical ordering and non-blank provenance.
func validateConflictView(key string, c *ConflictView) error {
	if !slices.IsSorted(c.Distinct) {
		return fmt.Errorf("invest: validate: unresolved %q conflict distinct not sorted", key)
	}
	for j := range c.Sources {
		s := &c.Sources[j]
		if strings.TrimSpace(s.EvidenceID) == "" {
			return fmt.Errorf("invest: validate: unresolved %q source has blank evidence id", key)
		}
		if strings.TrimSpace(s.DocumentID) == "" || s.Page < 1 {
			return fmt.Errorf("invest: validate: unresolved %q source lacks document provenance", key)
		}
	}
	return nil
}

// validateReviewViews checks review statuses, provenance, and candidate
// presence shape (MULTI_CANDIDATE keeps >= 2 candidates verbatim).
func validateReviewViews(key string, items []ReviewItemView) error {
	for j := range items {
		it := &items[j]
		switch it.Status {
		case extract.StatusAmbiguous, extract.StatusMultiCandidate:
		default:
			return fmt.Errorf("invest: validate: unresolved %q review item has non-review status %q", key, string(it.Status))
		}
		if strings.TrimSpace(it.EvidenceID) == "" {
			return fmt.Errorf("invest: validate: unresolved %q review item has blank evidence id", key)
		}
		if it.Status == extract.StatusMultiCandidate && len(it.Candidates) < 2 {
			return fmt.Errorf("invest: validate: unresolved %q multi-candidate item needs >= 2 candidates", key)
		}
		if it.Status == extract.StatusAmbiguous && len(it.Candidates) != 0 {
			return fmt.Errorf("invest: validate: unresolved %q ambiguous item must not carry candidates", key)
		}
		for k := range it.Candidates {
			c := &it.Candidates[k]
			if strings.TrimSpace(c.Value) == "" || strings.TrimSpace(c.EvidenceID) == "" {
				return fmt.Errorf("invest: validate: unresolved %q candidate %d needs value and evidence id", key, k)
			}
		}
	}
	return nil
}

// Marshal renders the canonical bytes for an envelope: validate first
// (fail closed: out-of-order or otherwise invalid envelopes are rejected
// rather than silently reordered), then encode a canonicalized copy with
// encoding/json v1. Canonical order is sorted everywhere except the two
// encounter-order contents, which are preserved verbatim: rule findings
// stay in verify R1-R10 emission order and review candidates stay in
// artifact encounter order. Equivalent envelopes produce byte-identical
// output regardless of construction path (no #50 regression: no maps in
// the shape, no timestamps, no floats).
func Marshal(e UnresolvedException) ([]byte, error) {
	if err := Validate(e); err != nil {
		return nil, err
	}
	c := canonicalize(e)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(c); err != nil {
		return nil, fmt.Errorf("invest: marshal: %v", err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// Decode parses and validates one envelope with strict field checking:
// unknown fields (including agreed_raw, confidence, prompt, or any future
// stage-foreign key) are rejected, trailing data is rejected, and the
// result must pass Validate.
func Decode(data []byte) (UnresolvedException, error) {
	var e UnresolvedException
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&e); err != nil {
		return UnresolvedException{}, fmt.Errorf("invest: decode: %v", err)
	}
	// Trailing-data check: a second decode must hit EOF.
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return UnresolvedException{}, fmt.Errorf("invest: decode: trailing data")
	}
	normalizeNil(&e)
	if err := Validate(e); err != nil {
		return UnresolvedException{}, err
	}
	return e, nil
}

// canonicalize returns a copy in canonical order with every nil slice
// normalized to empty, so marshaling is a pure function of content.
// Sorted everywhere except the two encounter-order contents, preserved
// verbatim: rule findings keep verify R1-R10 emission order (the rank
// sort below is a no-op on validated input) and review candidates keep
// artifact encounter order (never sorted here or in normalize).
func canonicalize(e UnresolvedException) UnresolvedException {
	normalize(&e)
	slices.SortFunc(e.RuleFindings, func(a, b RuleFinding) int {
		ra, rb := ruleRank[a.Code], ruleRank[b.Code]
		if ra != rb {
			return ra - rb
		}
		if c := strings.Compare(string(a.Severity), string(b.Severity)); c != 0 {
			return c
		}
		return strings.Compare(a.Message, b.Message)
	})
	slices.SortFunc(e.Unresolved, func(a, b UnresolvedField) int {
		return strings.Compare(a.Key, b.Key)
	})
	slices.SortFunc(e.AgreedSnapshot, func(a, b AgreedField) int {
		return strings.Compare(a.Key, b.Key)
	})
	slices.SortFunc(e.MissingEvidence, func(a, b MissingItem) int {
		if c := strings.Compare(string(a.Kind), string(b.Kind)); c != 0 {
			return c
		}
		if c := strings.Compare(a.Key, b.Key); c != 0 {
			return c
		}
		return strings.Compare(a.Detail, b.Detail)
	})
	slices.SortFunc(e.EvidenceRefs, func(a, b EvidenceRef) int {
		return strings.Compare(a.EvidenceID, b.EvidenceID)
	})
	return e
}

// normalizeNil nil-normalizes slices in place without reordering any
// content. Decode path only: Validate must see wire order verbatim so
// unsorted inner sets are rejected, not silently repaired.
func normalizeNil(e *UnresolvedException) {
	for i := range e.RuleFindings {
		f := &e.RuleFindings[i]
		if f.EvidenceIDs == nil {
			f.EvidenceIDs = []string{}
		}
		if f.AffectedFields == nil {
			f.AffectedFields = []string{}
		}
	}
	for i := range e.Unresolved {
		u := &e.Unresolved[i]
		if u.Conflict != nil {
			if u.Conflict.Distinct == nil {
				u.Conflict.Distinct = []string{}
			}
			if u.Conflict.Sources == nil {
				u.Conflict.Sources = []FieldSourceView{}
			}
		}
		for j := range u.Review {
			if u.Review[j].Candidates == nil {
				u.Review[j].Candidates = []CandidateView{}
			}
		}
		if u.Review == nil {
			u.Review = []ReviewItemView{}
		}
	}
	for i := range e.AgreedSnapshot {
		a := &e.AgreedSnapshot[i]
		if a.SourceDocTypes == nil {
			a.SourceDocTypes = []string{}
		}
		if a.EvidenceIDs == nil {
			a.EvidenceIDs = []string{}
		}
	}
	if e.RuleFindings == nil {
		e.RuleFindings = []RuleFinding{}
	}
	if e.Unresolved == nil {
		e.Unresolved = []UnresolvedField{}
	}
	if e.AgreedSnapshot == nil {
		e.AgreedSnapshot = []AgreedField{}
	}
	if e.MissingEvidence == nil {
		e.MissingEvidence = []MissingItem{}
	}
	if e.EvidenceRefs == nil {
		e.EvidenceRefs = []EvidenceRef{}
	}
	if e.Scope.AllowTools == nil {
		e.Scope.AllowTools = []ToolName{}
	}
}

// normalize sorts the inner sets and nil-normalizes slices in place.
// Marshal/canonicalize path only; never call on the decode path (it
// would silently repair the unsorted orders Validate must reject).
func normalize(e *UnresolvedException) {
	normalizeNil(e)
	for i := range e.RuleFindings {
		slices.Sort(e.RuleFindings[i].EvidenceIDs)
	}
	for i := range e.Unresolved {
		u := &e.Unresolved[i]
		if u.Conflict != nil {
			slices.Sort(u.Conflict.Distinct)
		}
	}
	for i := range e.AgreedSnapshot {
		a := &e.AgreedSnapshot[i]
		slices.Sort(a.SourceDocTypes)
		slices.Sort(a.EvidenceIDs)
	}
	slices.Sort(e.Scope.AllowTools)
}

// sortedKeys returns the sorted map keys for deterministic iteration.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// sortedSet returns the sorted elements of a string set.
func sortedSet(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	slices.Sort(out)
	return out
}
