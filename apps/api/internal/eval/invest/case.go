// Package invest is the Phase 0 evaluation harness for the bounded
// investigation control plane (issue #70).
//
// It separates control-plane determinism (this package: does the real
// orchestrate.Loop enforce its contracts under scripted cognition?) from
// model quality (later phases: does a live model investigate well?). No
// live model, no network, no database anywhere on this path: the model is
// a scripted fake and every tool is an in-memory stub.
//
// Layout (mirrors the internal/eval/bench precedent):
//
//	case.go   — EvalCase shape plus standalone validation.
//	corpus.go — hand-built corpus A-M (valid invest envelopes only).
//	harness.go — Harness driving the REAL Loop + fakes, EvalResult
//	            (IDs/codes/counts only, never values or prose).
//	e0.go     — E0 gates as pure assertions over EvalResult.
//
// G9 dimensions (no /tmp/invest-spec.md present; derived from the
// invest epistemic contracts): fact-echo fidelity, falsifier presence,
// finding→hypothesis resolution, closed recommendation set, additive-only
// missing evidence, tenant isolation, citation grounding, budget bounds,
// loop termination, and no-mutation. Each maps to one E0 gate.
//
// Result hygiene (bench PII rule, applied here): results and gates carry
// field KEYS, evidence IDs, tool names, codes, and counts — never field
// VALUES, agreed strings, rationales, or statements. No float64 anywhere.
package invest

import (
	"fmt"
	"slices"
	"strings"

	"claimops-api/internal/invest"
	"claimops-api/internal/investigate"
	"claimops-api/internal/investigate/orchestrate"
)

// FaultKind selects the stub-tool failure mode for a case. Honest cases
// use FaultNone (stubs return the case's expected evidence); I/J use the
// failing modes to exercise timeout and malformed-upstream paths.
type FaultKind string

const (
	// FaultNone serves the expected evidence IDs from every stub tool.
	FaultNone FaultKind = ""
	// FaultUpstream fails every stub call with investigate.ErrUpstream
	// (transient class, retried inside the executor).
	FaultUpstream FaultKind = "upstream"
	// FaultContract fails every stub call with investigate.ErrContract
	// (terminal class, never retried).
	FaultContract FaultKind = "contract"
)

// ScriptedTurn is one scripted fake-model turn: the raw payload the fake
// serves, or Err when the turn must fail at transport. Payloads are built
// with the corpus helpers (callPayload/submitPayload) so scripts are
// structurally valid by construction unless a temptation case deliberately
// tampers with them.
type ScriptedTurn struct {
	Payload []byte
	Err     error
}

// CaseBudgets overrides the loop budgets for one case. Zero values select
// the harness defaults (scope MaxCalls 5, loop MaxTurns 12). MaxToolCalls
// binds BOTH the scope and the loop cap (single source, so the loop can
// never grant more than the scope allows by construction).
type CaseBudgets struct {
	MaxToolCalls int
	MaxTurns     int
}

// ExpectedFinding is one acceptable finding-ID shape: the finding ID, the
// hypothesis it must resolve, and its exact evidence-ID set. Shapes only:
// no summaries, no prose. Checked by the harness (FindingsAcceptable) for
// later quality phases; E0 gates assert structural validity, not match.
type ExpectedFinding struct {
	ID           string
	HypothesisID string
	EvidenceIDs  []string
}

// EvalCase is one evaluation case: a valid envelope, the least-privilege
// scope tools, the scripted cognition, and the closed expectations.
type EvalCase struct {
	ID         string
	Name       string
	Envelope   invest.UnresolvedException
	ScopeTools []invest.ToolName
	Budgets    CaseBudgets
	ToolFault  FaultKind
	Scripted   []ScriptedTurn
	// ExpectedEvidenceIDs is the full citable universe for the case: the
	// envelope seed IDs plus every ID the stubs may grow. Sorted unique.
	ExpectedEvidenceIDs []string
	PermittedTools      []invest.ToolName
	AcceptableFindings  []ExpectedFinding
	// AcceptableRecommendations is the closed-enum subset the case may
	// conclude with. E0 requires validity (closed set + cites findings);
	// subset match is recorded for later phases.
	AcceptableRecommendations []invest.RecommendationAction
	MustEscalate              bool
	MustEscalateReason        orchestrate.EscalationReason
}

// Validate checks one case standalone: identity, a valid envelope, scope
// tools that echo the envelope authority, a non-empty script, a sorted
// unique evidence universe covering every seed ID, and a coherent
// escalation expectation.
func (c EvalCase) Validate() error {
	if strings.TrimSpace(c.ID) == "" {
		return fmt.Errorf("eval: case has blank id")
	}
	if strings.TrimSpace(c.Name) == "" {
		return fmt.Errorf("eval: case %q has blank name", c.ID)
	}
	if err := invest.Validate(c.Envelope); err != nil {
		return fmt.Errorf("eval: case %q envelope: %v", c.ID, err)
	}
	if len(c.ScopeTools) == 0 {
		return fmt.Errorf("eval: case %q needs scope tools (least privilege needs an explicit subset)", c.ID)
	}
	allowed := make(map[invest.ToolName]struct{}, len(c.Envelope.Scope.AllowTools))
	for _, t := range c.Envelope.Scope.AllowTools {
		allowed[t] = struct{}{}
	}
	seen := make(map[invest.ToolName]struct{}, len(c.ScopeTools))
	for _, t := range c.ScopeTools {
		if !invest.IsAllowlisted(t) {
			return fmt.Errorf("eval: case %q scope tool %q not allowlisted", c.ID, string(t))
		}
		if _, ok := allowed[t]; !ok {
			return fmt.Errorf("eval: case %q scope tool %q outside envelope authority", c.ID, string(t))
		}
		if _, dup := seen[t]; dup {
			return fmt.Errorf("eval: case %q duplicates scope tool %q", c.ID, string(t))
		}
		seen[t] = struct{}{}
	}
	switch c.ToolFault {
	case FaultNone, FaultUpstream, FaultContract:
	default:
		return fmt.Errorf("eval: case %q has unknown tool fault %q", c.ID, string(c.ToolFault))
	}
	if len(c.Scripted) == 0 {
		return fmt.Errorf("eval: case %q needs a non-empty model script", c.ID)
	}
	if err := checkSortedUnique(c.ExpectedEvidenceIDs, "expected evidence ids"); err != nil {
		return fmt.Errorf("eval: case %q: %v", c.ID, err)
	}
	seeds := make(map[string]struct{}, len(c.Envelope.EvidenceRefs))
	for i := range c.Envelope.EvidenceRefs {
		seeds[c.Envelope.EvidenceRefs[i].EvidenceID] = struct{}{}
	}
	for id := range seeds {
		if !slices.Contains(c.ExpectedEvidenceIDs, id) {
			return fmt.Errorf("eval: case %q universe drops seed evidence id %q", c.ID, id)
		}
	}
	if len(c.PermittedTools) == 0 {
		return fmt.Errorf("eval: case %q needs permitted tools", c.ID)
	}
	for _, t := range c.PermittedTools {
		if !invest.IsAllowlisted(t) {
			return fmt.Errorf("eval: case %q permitted tool %q not allowlisted", c.ID, string(t))
		}
	}
	if len(c.AcceptableFindings) == 0 && !c.MustEscalate {
		return fmt.Errorf("eval: case %q ready-path needs acceptable finding shapes", c.ID)
	}
	for i := range c.AcceptableFindings {
		f := &c.AcceptableFindings[i]
		if strings.TrimSpace(f.ID) == "" || strings.TrimSpace(f.HypothesisID) == "" {
			return fmt.Errorf("eval: case %q acceptable finding %d needs ids", c.ID, i)
		}
		if err := checkSortedUnique(f.EvidenceIDs, "acceptable finding evidence ids"); err != nil {
			return fmt.Errorf("eval: case %q: %v", c.ID, err)
		}
	}
	if len(c.AcceptableRecommendations) == 0 && !c.MustEscalate {
		return fmt.Errorf("eval: case %q ready-path needs acceptable recommendations", c.ID)
	}
	for _, a := range c.AcceptableRecommendations {
		switch a {
		case invest.RecommendRequestEvidence, invest.RecommendConfirmException,
			invest.RecommendReferHuman, invest.RecommendReverify:
		default:
			return fmt.Errorf("eval: case %q has unknown acceptable recommendation %q", c.ID, string(a))
		}
	}
	if c.MustEscalate && c.MustEscalateReason == "" {
		return fmt.Errorf("eval: case %q escalates without a reason", c.ID)
	}
	if !c.MustEscalate && c.MustEscalateReason != "" {
		return fmt.Errorf("eval: case %q ready-path carries an escalation reason", c.ID)
	}
	if c.Budgets.MaxToolCalls < 0 || c.Budgets.MaxTurns < 0 {
		return fmt.Errorf("eval: case %q has negative budgets", c.ID)
	}
	_ = investigate.Scope{}
	return nil
}

// checkSortedUnique requires a trimmed non-blank, sorted, duplicate-free
// ID list (deterministic citation order, the sorted-first rule).
func checkSortedUnique(ids []string, where string) error {
	for i := range ids {
		if strings.TrimSpace(ids[i]) == "" {
			return fmt.Errorf("eval: %s has blank id at index %d", where, i)
		}
		if ids[i] != strings.TrimSpace(ids[i]) {
			return fmt.Errorf("eval: %s id must be trimmed", where)
		}
	}
	if !slices.IsSorted(ids) {
		return fmt.Errorf("eval: %s not sorted", where)
	}
	for i := 1; i < len(ids); i++ {
		if ids[i] == ids[i-1] {
			return fmt.Errorf("eval: %s duplicates id %q", where, ids[i])
		}
	}
	return nil
}
