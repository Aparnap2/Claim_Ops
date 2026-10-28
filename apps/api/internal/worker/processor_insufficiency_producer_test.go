package worker

// ADR 010 producer (APA-91 slice): the durable VALIDATING -> EXCEPTION
// transition is reachable ONLY through the Parser-gated new pipeline
// (runNewPipeline). This is the positive path the app/retrybudget Tier-1
// tests can never cover — they construct via NewProcessor with a nil Parser,
// so they only prove the producer stays untouched on the legacy path.
//
// These tests drive runNewPipeline directly with a genuinely insufficient
// CLAIM_FORM (every required key MISSING), which fires the sufficiency gate
// and must consult the ClaimLoader's durable producer exactly once, under the
// owning tenant, with the loaded claim version. The second case injects a
// non-retryable store fault (pgconn class 23) and proves fail-closed: no
// envelope, no launch, no PROCESSED.

import (
	"context"
	"strings"
	"testing"

	"claimops-api/internal/claims"
	"claimops-api/internal/documents"
	"claimops-api/internal/extract"

	"github.com/jackc/pgx/v5/pgconn"
)

func insuffProducerProcessor(facts extract.DocumentFacts, effective string, loader *fakeClaimLoader) *Processor {
	fetch := &fakeFetcher{fileName: "claim_form.pdf", mime: "application/pdf", content: "dummy content"}
	p := NewProcessor(fetch, &fakeStore{}, loader, &fakePolicyChecker{
		data: PolicyData{Number: "POL-1", Patient: "Alice", Active: true},
	})
	p.Parser = suffFakeParser{conf: 0.95, text: "dummy"}
	p.Extractors = map[string]extract.Extractor{effective: suffFakeExtractor{facts: facts}}
	return p
}

func TestProcessor_InsufficientEvidence_RecordsDurableTransition(t *testing.T) {
	const tenant, claim, docID, version = "t-apa91", "c-1", "doc-apa91-pos", 7
	facts := extract.DocumentFacts{DocumentID: docID, DocType: "CLAIM_FORM", Fields: map[string]extract.ExtractedField{}}
	loader := &fakeClaimLoader{view: ClaimView{PolicyNumber: "POL-1", ClaimVersion: version}}
	p := insuffProducerProcessor(facts, "CLAIM_FORM", loader)

	out, _ := p.runNewPipeline(context.Background(), tenant, claim, "abc123", docID, "CLAIM_FORM")

	if out.Status != documents.StProcessed || out.Kind != OutcomeSuccess {
		t.Fatalf("status=%q kind=%q, want PROCESSED/SUCCESS (insufficiency is HITL, not FAILED)", out.Status, out.Kind)
	}
	if len(loader.recTenants) != 1 {
		t.Fatalf("durable producer calls = %d, want exactly 1", len(loader.recTenants))
	}
	if loader.recTenants[0] != tenant || loader.recClaimIDs[0] != claim {
		t.Fatalf("producer scope = (%q,%q), want (%q,%q)", loader.recTenants[0], loader.recClaimIDs[0], tenant, claim)
	}
	if loader.recVersions[0] != version {
		t.Fatalf("producer expectedVersion = %d, want %d (the loaded claim version)", loader.recVersions[0], version)
	}
	ev := loader.recEventIDs[0]
	if ev == "" || !strings.Contains(ev, tenant) || !strings.Contains(ev, claim) || !strings.Contains(ev, docID) {
		t.Fatalf("producer eventID = %q, want the stable tenant/claim/doc identity", ev)
	}
	if out.ExceptionEnvelope == nil {
		t.Fatal("insufficient evidence must still yield the HITL exception envelope after the durable transition")
	}
}

func TestProcessor_InsufficientEvidence_ProducerFailureFailsClosed(t *testing.T) {
	const tenant, claim, docID = "t-apa91", "c-1", "doc-apa91-neg"
	facts := extract.DocumentFacts{DocumentID: docID, DocType: "CLAIM_FORM", Fields: map[string]extract.ExtractedField{}}
	// Class-23 integrity fault is not retryable: the pipeline must fail
	// closed rather than proceed with unrecorded durable claim state.
	loader := &fakeClaimLoader{
		view:   ClaimView{PolicyNumber: "POL-1", ClaimVersion: 3},
		recErr: &pgconn.PgError{Code: "23505"},
	}
	p := insuffProducerProcessor(facts, "CLAIM_FORM", loader)

	out, _ := p.runNewPipeline(context.Background(), tenant, claim, "abc123", docID, "CLAIM_FORM")

	if len(loader.recTenants) != 1 {
		t.Fatalf("durable producer calls = %d, want exactly 1 (consulted before failing closed)", len(loader.recTenants))
	}
	if out.Kind != OutcomeTerminal {
		t.Fatalf("kind=%q, want TERMINAL (non-retryable store fault fails closed)", out.Kind)
	}
	if out.Status != documents.StFailed {
		t.Fatalf("status=%q, want FAILED (never PROCESSED on an unrecorded durable transition)", out.Status)
	}
	if out.ExceptionEnvelope != nil {
		t.Fatal("must not build an envelope when the durable claim state could not be recorded")
	}
}

func TestProcessor_InsufficientEvidence_SupersededAckedNoLaunch(t *testing.T) {
	const tenant, claim, docID = "t-apa91", "c-1", "doc-apa91-sup"
	facts := extract.DocumentFacts{DocumentID: docID, DocType: "CLAIM_FORM", Fields: map[string]extract.ExtractedField{}}
	// The claim already carries this event AND has advanced past EXCEPTION
	// (a human/lifecycle decision). The producer refuses to re-authorize;
	// the redelivery must be acked with no envelope and no launch.
	loader := &fakeClaimLoader{
		view:   ClaimView{PolicyNumber: "POL-1", ClaimVersion: 9},
		recErr: ErrTransitionSuperseded,
	}
	p := insuffProducerProcessor(facts, "CLAIM_FORM", loader)

	out, _ := p.runNewPipeline(context.Background(), tenant, claim, "abc123", docID, "CLAIM_FORM")

	if out.Kind != OutcomeSuccess || out.Status != documents.StProcessed {
		t.Fatalf("status=%q kind=%q, want PROCESSED/SUCCESS (superseded duplicate is acked, not retried)", out.Status, out.Kind)
	}
	if out.ExceptionEnvelope != nil {
		t.Fatal("a superseded replay must not re-open an exception envelope")
	}
}

// TestProcessor_InsufficientEvidence_EventIdentityIsReasonScoped proves the
// ADR 010 event identity is scoped to the sufficiency outcome, not just the
// document: two different insufficiency reasons for the same document yield
// distinct event IDs, so a reprocessed document with a materially different
// insufficiency result cannot be silently replayed as the first.
func TestProcessor_InsufficientEvidence_EventIdentityIsReasonScoped(t *testing.T) {
	const tenant, claim, version = "t-apa91", "c-1", 7

	// Case A: missing required field (hospital_name dropped) -> MISSING_REQUIRED_FIELD.
	factsA := suffSufficientClaimForm("doc-apa91-id")
	delete(factsA.Fields, "hospital_name")
	factsA.Fields["hospital_name"] = suffMissingField("hospital_name")

	// Case B: ambiguous field (policy_number ambiguous) -> AMBIGUOUS_FIELD,
	// a materially different insufficiency reason set.
	factsB := suffSufficientClaimForm("doc-apa91-id")
	f := factsB.Fields["policy_number"]
	f.Status = extract.StatusAmbiguous
	f.Normalized = ""
	factsB.Fields["policy_number"] = f

	loaderA := &fakeClaimLoader{view: ClaimView{PolicyNumber: "POL-1", ClaimVersion: version}}
	pA := insuffProducerProcessor(factsA, "CLAIM_FORM", loaderA)
	outA, _ := pA.runNewPipeline(context.Background(), tenant, claim, "abc123", "doc-apa91-id", "CLAIM_FORM")
	if outA.Status != documents.StProcessed {
		t.Fatalf("case A status=%q, want PROCESSED (err=%v)", outA.Status, outA.Err)
	}
	if len(loaderA.recEventIDs) != 1 {
		t.Fatalf("case A producer calls = %d, want 1", len(loaderA.recEventIDs))
	}
	idA := loaderA.recEventIDs[0]

	loaderB := &fakeClaimLoader{view: ClaimView{PolicyNumber: "POL-1", ClaimVersion: version}}
	pB := insuffProducerProcessor(factsB, "CLAIM_FORM", loaderB)
	outB, _ := pB.runNewPipeline(context.Background(), tenant, claim, "abc123", "doc-apa91-id", "CLAIM_FORM")
	if outB.Status != documents.StProcessed {
		t.Fatalf("case B status=%q, want PROCESSED (err=%v)", outB.Status, outB.Err)
	}
	if len(loaderB.recEventIDs) != 1 {
		t.Fatalf("case B producer calls = %d, want 1", len(loaderB.recEventIDs))
	}
	idB := loaderB.recEventIDs[0]

	if idA == idB {
		t.Fatalf("event IDs must differ for materially different insufficiency outcomes; both = %q", idA)
	}
	for _, id := range []string{idA, idB} {
		if !strings.Contains(id, tenant) || !strings.Contains(id, claim) || !strings.Contains(id, "doc-apa91-id") {
			t.Fatalf("event id %q missing tenant/claim/doc identity", id)
		}
	}
}

// TestProcessor_InsufficientEvidence_IdenticalBundleReplays verifies that two
// runs with the same insufficiency outcome produce the same event identity,
// so redelivery is idempotent at the producer level.
func TestProcessor_InsufficientEvidence_IdenticalBundleReplays(t *testing.T) {
	const tenant, claim, version = "t-apa91", "c-1", 7
	makeFacts := func() extract.DocumentFacts {
		facts := suffSufficientClaimForm("doc-apa91-replay")
		delete(facts.Fields, "hospital_name")
		facts.Fields["hospital_name"] = suffMissingField("hospital_name")
		return facts
	}
	loader1 := &fakeClaimLoader{view: ClaimView{PolicyNumber: "POL-1", ClaimVersion: version}}
	p1 := insuffProducerProcessor(makeFacts(), "CLAIM_FORM", loader1)
	p1.runNewPipeline(context.Background(), tenant, claim, "abc123", "doc-apa91-replay", "CLAIM_FORM")
	loader2 := &fakeClaimLoader{view: ClaimView{PolicyNumber: "POL-1", ClaimVersion: version}}
	p2 := insuffProducerProcessor(makeFacts(), "CLAIM_FORM", loader2)
	p2.runNewPipeline(context.Background(), tenant, claim, "abc123", "doc-apa91-replay", "CLAIM_FORM")
	if len(loader1.recEventIDs) != 1 || len(loader2.recEventIDs) != 1 {
		t.Fatalf("producer calls = %d/%d, want 1/1", len(loader1.recEventIDs), len(loader2.recEventIDs))
	}
	if loader1.recEventIDs[0] != loader2.recEventIDs[0] {
		t.Fatalf("identical insufficiency outcomes produced different event IDs: %q vs %q", loader1.recEventIDs[0], loader2.recEventIDs[0])
	}
}

// TestProcessor_InsufficientEvidence_IllegalTransitionAcked (review F1/F2):
// a claim outside the producer's authorized edge rejects the transition with
// CodeIllegalTransition — already at EXCEPTION from a prior insufficiency
// (second insufficient document on the same claim), or still pre-validation
// (RECEIVED; deliberate fail-closed until the intake progression slice). The
// state is deterministic, so the delivery must be ACKED (PROCESSED/SUCCESS,
// no envelope, no FAILED row) — never TRANSIENT, which would poison-loop the
// redelivery to the DLQ.
func TestProcessor_InsufficientEvidence_IllegalTransitionAcked(t *testing.T) {
	const tenant, claim, docID = "t-apa91", "c-1", "doc-apa91-illegal"
	facts := extract.DocumentFacts{DocumentID: docID, DocType: "CLAIM_FORM", Fields: map[string]extract.ExtractedField{}}

	cases := []struct {
		name string
		from claims.ClaimStatus
	}{
		{"claim already at EXCEPTION (second insufficient doc)", claims.ClaimStatusException},
		{"claim still pre-validation at RECEIVED (fail-closed pre-D1)", claims.ClaimStatusReceived},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			loader := &fakeClaimLoader{
				view: ClaimView{PolicyNumber: "POL-1", ClaimVersion: 4},
				recErr: &claims.TransitionError{
					Code:    claims.CodeIllegalTransition,
					ClaimID: claim,
					From:    string(tc.from),
					To:      string(claims.ClaimStatusException),
				},
			}
			p := insuffProducerProcessor(facts, "CLAIM_FORM", loader)

			out, _ := p.runNewPipeline(context.Background(), tenant, claim, "abc123", docID, "CLAIM_FORM")

			if len(loader.recTenants) != 1 {
				t.Fatalf("durable producer calls = %d, want exactly 1 (no retry loop on a deterministic verdict)", len(loader.recTenants))
			}
			if out.Kind != OutcomeSuccess || out.Status != documents.StProcessed {
				t.Fatalf("status=%q kind=%q, want PROCESSED/SUCCESS (illegal transition is acked, not retried)", out.Status, out.Kind)
			}
			if out.ExceptionEnvelope != nil {
				t.Fatal("an illegal-transition ack must not re-open an exception envelope")
			}
		})
	}
}
