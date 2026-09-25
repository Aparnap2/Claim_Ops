package worker

// APA-31 bridge RED: per-key sufficiency gaps must reach the built
// envelope as structured MissingField items, not display-only message
// text. The leaf gate (internal/sufficiency) already exposes the gaps
// via Result.MissingItems; the wiring currently injects only
// SynthesizeFinding (an R8 whose AffectedFields projection is empty by
// the frozen invest taxonomy), so invest.Build derives no MissingField
// from it and the per-key gap is invisible to HITL/agent input.

import (
	"context"
	"testing"

	"claimops-api/internal/extract"
	"claimops-api/internal/invest"
)

func hasMissingField(env invest.UnresolvedException, key string) bool {
	for _, m := range env.MissingEvidence {
		if m.Kind == invest.MissingField && m.Key == key {
			return true
		}
	}
	return false
}

func TestProcessor_SufficiencyBridge_MissingKeyStructured(t *testing.T) {
	docID := "doc-suff-bridge-a"
	facts := extract.DocumentFacts{
		DocumentID: docID,
		DocType:    "CLAIM_FORM",
		Fields:     map[string]extract.ExtractedField{},
	}
	p, _ := suffProcessor(facts, "CLAIM_FORM", ClaimView{}, PolicyData{Active: true})
	out, _ := p.runNewPipeline(context.Background(), "t1", "c1", "abc123", docID, "CLAIM_FORM")
	if out.ExceptionEnvelope == nil {
		t.Fatal("insufficient evidence must produce an exception envelope (HITL input)")
	}
	env, err := invest.Decode(out.ExceptionEnvelope)
	if err != nil {
		t.Fatalf("Decode envelope: %v", err)
	}
	if !hasMissingField(env, "hospital_name") {
		t.Fatalf("envelope MissingEvidence lacks structured MissingField hospital_name; got %v", env.MissingEvidence)
	}
}
