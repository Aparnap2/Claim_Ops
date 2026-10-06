// APA-54: the model-facing tool/knob ownership contract.
//
// The authoritative ownership mapping lives in tool.go's package doc
// (lines 22-35) and is enforced by checkKnob (tool.go:269-280) via
// Request.Validate (tool.go:229-247). This file pins two things the
// audit found unasserted:
//
//	(a) get_evidence(subject_id=<document_id>) is rejected at the
//	    ownership layer — the previously-unasserted I4 case. A DocumentID
//	    from get_documents is NOT a valid subject_id, and get_evidence
//	    does not accept subject_id at all.
//	(b) A valid subject_id on each owning tool is accepted at the
//	    ownership layer (Request.Validate).
//
// Nothing here changes enforcement: checkKnob, the decoder, grounding,
// and the loop are untouched. These are pins of existing semantics.
package investigate

import (
	"context"
	"errors"
	"testing"
	"time"

	"claimops-api/internal/invest"
)

// TestKnobOwnership_GetEvidenceRejectsSubjectID is the previously-unasserted
// I4 ownership rejection: a subject_id (here a document id, the exact value
// the observed Qwen failure produced) set on get_evidence must fail closed
// at the executor gate, and the backend must not be invoked.
func TestKnobOwnership_GetEvidenceRejectsSubjectID(t *testing.T) {
	ctx := context.Background()
	var called bool
	ex := NewExecutor(map[invest.ToolName]ToolFunc{
		invest.ToolGetEvidence: xStub(&called, Response{RowCount: 1, IDs: []string{"ev-01"}}),
	}, time.Time{})
	req := xReq(invest.ToolGetEvidence)
	req.SubjectID = "doc-01" // get_evidence owns no subject_id knob
	if _, err := ex.Execute(ctx, xScope(invest.ToolGetEvidence), invest.ToolGetEvidence, req); !errors.Is(err, ErrContract) {
		t.Fatalf("err = %v, want ErrContract", err)
	}
	if called {
		t.Error("backend invoked for knob-violating request")
	}
	if ex.Calls() != 0 {
		t.Errorf("Calls() = %d, want 0", ex.Calls())
	}
}

// TestKnobOwnership_ValidSubjectIDAccepted pins that a valid subject_id is
// accepted at the ownership layer (Request.Validate) on every tool that
// owns the knob, and rejected on get_evidence, which does not.
func TestKnobOwnership_ValidSubjectIDAccepted(t *testing.T) {
	valid := map[invest.ToolName]string{
		invest.ToolGetPolicyContext:          "pol-0123456789abcdef0123456789abcdef",
		invest.ToolGetExternalPolicyStatus:   "pol-0123456789abcdef0123456789abcdef",
		invest.ToolGetTPACase:                "case-0123456789abcdef0123456789abcdef",
		invest.ToolGetProviderEncounter:      "enc-0123456789abcdef0123456789abcdef",
		invest.ToolCreateInvestigationReport: "ex-0123456789abcdef0123456789abcdef",
	}
	for tool, subjectID := range valid {
		req := xReq(tool)
		req.SubjectID = subjectID
		if err := req.Validate(); err != nil {
			t.Errorf("Validate() for %s with subject_id = %v, want nil", tool, err)
		}
	}
	// get_evidence does not own subject_id: the same value must be refused.
	req := xReq(invest.ToolGetEvidence)
	req.SubjectID = "doc-01"
	if err := req.Validate(); !errors.Is(err, ErrContract) {
		t.Errorf("Validate() for get_evidence with subject_id = %v, want ErrContract", err)
	}
}
