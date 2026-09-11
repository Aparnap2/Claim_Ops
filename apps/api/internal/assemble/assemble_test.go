// Deterministic assembly tests: hand-built extract.DocumentFacts only,
// no fixtures, no network, no I/O, no clock reads. Synthetic identifiers
// throughout (no PII/PHI).
package assemble_test

import (
	"reflect"
	"testing"

	"claimops-api/internal/assemble"
	"claimops-api/internal/extract"
)

// present builds a PRESENT observation for key.
func present(key, value, normalized, extractor string, ev extract.EvidenceRef) extract.ExtractedField {
	return extract.ExtractedField{
		Key: key, Value: value, Normalized: normalized,
		Evidence: ev, Status: extract.StatusPresent,
		Extractor: extractor, ExtractorVersion: "0.0.1",
	}
}

// missing builds a MISSING observation for key.
func missing(key string) extract.ExtractedField {
	return extract.ExtractedField{
		Key: key, Status: extract.StatusMissing,
		Extractor: "stub", ExtractorVersion: "0.0.1",
	}
}

// ambiguous builds an AMBIGUOUS observation preserving raw.
func ambiguous(key, raw string, ev extract.EvidenceRef) extract.ExtractedField {
	return extract.ExtractedField{
		Key: key, Value: raw,
		Evidence: ev, Status: extract.StatusAmbiguous,
		Extractor: "stub", ExtractorVersion: "0.0.1",
	}
}

// multi builds a MULTI_CANDIDATE observation with verbatim candidates.
func multi(key string, cands []extract.Candidate, ev extract.EvidenceRef) extract.ExtractedField {
	return extract.ExtractedField{
		Key: key, Status: extract.StatusMultiCandidate,
		Candidates: cands, Evidence: ev,
		Extractor: "stub", ExtractorVersion: "0.0.1",
	}
}

// facts builds one document's facts from field observations.
func facts(docID, docType string, fields ...extract.ExtractedField) extract.DocumentFacts {
	m := make(map[string]extract.ExtractedField, len(fields))
	for _, f := range fields {
		m[f.Key] = f
	}
	return extract.DocumentFacts{DocumentID: docID, DocType: docType, Fields: m}
}

func ev(docID, block string, page int) extract.EvidenceRef {
	return extract.EvidenceRef{DocumentID: docID, Page: page, BlockID: block}
}

// mustAssemble fails the test on error.
func mustAssemble(t *testing.T, docs []extract.DocumentFacts) assemble.CanonicalClaim {
	t.Helper()
	got, err := assemble.Assemble(docs)
	if err != nil {
		t.Fatalf("Assemble = error %v", err)
	}
	return got
}

// 1: single-source PRESENT -> AGREED with value, raw, one source.
func TestSingleSourcePresentAgrees(t *testing.T) {
	got := mustAssemble(t, []extract.DocumentFacts{
		facts("doc-a", "CLAIM_FORM",
			present("policy_number", "POL-1001", "POL-1001", "stub", ev("doc-a", "b1", 1))),
	})
	f := got.Fields["policy_number"]
	if f.Status != assemble.StatusAgreed {
		t.Fatalf("status = %q, want AGREED", f.Status)
	}
	if f.Agreed != "POL-1001" || f.AgreedRaw != "POL-1001" {
		t.Fatalf("agreed = %q raw = %q, want POL-1001/POL-1001", f.Agreed, f.AgreedRaw)
	}
	if len(f.Sources) != 1 || len(f.NeedsReview) != 0 {
		t.Fatalf("sources = %d review = %d, want 1/0", len(f.Sources), len(f.NeedsReview))
	}
	if len(got.Conflicts) != 0 {
		t.Fatalf("conflicts = %+v, want none", got.Conflicts)
	}
	if src := f.Sources[0]; src.DocType != "CLAIM_FORM" {
		t.Fatalf("source DocType = %q, want CLAIM_FORM (F11a)", src.DocType)
	}
}

// 2: multi-doc identical normalized incl. case/whitespace variants -> AGREED.
func TestMultiDocIdenticalNormalizedAgrees(t *testing.T) {
	got := mustAssemble(t, []extract.DocumentFacts{
		facts("doc-a", "CLAIM_FORM",
			present("policy_number", "POL-1001", "POL-1001", "stub", ev("doc-a", "b1", 1))),
		facts("doc-b", "DISCHARGE_SUMMARY",
			present("policy_number", "pol-1001", "pol-1001", "stub", ev("doc-b", "b2", 1))),
		facts("doc-c", "HOSPITAL_BILL",
			present("policy_number", "  POL-1001 ", "POL-1001", "stub", ev("doc-c", "b3", 2))),
	})
	f := got.Fields["policy_number"]
	if f.Status != assemble.StatusAgreed {
		t.Fatalf("status = %q, want AGREED (ID fold: upper(trimmed))", f.Status)
	}
	// Stored Agreed keeps the sorted-first source's Normalized unchanged.
	if f.Agreed != "POL-1001" || f.AgreedRaw != "  POL-1001 " {
		t.Fatalf("agreed = %q raw = %q, want sorted-first POL-1001/\"  POL-1001 \"", f.Agreed, f.AgreedRaw)
	}
	if len(f.Sources) != 3 {
		t.Fatalf("sources = %d, want 3", len(f.Sources))
	}
	if len(got.Conflicts) != 0 {
		t.Fatalf("conflicts = %+v, want none", got.Conflicts)
	}
}

// 3: different values -> CONFLICT with sorted Distinct + all Sources.
func TestDifferentValuesConflict(t *testing.T) {
	got := mustAssemble(t, []extract.DocumentFacts{
		facts("doc-b", "DISCHARGE_SUMMARY",
			present("claim_number", "CLM-9999", "CLM-9999", "stub", ev("doc-b", "b9", 1))),
		facts("doc-a", "CLAIM_FORM",
			present("claim_number", "CLM-2002", "CLM-2002", "stub", ev("doc-a", "b1", 1))),
	})
	f := got.Fields["claim_number"]
	if f.Status != assemble.StatusConflict {
		t.Fatalf("status = %q, want CONFLICT", f.Status)
	}
	if f.Agreed != "" || f.AgreedRaw != "" {
		t.Fatalf("agreed = %q raw = %q, want empty under CONFLICT", f.Agreed, f.AgreedRaw)
	}
	if len(got.Conflicts) != 1 {
		t.Fatalf("conflicts = %d, want 1", len(got.Conflicts))
	}
	c := got.Conflicts[0]
	if c.Key != "claim_number" {
		t.Fatalf("conflict key = %q, want claim_number", c.Key)
	}
	wantDistinct := []string{"CLM-2002", "CLM-9999"}
	if !reflect.DeepEqual(c.Distinct, wantDistinct) {
		t.Fatalf("distinct = %q, want %q (sorted)", c.Distinct, wantDistinct)
	}
	if len(c.Sources) != 2 || len(f.Sources) != 2 {
		t.Fatalf("sources = %d/%d, want 2/2", len(c.Sources), len(f.Sources))
	}
	// Sources sorted by (Normalized, Value, DocumentID, Page, BlockID).
	if c.Sources[0].Normalized != "CLM-2002" || c.Sources[1].Normalized != "CLM-9999" {
		t.Fatalf("sources not sorted: %+v", c.Sources)
	}
}

// 4: AMBIGUOUS survives verbatim into NeedsReview, casts no vote.
func TestAmbiguousSurvives(t *testing.T) {
	got := mustAssemble(t, []extract.DocumentFacts{
		facts("doc-a", "CLAIM_FORM",
			ambiguous("admission_date", "sometime last week", ev("doc-a", "b1", 1))),
	})
	f := got.Fields["admission_date"]
	if f.Status != assemble.StatusNeedsReview {
		t.Fatalf("status = %q, want NEEDS_REVIEW (review with no votes)", f.Status)
	}
	if len(f.Sources) != 0 {
		t.Fatalf("sources = %d, want 0 (AMBIGUOUS casts no vote)", len(f.Sources))
	}
	if len(f.NeedsReview) != 1 {
		t.Fatalf("review = %d, want 1", len(f.NeedsReview))
	}
	r := f.NeedsReview[0]
	if r.Status != extract.StatusAmbiguous || r.Value != "sometime last week" {
		t.Fatalf("review not verbatim: %+v", r)
	}
	if r.DocumentID != "doc-a" || r.DocType != "CLAIM_FORM" {
		t.Fatalf("review provenance lost: %+v", r)
	}
	if len(got.Conflicts) != 0 {
		t.Fatalf("conflicts = %+v, want none", got.Conflicts)
	}
}

// 5: MULTI candidates survive verbatim in encounter order, no vote.
func TestMultiSurvivesVerbatim(t *testing.T) {
	cands := []extract.Candidate{
		{Value: "CLM-2002", Normalized: "CLM-2002", Evidence: ev("doc-a", "b1", 1)},
		{Value: "CLM-2003", Normalized: "CLM-2003", Evidence: ev("doc-a", "b7", 2)},
	}
	got := mustAssemble(t, []extract.DocumentFacts{
		facts("doc-a", "CLAIM_FORM", multi("claim_number", cands, ev("doc-a", "b1", 1))),
	})
	f := got.Fields["claim_number"]
	if f.Status != assemble.StatusNeedsReview {
		t.Fatalf("status = %q, want NEEDS_REVIEW", f.Status)
	}
	if len(f.Sources) != 0 {
		t.Fatalf("sources = %d, want 0 (MULTI casts no vote)", len(f.Sources))
	}
	if len(f.NeedsReview) != 1 {
		t.Fatalf("review = %d, want 1", len(f.NeedsReview))
	}
	if !reflect.DeepEqual(f.NeedsReview[0].Candidates, cands) {
		t.Fatalf("candidates = %+v, want verbatim %+v", f.NeedsReview[0].Candidates, cands)
	}
	if len(got.Conflicts) != 0 {
		t.Fatalf("conflicts = %+v, want none", got.Conflicts)
	}
}

// 6: missing+present mix -> PRESENT decides, MISSING ignored.
func TestMissingPresentMix(t *testing.T) {
	got := mustAssemble(t, []extract.DocumentFacts{
		facts("doc-a", "CLAIM_FORM", missing("policy_number")),
		facts("doc-b", "HOSPITAL_BILL",
			present("policy_number", "POL-1001", "POL-1001", "stub", ev("doc-b", "b1", 1))),
	})
	f := got.Fields["policy_number"]
	if f.Status != assemble.StatusAgreed || f.Agreed != "POL-1001" {
		t.Fatalf("got %+v, want AGREED POL-1001", f)
	}
	if len(f.Sources) != 1 {
		t.Fatalf("sources = %d, want 1 (MISSING contributes nothing)", len(f.Sources))
	}
}

// 7: normalization-equivalent agreement (same Normalized, different raw).
func TestNormalizationEquivalentAgreement(t *testing.T) {
	got := mustAssemble(t, []extract.DocumentFacts{
		facts("doc-a", "CLAIM_FORM",
			present("patient_name", "Aparna Pradhan", "aparna pradhan", "stub", ev("doc-a", "b1", 1))),
		facts("doc-b", "DISCHARGE_SUMMARY",
			present("patient_name", "aparna  pradhan", "aparna pradhan", "stub", ev("doc-b", "b2", 1))),
	})
	f := got.Fields["patient_name"]
	if f.Status != assemble.StatusAgreed {
		t.Fatalf("status = %q, want AGREED", f.Status)
	}
	if f.Agreed != "aparna pradhan" {
		t.Fatalf("agreed = %q, want aparna pradhan", f.Agreed)
	}
	// AgreedRaw is the sorted-first source's raw.
	if f.AgreedRaw != "Aparna Pradhan" {
		t.Fatalf("raw = %q, want sorted-first raw Aparna Pradhan", f.AgreedRaw)
	}
}

// 8: same value, multiple evidence locations -> AGREED with all sources.
func TestSameValueMultipleEvidence(t *testing.T) {
	got := mustAssemble(t, []extract.DocumentFacts{
		facts("doc-a", "HOSPITAL_BILL",
			present("total_bill", "Rs. 10.00", "1000", "stub", ev("doc-a", "b1", 1)),
			present("admission_date", "2026-01-05", "2026-01-05", "stub", ev("doc-a", "b9", 3))),
	})
	f := got.Fields["total_bill"]
	if f.Status != assemble.StatusAgreed || f.Agreed != "1000" {
		t.Fatalf("got %+v, want AGREED 1000", f)
	}
	// bill_lines F11b guard: descriptions-only lines still assemble as
	// plain keys; no amount recovery is attempted (no such key here, but
	// the path must not error on multi-field docs).
	if got.Fields["admission_date"].Status != assemble.StatusAgreed {
		t.Fatalf("admission_date = %+v, want AGREED", got.Fields["admission_date"])
	}
}

// 8b: same normalized across two evidence pins in different docs.
func TestSameValueTwoDocsTwoPins(t *testing.T) {
	got := mustAssemble(t, []extract.DocumentFacts{
		facts("doc-a", "CLAIM_FORM",
			present("admission_date", "2026-01-05", "2026-01-05", "stub", ev("doc-a", "b1", 1))),
		facts("doc-b", "DISCHARGE_SUMMARY",
			present("admission_date", "2026-01-05", "2026-01-05", "stub", ev("doc-b", "b4", 2))),
	})
	f := got.Fields["admission_date"]
	if f.Status != assemble.StatusAgreed {
		t.Fatalf("status = %q, want AGREED", f.Status)
	}
	if len(f.Sources) != 2 {
		t.Fatalf("sources = %d, want 2 (both pins kept)", len(f.Sources))
	}
}

// 9: duplicate identical DocumentFacts input -> still AGREED, no conflict.
func TestDuplicateIdenticalInput(t *testing.T) {
	dup := facts("doc-a", "CLAIM_FORM",
		present("policy_number", "POL-1001", "POL-1001", "stub", ev("doc-a", "b1", 1)))
	got := mustAssemble(t, []extract.DocumentFacts{dup, dup})
	f := got.Fields["policy_number"]
	if f.Status != assemble.StatusAgreed || f.Agreed != "POL-1001" {
		t.Fatalf("got %+v, want AGREED POL-1001 (no spurious conflict)", f)
	}
	if len(got.Conflicts) != 0 {
		t.Fatalf("conflicts = %+v, want none", got.Conflicts)
	}
}

// AGREED votes shadowed by a review item -> NEEDS_REVIEW, never clean input.
func TestShadowedAgreementNeedsReview(t *testing.T) {
	got := mustAssemble(t, []extract.DocumentFacts{
		facts("doc-a", "CLAIM_FORM",
			present("admission_date", "2026-01-05", "2026-01-05", "stub", ev("doc-a", "b1", 1))),
		facts("doc-b", "DISCHARGE_SUMMARY",
			present("admission_date", "2026-01-05", "2026-01-05", "stub", ev("doc-b", "b2", 1))),
		facts("doc-c", "HOSPITAL_BILL",
			ambiguous("admission_date", "05/01/2026?", ev("doc-c", "b3", 1))),
	})
	f := got.Fields["admission_date"]
	if f.Status != assemble.StatusNeedsReview {
		t.Fatalf("status = %q, want NEEDS_REVIEW (shadowed agreement)", f.Status)
	}
	// Agreement still recorded for display, but the #47 gate
	// (AGREED && no review) blocks typed mapping.
	if f.Agreed != "2026-01-05" || len(f.Sources) != 2 || len(f.NeedsReview) != 1 {
		t.Fatalf("got %+v, want agreed 2026-01-05 with 2 sources + 1 review", f)
	}
	if len(got.Conflicts) != 0 {
		t.Fatalf("conflicts = %+v, want none (agreement holds)", got.Conflicts)
	}
}

// 10: multi-conflict docs with input-order reversal -> identical output.
func TestInputOrderReversalDeterministic(t *testing.T) {
	cands := []extract.Candidate{
		{Value: "CLM-2002", Normalized: "CLM-2002", Evidence: ev("doc-c", "b1", 1)},
		{Value: "CLM-2003", Normalized: "CLM-2003", Evidence: ev("doc-c", "b2", 1)},
	}
	forward := []extract.DocumentFacts{
		facts("doc-a", "CLAIM_FORM",
			present("policy_number", "POL-1001", "POL-1001", "stub", ev("doc-a", "b1", 1)),
			present("claim_number", "CLM-2002", "CLM-2002", "stub", ev("doc-a", "b2", 1)),
			ambiguous("admission_date", "illegible", ev("doc-a", "b3", 1))),
		facts("doc-b", "DISCHARGE_SUMMARY",
			present("policy_number", "POL-7777", "POL-7777", "stub", ev("doc-b", "b1", 2)),
			present("claim_number", "CLM-5555", "CLM-5555", "stub", ev("doc-b", "b2", 1)),
			present("admission_date", "2026-01-05", "2026-01-05", "stub", ev("doc-b", "b3", 1))),
		facts("doc-c", "HOSPITAL_BILL",
			present("policy_number", "POL-1001", "POL-1001", "stub", ev("doc-c", "b5", 1)),
			multi("claim_number", cands, ev("doc-c", "b1", 1)),
			missing("admission_date")),
	}
	reversed := []extract.DocumentFacts{forward[2], forward[1], forward[0]}

	first := mustAssemble(t, forward)
	again := mustAssemble(t, forward)
	if !reflect.DeepEqual(first, again) {
		t.Fatalf("repeat Assemble differs:\n%+v\nvs\n%+v", first, again)
	}
	flipped := mustAssemble(t, reversed)
	if !reflect.DeepEqual(first, flipped) {
		t.Fatalf("reversed input differs:\n%+v\nvs\n%+v", first, flipped)
	}
	// The fixture really does exercise conflicts on two keys.
	if len(first.Conflicts) != 2 {
		t.Fatalf("conflicts = %d, want 2 (policy_number + claim_number)", len(first.Conflicts))
	}
	if first.Conflicts[0].Key != "claim_number" || first.Conflicts[1].Key != "policy_number" {
		t.Fatalf("conflicts not sorted by key: %+v", first.Conflicts)
	}
}

// Multi-vote AGREED reversal: Agreed/AgreedRaw derive from sorted
// Sources[0], so forward vs reversed input yields identical output
// INCLUDING Agreed/AgreedRaw (byte-stability for the
// UnresolvedException envelope).
func TestMultiVoteAgreedReversalStable(t *testing.T) {
	forward := []extract.DocumentFacts{
		facts("doc-a", "CLAIM_FORM",
			present("policy_number", "pol-1001", "pol-1001", "stub", ev("doc-a", "b1", 1)),
			present("patient_name", "aparna  pradhan", "aparna pradhan", "stub", ev("doc-a", "b2", 1))),
		facts("doc-b", "DISCHARGE_SUMMARY",
			present("policy_number", "POL-1001", "POL-1001", "stub", ev("doc-b", "b1", 1)),
			present("patient_name", "Aparna Pradhan", "aparna pradhan", "stub", ev("doc-b", "b2", 1))),
	}
	reversed := []extract.DocumentFacts{forward[1], forward[0]}

	fwd := mustAssemble(t, forward)
	rev := mustAssemble(t, reversed)
	if !reflect.DeepEqual(fwd, rev) {
		t.Fatalf("multi-vote AGREED reversal differs:\n%+v\nvs\n%+v", fwd, rev)
	}
	// ID key: fold is upper(trimmed); representative is sorted-first, so
	// "POL-1001" (0x50) sorts before "pol-1001" (0x70).
	if f := fwd.Fields["policy_number"]; f.Status != assemble.StatusAgreed ||
		f.Agreed != "POL-1001" || f.AgreedRaw != "POL-1001" {
		t.Fatalf("policy_number = %+v, want AGREED POL-1001/POL-1001 (sorted-first)", f)
	}
	// Non-ID key: Normalized ties, so raw sort decides ("Aparna..." <
	// "aparna...").
	if f := fwd.Fields["patient_name"]; f.Status != assemble.StatusAgreed ||
		f.Agreed != "aparna pradhan" || f.AgreedRaw != "Aparna Pradhan" {
		t.Fatalf("patient_name = %+v, want AGREED aparna pradhan/Aparna Pradhan (sorted-first)", f)
	}
}

// Empty input -> empty claim, no conflicts, no error.
func TestEmptyInput(t *testing.T) {
	got := mustAssemble(t, nil)
	if len(got.Fields) != 0 || len(got.Conflicts) != 0 {
		t.Fatalf("got %+v, want empty claim", got)
	}
	if len(got.DocsPresent) != 0 || len(got.DocTypes) != 0 || len(got.DocIDs) != 0 {
		t.Fatalf("got docs %+v, want none", got)
	}
}

// All sources MISSING -> MISSING with empty Sources/NeedsReview, no conflict.
func TestAllMissing(t *testing.T) {
	got := mustAssemble(t, []extract.DocumentFacts{
		facts("doc-a", "CLAIM_FORM", missing("policy_number")),
		facts("doc-b", "HOSPITAL_BILL", missing("policy_number")),
	})
	f := got.Fields["policy_number"]
	if f.Status != assemble.StatusMissing {
		t.Fatalf("status = %q, want MISSING", f.Status)
	}
	if len(f.Sources) != 0 || len(f.NeedsReview) != 0 {
		t.Fatalf("MISSING carries content: %+v", f)
	}
	if f.Agreed != "" || f.AgreedRaw != "" {
		t.Fatalf("MISSING carries agreement: %+v", f)
	}
	if len(got.Conflicts) != 0 {
		t.Fatalf("conflicts = %+v, want none", got.Conflicts)
	}
}

// DocsPresent holds exactly the consumed DocTypes; DocTypes/DocIDs sorted.
func TestDocsPresentOnlyConsumed(t *testing.T) {
	got := mustAssemble(t, []extract.DocumentFacts{
		facts("doc-z", "HOSPITAL_BILL",
			present("policy_number", "POL-1001", "POL-1001", "stub", ev("doc-z", "b1", 1))),
		facts("doc-a", "CLAIM_FORM", missing("policy_number")),
	})
	if !reflect.DeepEqual(got.DocsPresent, map[string]bool{"HOSPITAL_BILL": true, "CLAIM_FORM": true}) {
		t.Fatalf("DocsPresent = %v, want both consumed types", got.DocsPresent)
	}
	wantTypes := []string{"CLAIM_FORM", "HOSPITAL_BILL"}
	if !reflect.DeepEqual(got.DocTypes, wantTypes) {
		t.Fatalf("DocTypes = %q, want %q (sorted)", got.DocTypes, wantTypes)
	}
	wantIDs := []string{"doc-a", "doc-z"}
	if !reflect.DeepEqual(got.DocIDs, wantIDs) {
		t.Fatalf("DocIDs = %q, want %q (sorted)", got.DocIDs, wantIDs)
	}
}

// R8-relevant types pass through verbatim; no R8 judgment is implemented
// (a missing required type is NOT an error here).
func TestR8TypesPassThrough(t *testing.T) {
	got := mustAssemble(t, []extract.DocumentFacts{
		facts("doc-a", "CLAIM_FORM",
			present("policy_number", "POL-1001", "POL-1001", "stub", ev("doc-a", "b1", 1))),
		facts("doc-p", "PREAUTH",
			present("policy_number", "POL-1001", "POL-1001", "stub", ev("doc-p", "b1", 1))),
	})
	if !got.DocsPresent["CLAIM_FORM"] || !got.DocsPresent["PREAUTH"] {
		t.Fatalf("DocsPresent = %v, want CLAIM_FORM + PREAUTH passthrough", got.DocsPresent)
	}
	for _, src := range got.Fields["policy_number"].Sources {
		if src.DocType == "" {
			t.Fatalf("source lost DocType: %+v", src)
		}
	}
	// Only CLAIM_FORM present, DISCHARGE_SUMMARY/HOSPITAL_BILL absent: that
	// is #47/R8's concern, not an assembly error.
	if got.Fields["policy_number"].Status != assemble.StatusAgreed {
		t.Fatalf("got %+v, want AGREED", got.Fields["policy_number"])
	}
}

// bill_lines_N keys assemble per-N independently (positional, not semantic).
func TestBillLinesPerNIndependent(t *testing.T) {
	got := mustAssemble(t, []extract.DocumentFacts{
		facts("doc-a", "HOSPITAL_BILL",
			present("bill_lines_0", "Room charges", "room charges", "stub", ev("doc-a", "b1", 1)),
			present("bill_lines_1", "Pharmacy", "pharmacy", "stub", ev("doc-a", "b2", 1))),
		facts("doc-b", "HOSPITAL_BILL",
			present("bill_lines_0", "Room charges", "room charges", "stub", ev("doc-b", "b1", 1)),
			present("bill_lines_1", "Surgery", "surgery", "stub", ev("doc-b", "b2", 1))),
	})
	if got.Fields["bill_lines_0"].Status != assemble.StatusAgreed {
		t.Fatalf("bill_lines_0 = %+v, want AGREED", got.Fields["bill_lines_0"])
	}
	// N=1 differs across docs -> CONFLICT on that N only; no cross-N logic.
	if got.Fields["bill_lines_1"].Status != assemble.StatusConflict {
		t.Fatalf("bill_lines_1 = %+v, want CONFLICT", got.Fields["bill_lines_1"])
	}
}

// Validation: blank DocumentID and unknown status are terminal errors.
func TestValidationErrors(t *testing.T) {
	if _, err := assemble.Assemble([]extract.DocumentFacts{{DocType: "CLAIM_FORM"}}); err == nil {
		t.Fatalf("blank DocumentID: want error, got nil")
	}
	bad := facts("doc-a", "CLAIM_FORM")
	bad.Fields["k"] = extract.ExtractedField{Key: "k", Status: extract.FieldStatus("BOGUS")}
	if _, err := assemble.Assemble([]extract.DocumentFacts{bad}); err == nil {
		t.Fatalf("unknown status: want error, got nil")
	}
}
