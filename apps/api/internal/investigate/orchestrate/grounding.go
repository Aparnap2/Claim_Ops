package orchestrate

import (
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"

	"claimops-api/internal/invest"
	"claimops-api/internal/investigate"
)

// KnownEvidence is the closed citation set for one investigation run.
//
// It is seeded from the envelope EvidenceRefs and grows ONLY from
// validated investigate.Response.IDs (see GrowKnownEvidence). Model text
// never contributes an ID: citations outside this set fail Gate B with
// ErrGrounding. The zero value is an empty set; SeedKnownEvidence is the
// normal constructor.
//
// This is a plain struct with package-level functions only: wire types
// carry zero methods in this package, and the only method in non-test
// code is (*Loop).Run.
type KnownEvidence struct {
	ids map[string]struct{}
}

// SeedKnownEvidence builds the initial set from the envelope provenance
// rows. Every entry must carry a trimmed non-blank evidence ID; duplicates
// across rows collapse to one member. The envelope itself is validated by
// the caller (NewLoop); this constructor enforces only the set property
// so a malformed seed fails closed here too.
func SeedKnownEvidence(e invest.UnresolvedException) (KnownEvidence, error) {
	out := KnownEvidence{ids: make(map[string]struct{}, len(e.EvidenceRefs))}
	for i := range e.EvidenceRefs {
		id := e.EvidenceRefs[i].EvidenceID
		if strings.TrimSpace(id) == "" || id != strings.TrimSpace(id) {
			return KnownEvidence{}, fmt.Errorf("orchestrate: evidence seed[%d] needs a trimmed id: %w", i, ErrModelContract)
		}
		out.ids[id] = struct{}{}
	}
	return out, nil
}

// GrowKnownEvidence adds the IDs of one tool response to the set and
// reports how many members are new. The response is re-validated first
// (fail closed: an invalid response grows nothing and reports the cause),
// so only validated investigate.Response.IDs can widen the set. A nil
// receiver map is allocated on first growth.
func GrowKnownEvidence(k *KnownEvidence, resp investigate.Response) (int, error) {
	if err := resp.Validate(); err != nil {
		return 0, fmt.Errorf("orchestrate: known evidence refuses invalid response: %v: %w", err, ErrModelContract)
	}
	if k.ids == nil {
		k.ids = make(map[string]struct{}, len(resp.IDs))
	}
	added := 0
	for _, id := range resp.IDs {
		if _, ok := k.ids[id]; !ok {
			k.ids[id] = struct{}{}
			added++
		}
	}
	return added, nil
}

// KnownContains reports whether id is already citable.
func KnownContains(k KnownEvidence, id string) bool {
	_, ok := k.ids[id]
	return ok
}

// KnownLen reports the set cardinality.
func KnownLen(k KnownEvidence) int {
	return len(k.ids)
}

// KnownIDs returns the sorted member copy the loop hands to the model as
// KnownEvidenceIDs. Sorting keeps the canonical ModelRequest a pure
// function of content.
func KnownIDs(k KnownEvidence) []string {
	out := make([]string, 0, len(k.ids))
	for id := range k.ids {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

// checkKnownSubset requires every cited ID to already be a member, so
// invented citations fail closed with ErrGrounding (I6).
func checkKnownSubset(k KnownEvidence, ids []string, where string) error {
	for _, id := range ids {
		if !KnownContains(k, id) {
			return newInvalidError(invalidGrounding, ErrGrounding,
				fmt.Errorf("%s cites unknown evidence id %q", where, id))
		}
	}
	return nil
}

// CheckReportGrounding enforces Gate B over one structurally valid report:
// every citation must already be a KnownEvidence member, every finding
// must resolve to a hypothesis from the same report, the recommendation
// must rest on findings from the same report (transitively grounding it
// in resolved hypotheses), the missing list must extend the envelope
// (additive only, details required), and every hypothesis fact reference
// must echo an agreed snapshot entry exactly (read-only: key and agreed
// value match verbatim, and the cited row is one of that entry's rows).
//
// Structural shape (ValidateReport) must pass before this runs; failures
// here classify as I6 and wrap ErrGrounding.
func CheckReportGrounding(r Report, known KnownEvidence, e invest.UnresolvedException) error {
	hyps := make(map[string]invest.Hypothesis, len(r.Hypotheses))
	for i := range r.Hypotheses {
		h := &r.Hypotheses[i]
		hyps[h.ID] = *h
		if err := checkKnownSubset(known, h.EvidenceIDs, "hypothesis "+h.ID); err != nil {
			return err
		}
		if err := checkFactRefs(*h, e); err != nil {
			return err
		}
	}
	finds := make(map[string]invest.Finding, len(r.Findings))
	for i := range r.Findings {
		f := &r.Findings[i]
		if _, ok := hyps[f.HypothesisID]; !ok {
			return newInvalidError(invalidGrounding, ErrGrounding,
				fmt.Errorf("finding %q resolves unknown hypothesis %q", f.ID, f.HypothesisID))
		}
		finds[f.ID] = *f
		if err := checkKnownSubset(known, f.EvidenceIDs, "finding "+f.ID); err != nil {
			return err
		}
	}
	for _, id := range r.Recommendation.FindingIDs {
		if _, ok := finds[id]; !ok {
			return newInvalidError(invalidGrounding, ErrGrounding,
				fmt.Errorf("recommendation cites unknown finding %q", id))
		}
	}
	if err := checkMissingAdditive(r.MissingAdditive, e.MissingEvidence); err != nil {
		return err
	}
	return nil
}

// checkFactRefs enforces the read-only agreed-snapshot check for one
// hypothesis: each fact reference must name an agreed entry by key, echo
// its agreed value verbatim, and cite one of that entry's evidence rows
// (which is therefore already known). Anything else is an attempt to
// re-judge agreed context or to invent provenance.
func checkFactRefs(h invest.Hypothesis, e invest.UnresolvedException) error {
	agreed := make(map[string]invest.AgreedField, len(e.AgreedSnapshot))
	for i := range e.AgreedSnapshot {
		a := &e.AgreedSnapshot[i]
		agreed[a.Key] = *a
	}
	for i := range h.FactRefs {
		fr := &h.FactRefs[i]
		a, ok := agreed[fr.Key]
		if !ok {
			return newInvalidError(invalidGrounding, ErrGrounding,
				fmt.Errorf("hypothesis %q fact ref cites non-agreed key %q", h.ID, fr.Key))
		}
		if fr.Agreed != a.Agreed {
			return newInvalidError(invalidGrounding, ErrGrounding,
				fmt.Errorf("hypothesis %q fact ref re-judges agreed key %q", h.ID, fr.Key))
		}
		if !slices.Contains(a.EvidenceIDs, fr.EvidenceID) {
			return newInvalidError(invalidGrounding, ErrGrounding,
				fmt.Errorf("hypothesis %q fact ref cites row %q outside agreed key %q", h.ID, fr.EvidenceID, fr.Key))
		}
	}
	return nil
}

// checkMissingAdditive requires the submitted list to extend the envelope
// list (every derived open question survives; the model may only add) and
// re-asserts the non-blank detail rule at the grounding boundary.
func checkMissingAdditive(got []invest.MissingItem, envelope []invest.MissingItem) error {
	have := make(map[invest.MissingItem]struct{}, len(got))
	for _, m := range got {
		if strings.TrimSpace(m.Detail) == "" {
			return newInvalidError(invalidGrounding, ErrGrounding,
				fmt.Errorf("missing_additive item %q has blank detail", m.Key))
		}
		have[m] = struct{}{}
	}
	for _, m := range envelope {
		if _, ok := have[m]; !ok {
			return newInvalidError(invalidGrounding, ErrGrounding,
				fmt.Errorf("missing_additive drops envelope item %q (additive only)", m.Key))
		}
	}
	return nil
}

// checkSortedUniqueStrings requires a trimmed non-blank, sorted,
// duplicate-free ID list. It backs ModelRequest and TurnRecord validation.
func checkSortedUniqueStrings(ids []string, where string) error {
	for i := range ids {
		if strings.TrimSpace(ids[i]) == "" {
			return fmt.Errorf("orchestrate: %s has blank id at index %d: %w", where, i, ErrModelContract)
		}
		if ids[i] != strings.TrimSpace(ids[i]) {
			return fmt.Errorf("orchestrate: %s id must be trimmed: %w", where, ErrModelContract)
		}
	}
	if !slices.IsSorted(ids) {
		return fmt.Errorf("orchestrate: %s not sorted: %w", where, ErrModelContract)
	}
	for i := 1; i < len(ids); i++ {
		if ids[i] == ids[i-1] {
			return fmt.Errorf("orchestrate: %s duplicates id %q: %w", where, ids[i], ErrModelContract)
		}
	}
	return nil
}

// validHex64 reports whether s is a 64-character hex string (the
// request-hash shape TurnRecord carries).
func validHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// Closed turn error codes. Empty means the tool call succeeded; any other
// value names the terminal class of a recorded tool failure. The set is
// closed so AttemptLog rows stay machine-readable.
const (
	errorCodeOK       = ""
	errorCodeUpstream = "UPSTREAM"
	errorCodeDenied   = "DENIED"
	errorCodeContract = "CONTRACT"
	errorCodeBudget   = "BUDGET"
	errorCodeDeadline = "DEADLINE"
	errorCodeTenant   = "TENANT"
)

// validErrorCode reports whether code is one of the closed turn codes.
func validErrorCode(code string) bool {
	switch code {
	case errorCodeOK, errorCodeUpstream, errorCodeDenied, errorCodeContract,
		errorCodeBudget, errorCodeDeadline, errorCodeTenant:
		return true
	default:
		return false
	}
}

// errorCodeFor maps a tool failure onto the closed turn code using the
// sentinel chain (most specific first).
func errorCodeFor(err error) string {
	switch {
	case err == nil:
		return errorCodeOK
	case errors.Is(err, investigate.ErrTenantMismatch):
		return errorCodeTenant
	case errors.Is(err, investigate.ErrBudgetExceeded):
		return errorCodeBudget
	case errors.Is(err, investigate.ErrDeadlineExceeded):
		return errorCodeDeadline
	case errors.Is(err, investigate.ErrToolNotAllowed):
		return errorCodeDenied
	case errors.Is(err, investigate.ErrUpstream):
		return errorCodeUpstream
	default:
		return errorCodeContract
	}
}
