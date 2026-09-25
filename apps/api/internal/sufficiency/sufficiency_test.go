// RED contract for APA-31 (ADR-007 sufficiency-gate follow-up).
//
// Sufficiency invariant under test: for each document class, the gate
// reports sufficient iff every required key for that class is observed
// PRESENT (exactly-at-threshold is sufficient; one key below is
// insufficient) and, for HOSPITAL_BILL, at least one bill_lines_N line
// item is PRESENT (tables reconstructed where the golden class demands
// them, per ADR-007). Any MISSING / AMBIGUOUS / MULTI_CANDIDATE
// required key, any absent bill line set, or any unknown document class
// fails closed to insufficient, routable through the existing
// exception/HITL/Unresolved vocabulary (invest.MissingField items and a
// verify R8 finding — no new taxonomy, no new topology).
//
// TestCurrentPipelineSlipThrough is the pre-fix gap proof against
// CURRENT code (frozen packages only): wholly empty evidence across all
// three R8-required document types assembles, maps, and verifies with
// Passed=true and zero Unresolved entries — insufficient evidence
// slipping through as verified because no sufficiency rule runs before
// judgment. It passes both before and after the fix and stays as the
// regression record for why the gate must precede judgment.
package sufficiency

import (
	"slices"
	"strings"
	"testing"

	"claimops-api/internal/assemble"
	"claimops-api/internal/extract"
	"claimops-api/internal/extract/xdoc"
	"claimops-api/internal/invest"
	"claimops-api/internal/verify"
	"claimops-api/internal/verifywrap"
)

// pinRequiredKeys pins the invariant's per-class required sets: the frozen
// xdoc observed scalar sets minus the clinical keys (diagnosis,
// procedure) that no verify rule or amount route consumes and that stay
// HITL context only.
var pinRequiredKeys = map[string][]string{
	xdoc.DocClaimForm: {
		xdoc.KeyClaimNumber, xdoc.KeyPolicyNumber, xdoc.KeyPatientName,
		xdoc.KeyHospitalName, xdoc.KeyAdmissionDate, xdoc.KeyDischargeDate,
		xdoc.KeyTotalAmountPaise,
	},
	xdoc.DocDischargeSummary: {
		xdoc.KeyPatientName, xdoc.KeyHospitalName,
		xdoc.KeyAdmissionDate, xdoc.KeyDischargeDate,
	},
	xdoc.DocHospitalBill: {
		xdoc.KeyPatientName, xdoc.KeyHospitalName,
		xdoc.KeyAdmissionDate, xdoc.KeyDischargeDate,
		xdoc.KeyTotalAmountPaise,
	},
	xdoc.DocPolicySchedule: {xdoc.KeyPolicyNumber, xdoc.KeyPatientName},
	xdoc.DocPreauthForm: {
		xdoc.KeyClaimNumber, xdoc.KeyPolicyNumber, xdoc.KeyPatientName,
		xdoc.KeyHospitalName, xdoc.KeyAdmissionDate,
		xdoc.KeyTotalAmountPaise,
	},
	xdoc.DocLabReport: {xdoc.KeyPatientName, xdoc.KeyHospitalName},
}

// presentField builds one PRESENT observation pinned to page 1.
func presentField(key, val, docID string) extract.ExtractedField {
	return extract.ExtractedField{
		Key: key, Value: val, Normalized: val,
		Evidence:         extract.EvidenceRef{DocumentID: docID, Page: 1},
		Status:           extract.StatusPresent,
		Extractor:        "xdoc-test",
		ExtractorVersion: xdoc.Version,
	}
}

// fullFacts builds sufficient evidence for a class: every required key
// PRESENT plus one bill line for HOSPITAL_BILL.
func fullFacts(docType, docID string) extract.DocumentFacts {
	fields := make(map[string]extract.ExtractedField)
	for _, k := range pinRequiredKeys[docType] {
		fields[k] = presentField(k, "v-"+k, docID)
	}
	if docType == xdoc.DocHospitalBill {
		fields[xdoc.KeyBillLines+"_0"] = presentField(xdoc.KeyBillLines+"_0", "Room charges", docID)
	}
	return extract.DocumentFacts{DocumentID: docID, DocType: docType, Fields: fields}
}

// TestSufficientPerClass pins the happy path: complete evidence for
// every document class is sufficient and routes to continue.
func TestSufficientPerClass(t *testing.T) {
	for _, docType := range []string{
		xdoc.DocClaimForm, xdoc.DocDischargeSummary, xdoc.DocHospitalBill,
		xdoc.DocPolicySchedule, xdoc.DocPreauthForm, xdoc.DocLabReport,
	} {
		t.Run(docType, func(t *testing.T) {
			res, err := Evaluate(fullFacts(docType, "doc-1"))
			if err != nil {
				t.Fatalf("Evaluate(full %s) error = %v", docType, err)
			}
			if !res.Sufficient {
				t.Fatalf("Evaluate(full %s) Sufficient = false, Missing=%v Reasons=%v",
					docType, res.Missing, res.ReasonCodes)
			}
			if len(res.Missing) != 0 || len(res.ReasonCodes) != 0 {
				t.Fatalf("sufficient result carries gaps: Missing=%v Reasons=%v",
					res.Missing, res.ReasonCodes)
			}
			if res.Routing != RoutingContinue {
				t.Fatalf("sufficient routing = %q, want %q", res.Routing, RoutingContinue)
			}
			if _, ok := res.SynthesizeFinding(); ok {
				t.Fatal("sufficient result must not synthesize an exception finding")
			}
			if len(res.MissingItems()) != 0 {
				t.Fatal("sufficient result must not yield missing-evidence items")
			}
		})
	}
}

// TestBoundaryMinusOne pins the threshold: exactly-at-threshold is
// sufficient (above) while one required key below is insufficient, for
// every class and every required key.
func TestBoundaryMinusOne(t *testing.T) {
	for docType, keys := range pinRequiredKeys {
		for _, drop := range keys {
			t.Run(docType+"/"+drop, func(t *testing.T) {
				facts := fullFacts(docType, "doc-1")
				f := facts.Fields[drop]
				f.Status = extract.StatusMissing
				f.Value, f.Normalized = "", ""
				f.Evidence = extract.EvidenceRef{}
				facts.Fields[drop] = f
				res, err := Evaluate(facts)
				if err != nil {
					t.Fatalf("Evaluate error = %v", err)
				}
				if res.Sufficient {
					t.Fatalf("one key below threshold (%s) must be insufficient", drop)
				}
				if !slices.Contains(res.Missing, drop) {
					t.Fatalf("Missing = %v, want it to contain %q", res.Missing, drop)
				}
				if !slices.Contains(res.ReasonCodes, ReasonMissingField) {
					t.Fatalf("ReasonCodes = %v, want %q", res.ReasonCodes, ReasonMissingField)
				}
				if res.Routing != RoutingHITL {
					t.Fatalf("insufficient routing = %q, want %q", res.Routing, RoutingHITL)
				}
			})
		}
	}
}

// TestNonPresentStatuses pins that unparseable/conflicting evidence is
// never sufficient, with distinct reason codes per status.
func TestNonPresentStatuses(t *testing.T) {
	cases := []struct {
		status extract.FieldStatus
		reason string
	}{
		{extract.StatusAmbiguous, ReasonAmbiguousField},
		{extract.StatusMultiCandidate, ReasonMultiCandidateField},
	}
	for _, tc := range cases {
		t.Run(string(tc.status), func(t *testing.T) {
			facts := fullFacts(xdoc.DocClaimForm, "doc-1")
			f := facts.Fields[xdoc.KeyPolicyNumber]
			f.Status = tc.status
			if tc.status == extract.StatusMultiCandidate {
				f.Value, f.Normalized = "", ""
				f.Candidates = []extract.Candidate{
					{Value: "A", Evidence: extract.EvidenceRef{DocumentID: "doc-1", Page: 1}},
					{Value: "B", Evidence: extract.EvidenceRef{DocumentID: "doc-1", Page: 1}},
				}
			}
			facts.Fields[xdoc.KeyPolicyNumber] = f
			res, err := Evaluate(facts)
			if err != nil {
				t.Fatalf("Evaluate error = %v", err)
			}
			if res.Sufficient {
				t.Fatalf("status %s must be insufficient", tc.status)
			}
			if !slices.Contains(res.ReasonCodes, tc.reason) {
				t.Fatalf("ReasonCodes = %v, want %q", res.ReasonCodes, tc.reason)
			}
		})
	}
}

// TestHospitalBillLines pins the ADR-007 table demand: a bill total
// without reconstructed line items is insufficient (flattened-to-
// paragraphs is TABLE_MISSED territory, never hidden sufficiency);
// exactly one line is the sufficient boundary.
func TestHospitalBillLines(t *testing.T) {
	t.Run("no lines insufficient", func(t *testing.T) {
		facts := fullFacts(xdoc.DocHospitalBill, "doc-1")
		for k := range facts.Fields {
			if strings.HasPrefix(k, xdoc.KeyBillLines+"_") {
				delete(facts.Fields, k)
			}
		}
		res, err := Evaluate(facts)
		if err != nil {
			t.Fatalf("Evaluate error = %v", err)
		}
		if res.Sufficient {
			t.Fatal("bill without line items must be insufficient")
		}
		if !slices.Contains(res.ReasonCodes, ReasonMissingBillLines) {
			t.Fatalf("ReasonCodes = %v, want %q", res.ReasonCodes, ReasonMissingBillLines)
		}
	})
	t.Run("exactly one line sufficient", func(t *testing.T) {
		res, err := Evaluate(fullFacts(xdoc.DocHospitalBill, "doc-1"))
		if err != nil {
			t.Fatalf("Evaluate error = %v", err)
		}
		if !res.Sufficient {
			t.Fatalf("bill with exactly one line must be sufficient, got %v", res.ReasonCodes)
		}
	})
	t.Run("bare bill_lines MISSING is not a line", func(t *testing.T) {
		facts := fullFacts(xdoc.DocHospitalBill, "doc-1")
		for k := range facts.Fields {
			if strings.HasPrefix(k, xdoc.KeyBillLines+"_") {
				delete(facts.Fields, k)
			}
		}
		facts.Fields[xdoc.KeyBillLines] = extract.ExtractedField{
			Key: xdoc.KeyBillLines, Status: extract.StatusMissing,
			Extractor: "xdoc-test", ExtractorVersion: xdoc.Version,
		}
		res, err := Evaluate(facts)
		if err != nil {
			t.Fatalf("Evaluate error = %v", err)
		}
		if res.Sufficient {
			t.Fatal("bare bill_lines MISSING marker must not count as a reconstructed line")
		}
	})
}

// TestUnknownDocType pins fail-closed routing: a class the gate does
// not know never passes; it routes to HITL without an error.
func TestUnknownDocType(t *testing.T) {
	facts := fullFacts(xdoc.DocClaimForm, "doc-1")
	facts.DocType = "XRAY_REPORT"
	res, err := Evaluate(facts)
	if err != nil {
		t.Fatalf("unknown class must not error (fail closed to HITL), got %v", err)
	}
	if res.Sufficient {
		t.Fatal("unknown document class must be insufficient")
	}
	if !slices.Contains(res.ReasonCodes, ReasonUnknownDocType) {
		t.Fatalf("ReasonCodes = %v, want %q", res.ReasonCodes, ReasonUnknownDocType)
	}
	if res.Routing != RoutingHITL {
		t.Fatalf("routing = %q, want %q", res.Routing, RoutingHITL)
	}
}

// TestInvalidInputs pins terminal validation: evidence that cannot be
// attributed to a document is an error, never a silent verdict.
func TestInvalidInputs(t *testing.T) {
	t.Run("blank document id errors", func(t *testing.T) {
		facts := fullFacts(xdoc.DocClaimForm, "doc-1")
		facts.DocumentID = "  "
		if _, err := Evaluate(facts); err == nil {
			t.Fatal("blank DocumentID must error")
		}
	})
	t.Run("blank doc type fails closed to HITL", func(t *testing.T) {
		facts := fullFacts(xdoc.DocClaimForm, "doc-1")
		facts.DocType = ""
		res, err := Evaluate(facts)
		if err != nil {
			t.Fatalf("blank DocType must fail closed to HITL, not error: %v", err)
		}
		if res.Sufficient || res.Routing != RoutingHITL {
			t.Fatalf("blank DocType must be insufficient/HITL, got %+v", res)
		}
	})
}

// TestExtrasIgnored pins that valid documents are never wrongly
// excepted: optional clinical keys and unknown extra keys PRESENT do
// not disturb sufficiency.
func TestExtrasIgnored(t *testing.T) {
	facts := fullFacts(xdoc.DocClaimForm, "doc-1")
	facts.Fields[xdoc.KeyDiagnosis] = presentField(xdoc.KeyDiagnosis, "Fever", "doc-1")
	facts.Fields[xdoc.KeyProcedure] = presentField(xdoc.KeyProcedure, "Rest", "doc-1")
	facts.Fields["future_key"] = presentField("future_key", "x", "doc-1")
	res, err := Evaluate(facts)
	if err != nil {
		t.Fatalf("Evaluate error = %v", err)
	}
	if !res.Sufficient {
		t.Fatalf("optional/unknown extras must not except a valid document: %v", res.ReasonCodes)
	}
}

// TestReasonCodesDeterministic pins sorted, stable reason output under
// Go map iteration randomization.
func TestReasonCodesDeterministic(t *testing.T) {
	facts := extract.DocumentFacts{
		DocumentID: "doc-1", DocType: xdoc.DocClaimForm, Fields: map[string]extract.ExtractedField{},
	}
	first, err := Evaluate(facts)
	if err != nil {
		t.Fatalf("Evaluate error = %v", err)
	}
	if first.Sufficient {
		t.Fatal("empty facts must be insufficient")
	}
	if !slices.IsSorted(first.ReasonCodes) || !slices.IsSorted(first.Missing) {
		t.Fatalf("outputs must be sorted: Reasons=%v Missing=%v", first.ReasonCodes, first.Missing)
	}
	for range 25 {
		res, err := Evaluate(facts)
		if err != nil {
			t.Fatalf("Evaluate error = %v", err)
		}
		if !slices.Equal(res.ReasonCodes, first.ReasonCodes) || !slices.Equal(res.Missing, first.Missing) {
			t.Fatalf("unstable output:\nfirst Reasons=%v Missing=%v\ngot   Reasons=%v Missing=%v",
				first.ReasonCodes, first.Missing, res.ReasonCodes, res.Missing)
		}
	}
}

// TestExceptionBridges pins routing through the EXISTING vocabulary:
// missing items use the closed invest MissingField kind with display
// detail, and the synthesized finding reuses the closed R1-R10 taxonomy
// (R8, the evidence-sufficiency signal) so invest.Build accepts it.
func TestExceptionBridges(t *testing.T) {
	facts := fullFacts(xdoc.DocHospitalBill, "doc-1")
	delete(facts.Fields, xdoc.KeyTotalAmountPaise)
	res, err := Evaluate(facts)
	if err != nil {
		t.Fatalf("Evaluate error = %v", err)
	}
	if res.Sufficient {
		t.Fatal("want insufficient fixture")
	}
	for _, m := range res.MissingItems() {
		if m.Kind != invest.MissingField {
			t.Fatalf("missing item kind = %q, want %q", m.Kind, invest.MissingField)
		}
		if strings.TrimSpace(m.Key) == "" || strings.TrimSpace(m.Detail) == "" {
			t.Fatalf("missing items need key + display detail: %+v", m)
		}
	}
	finding, ok := res.SynthesizeFinding()
	if !ok {
		t.Fatal("insufficient result must synthesize a finding")
	}
	if finding.Code != verify.CodeMissingRequiredDocument {
		t.Fatalf("finding code = %q, want R8 %q", finding.Code, verify.CodeMissingRequiredDocument)
	}
	if !invest.IsKnownRuleCode(finding.Code) {
		t.Fatalf("finding code %q must stay inside the closed rule taxonomy", finding.Code)
	}
}

// TestCurrentPipelineSlipThrough proves the pre-fix gap with CURRENT
// code (frozen packages only, no gate): wholly empty evidence — every
// field MISSING across all three R8-required document types —
// assembles, maps with zero Unresolved entries, and verifies
// Passed=true. Empty/insufficient evidence slips through as verified;
// the insufficient-parse path never fires because no such rule runs
// before judgment. This test passes before and after the fix and stays
// as the regression record for why sufficiency must precede judgment.
func TestCurrentPipelineSlipThrough(t *testing.T) {
	emptyDoc := func(docType, docID string) extract.DocumentFacts {
		return extract.DocumentFacts{DocumentID: docID, DocType: docType, Fields: map[string]extract.ExtractedField{}}
	}
	claim, err := assemble.Assemble([]extract.DocumentFacts{
		emptyDoc(verify.DocClaimForm, "d1"),
		emptyDoc(verify.DocDischargeSummary, "d2"),
		emptyDoc(verify.DocHospitalBill, "d3"),
	})
	if err != nil {
		t.Fatalf("Assemble error = %v", err)
	}
	in, unresolved, err := verifywrap.Map(claim, verifywrap.Externals{
		PolicyActive: true, ExternalPolicyOK: true,
	})
	if err != nil {
		t.Fatalf("Map error = %v", err)
	}
	if len(unresolved) != 0 {
		t.Fatalf("MISSING-only evidence yields zero Unresolved entries, got %v", unresolved)
	}
	res := verify.Verify(in)
	if !res.Passed || len(res.Exceptions) != 0 {
		t.Fatalf("gap proof requires a clean verify pass on empty evidence, got %+v", res)
	}
	_ = assemble.StatusAgreed
}
