package invest

import (
	"fmt"
	"slices"
	"strings"
)

// Epistemic contracts for the future bounded cognitive layer (issue #53).
//
// These types are the structural epistemic model from doc.go:
//
//	FACT -> EVIDENCE -> HYPOTHESIS -> FINDING -> RECOMMENDATION -> HUMAN -> MUTATION
//
// CONTRACTS ONLY. No agent, no model client, no tools, no prompts, no
// planning. Every stage is a distinct named type; all validation is
// package-level Validate* functions (zero methods on any type in this
// file, hence no conversion helpers to discover); there are no confidence
// floats anywhere (a hypothesis carries its REQUIRED falsifier instead);
// serialized forms use encoding/json v1 struct tags only.
//
// HYPOTHESIS->FACT is impossible by construction: a Hypothesis cites
// FactRefs (key + agreed value + evidence id) but carries no agreed-value
// setter, no assembler input, and no verify input; strict JSON decoding
// (DisallowUnknownFields, enforced by callers and asserted in tests)
// rejects stage-foreign fields, so a hypothesis can never be smuggled
// back into a fact slot.
//
// RECOMMENDATION->STATE is impossible by construction: a Recommendation
// carries only the closed 4-action enum plus rationale and finding IDs.
// It carries no transition shape, no verdict, no state mutation, no actor,
// and no claimant-facing outcome vocabulary (APPROVE/DENY/PAY/MUTATE and
// kin are absent by contract and asserted by the vocabulary scan in
// tests). Only HumanDecision — actor + role + idempotency key + expected
// version — can authorize a transition, and the transition itself lives in
// the existing claims/workflow types, never here. There is no mutation
// type or helper in this package, and there never will be.

// FactRef cites one agreed-clean fact a hypothesis reasons over: the
// canonical key, its agreed value (read-only echo for retrieval scoping),
// and the stored-evidence row that pins it. Presence IS the signal; there
// is no confidence float.
type FactRef struct {
	Key        string `json:"key"`
	Agreed     string `json:"agreed"`
	EvidenceID string `json:"evidence_id"`
}

// ValidateFactRef checks one fact citation standalone.
func ValidateFactRef(f FactRef) error {
	if strings.TrimSpace(f.Key) == "" {
		return fmt.Errorf("invest: fact_ref has blank key")
	}
	if strings.TrimSpace(f.Agreed) == "" {
		return fmt.Errorf("invest: fact_ref %q has blank agreed value", f.Key)
	}
	if strings.TrimSpace(f.EvidenceID) == "" {
		return fmt.Errorf("invest: fact_ref %q has blank evidence id", f.Key)
	}
	return nil
}

// HypothesisStatus is the closed lifecycle of a candidate explanation.
type HypothesisStatus string

// Closed hypothesis statuses: OPEN (under test), SUPPORTED (survived
// retrieval against cited evidence), REFUTED (killed by its falsifier or
// by cited evidence). No other value exists.
const (
	HypothesisOpen      HypothesisStatus = "OPEN"
	HypothesisSupported HypothesisStatus = "SUPPORTED"
	HypothesisRefuted   HypothesisStatus = "REFUTED"
)

// Hypothesis is one candidate explanation for the exception under
// investigation. Falsifier is REQUIRED and non-blank: it states what
// cited evidence would kill this hypothesis, replacing confidence floats
// with structural uncertainty. EvidenceIDs cite stored-evidence rows
// only; invented citations fail validation downstream at report-store
// time and fail review now by shape (non-blank, sorted, unique).
type Hypothesis struct {
	ID          string           `json:"id"`
	Statement   string           `json:"statement"`
	Falsifier   string           `json:"falsifier"`
	Status      HypothesisStatus `json:"status"`
	FactRefs    []FactRef        `json:"fact_refs"`
	EvidenceIDs []string         `json:"evidence_ids"`
}

// ValidateHypothesis checks one hypothesis standalone.
func ValidateHypothesis(h Hypothesis) error {
	if strings.TrimSpace(h.ID) == "" {
		return fmt.Errorf("invest: hypothesis has blank id")
	}
	if strings.TrimSpace(h.Statement) == "" {
		return fmt.Errorf("invest: hypothesis %q has blank statement", h.ID)
	}
	if strings.TrimSpace(h.Falsifier) == "" {
		return fmt.Errorf("invest: hypothesis %q needs a falsifier (structural uncertainty, no confidence floats)", h.ID)
	}
	switch h.Status {
	case HypothesisOpen, HypothesisSupported, HypothesisRefuted:
	default:
		return fmt.Errorf("invest: hypothesis %q has unknown status %q", h.ID, string(h.Status))
	}
	for i := range h.FactRefs {
		if err := ValidateFactRef(h.FactRefs[i]); err != nil {
			return fmt.Errorf("invest: hypothesis %q: %v", h.ID, err)
		}
	}
	if err := checkSortedUniqueIDs(h.EvidenceIDs, "hypothesis "+h.ID+" evidence_ids"); err != nil {
		return err
	}
	if len(h.FactRefs) == 0 && len(h.EvidenceIDs) == 0 {
		return fmt.Errorf("invest: hypothesis %q cites nothing (uncited hypotheses are invented findings)", h.ID)
	}
	return nil
}

// Finding is one hypothesis that survived retrieval: it names the
// hypothesis it resolves and summarizes what the CITED evidence shows.
// It cites evidence only — no agreed-value echoes, no new facts, no
// recommendations. At least one evidence citation is required: an
// uncited finding is an invented finding.
type Finding struct {
	ID           string   `json:"id"`
	HypothesisID string   `json:"hypothesis_id"`
	Summary      string   `json:"summary"`
	EvidenceIDs  []string `json:"evidence_ids"`
}

// ValidateFinding checks one finding standalone.
func ValidateFinding(f Finding) error {
	if strings.TrimSpace(f.ID) == "" {
		return fmt.Errorf("invest: finding has blank id")
	}
	if strings.TrimSpace(f.HypothesisID) == "" {
		return fmt.Errorf("invest: finding %q has blank hypothesis id", f.ID)
	}
	if strings.TrimSpace(f.Summary) == "" {
		return fmt.Errorf("invest: finding %q has blank summary", f.ID)
	}
	if len(f.EvidenceIDs) == 0 {
		return fmt.Errorf("invest: finding %q cites no evidence (uncited findings are invented findings)", f.ID)
	}
	return checkSortedUniqueIDs(f.EvidenceIDs, "finding "+f.ID+" evidence_ids")
}

// RecommendationAction is the closed recommendation set. Exactly four
// values exist; anything else (approve/deny/pay/mutate/transition/
// verdict vocabulary of any kind) is rejected.
type RecommendationAction string

// Closed recommendation actions (spec: the agent recommends and explains
// but never decides).
const (
	// RecommendRequestEvidence asks for named further evidence before
	// any conclusion.
	RecommendRequestEvidence RecommendationAction = "REQUEST_EVIDENCE"
	// RecommendConfirmException holds the deterministic exception as
	// correctly raised and routes it to human review unchanged.
	RecommendConfirmException RecommendationAction = "CONFIRM_EXCEPTION"
	// RecommendReferHuman routes the case to a named human role with
	// the findings attached.
	RecommendReferHuman RecommendationAction = "REFER_HUMAN"
	// RecommendReverify asks the deterministic side to re-run
	// verification after corrected inputs arrive.
	RecommendReverify RecommendationAction = "REVERIFY"
)

// Recommendation is the terminal cognitive output: one closed action plus
// the rationale and the findings it rests on. It carries no transition,
// no verdict, no mutation, no actor, and no outcome vocabulary — those
// belong to HumanDecision and the existing claims/workflow types only.
type Recommendation struct {
	Action     RecommendationAction `json:"action"`
	Rationale  string               `json:"rationale"`
	FindingIDs []string             `json:"finding_ids"`
}

// ValidateRecommendation checks one recommendation standalone.
func ValidateRecommendation(r Recommendation) error {
	switch r.Action {
	case RecommendRequestEvidence, RecommendConfirmException, RecommendReferHuman, RecommendReverify:
	default:
		return fmt.Errorf("invest: recommendation has unknown action %q (closed 4-action set)", string(r.Action))
	}
	if strings.TrimSpace(r.Rationale) == "" {
		return fmt.Errorf("invest: recommendation %s has blank rationale", string(r.Action))
	}
	if len(r.FindingIDs) == 0 {
		return fmt.Errorf("invest: recommendation %s cites no findings", string(r.Action))
	}
	return checkSortedUniqueIDs(r.FindingIDs, "recommendation "+string(r.Action)+" finding_ids")
}

// Decision is the closed human outcome set. These words appear ONLY on
// HumanDecision, never on Recommendation (asserted by the vocabulary scan
// in tests).
type Decision string

// Closed human decisions.
const (
	DecisionApprove  Decision = "APPROVE"
	DecisionReject   Decision = "REJECT"
	DecisionEscalate Decision = "ESCALATE"
)

// HumanRole is the closed set of roles that may authorize a transition.
type HumanRole string

// Closed human roles. AUDITOR is deliberately absent: auditors are
// read-only and can never authorize a transition; a HumanDecision naming
// AUDITOR is rejected with a dedicated error (see ValidateHumanDecision).
const (
	RoleAdjudicator     HumanRole = "ADJUDICATOR"
	RoleSupervisor      HumanRole = "SUPERVISOR"
	RoleMedicalReviewer HumanRole = "MEDICAL_REVIEWER"
)

// HumanDecision is the only record that can authorize a transition: the
// named actor, their authorizing role, the decision, the investigation it
// resolves, and the optimistic-concurrency pair (idempotency key +
// expected version) the report store enforces. Versions start at 1.
type HumanDecision struct {
	Actor           string    `json:"actor"`
	Role            HumanRole `json:"role"`
	Decision        Decision  `json:"decision"`
	InvestigationID string    `json:"investigation_id"`
	IdempotencyKey  string    `json:"idempotency_key"`
	ExpectedVersion int64     `json:"expected_version"`
	Rationale       string    `json:"rationale"`
}

// ValidateHumanDecision checks one human decision standalone.
func ValidateHumanDecision(d HumanDecision) error {
	if strings.TrimSpace(d.Actor) == "" {
		return fmt.Errorf("invest: human decision has blank actor")
	}
	if string(d.Role) == "AUDITOR" {
		return fmt.Errorf("invest: human decision role AUDITOR is read-only and cannot authorize a transition")
	}
	switch d.Role {
	case RoleAdjudicator, RoleSupervisor, RoleMedicalReviewer:
	default:
		return fmt.Errorf("invest: human decision has unknown role %q", string(d.Role))
	}
	switch d.Decision {
	case DecisionApprove, DecisionReject, DecisionEscalate:
	default:
		return fmt.Errorf("invest: human decision has unknown decision %q", string(d.Decision))
	}
	if err := ValidateID(InvestigationIDPrefix, d.InvestigationID); err != nil {
		return fmt.Errorf("invest: human decision: %v", err)
	}
	if strings.TrimSpace(d.IdempotencyKey) == "" {
		return fmt.Errorf("invest: human decision needs an idempotency key")
	}
	if d.ExpectedVersion < 1 {
		return fmt.Errorf("invest: human decision expected version must be >= 1")
	}
	if strings.TrimSpace(d.Rationale) == "" {
		return fmt.Errorf("invest: human decision has blank rationale")
	}
	return nil
}

// checkSortedUniqueIDs requires a non-blank, sorted, duplicate-free ID
// list (deterministic citation order, the sorted-first rule).
func checkSortedUniqueIDs(ids []string, where string) error {
	for i := range ids {
		if strings.TrimSpace(ids[i]) == "" {
			return fmt.Errorf("invest: %s has blank id at index %d", where, i)
		}
	}
	if !slices.IsSorted(ids) {
		return fmt.Errorf("invest: %s not sorted", where)
	}
	for i := 1; i < len(ids); i++ {
		if ids[i] == ids[i-1] {
			return fmt.Errorf("invest: %s duplicates id %q", where, ids[i])
		}
	}
	return nil
}
