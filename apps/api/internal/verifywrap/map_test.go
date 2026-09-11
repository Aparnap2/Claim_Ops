// Deterministic verifywrap tests: hand-built assemble.CanonicalClaim
// values only (no fixtures, no network, no I/O, no clock reads), plus two
// assemble-driven end-to-end tests (assemble -> Map -> verify.Verify).
// Synthetic identifiers throughout (no PII/PHI).
//
// Coverage: all 15 binding mapping cases (AGREED single + multiple,
// MISSING stays missing, CONFLICT never scalar, NEEDS_REVIEW never scalar
// incl. shadowed agreement, evidence grouping by DocType, multi-source
// agreement, case-variant agreement, deterministic ordering,
// reversal-identical Input, unsupported DocType passthrough,
// descriptions-only BillLinePaise nil + R5 absent, empty claim, duplicate
// provenance, invalid assembly rejected) plus F11a amount routing, externals
// verbatim, and the R1-R10 regression pair (clean PASS + known-exception
// end-to-end). The existing verify suite is untouched.
package verifywrap_test

import (
	"reflect"
	"testing"
	"time"

	"claimops-api/internal/assemble"
	"claimops-api/internal/extract"
	"claimops-api/internal/verify"
	"claimops-api/internal/verifywrap"
)

// ---------------------------------------------------------------------------
// Builders
// ---------------------------------------------------------------------------

func ev(docID, block string, page int) extract.EvidenceRef {
	return extract.EvidenceRef{DocumentID: docID, Page: page, BlockID: block}
}

// fsrc builds one voting source for a hand-built AssembledField.
func fsrc(docID, docType, value, normalized string) assemble.FieldSource {
	return assemble.FieldSource{
		Value: value, Normalized: normalized,
		Evidence:         ev(docID, "b1", 1),
		Extractor:        "stub",
		ExtractorVersion: "1.0.0",
		DocType:          docType,
	}
}

func agreed(key, agreedVal, rawVal string, sources ...assemble.FieldSource) assemble.AssembledField {
	return assemble.AssembledField{
		Key: key, Status: assemble.StatusAgreed,
		Agreed: agreedVal, AgreedRaw: rawVal, Sources: sources,
	}
}

func missing(key string) assemble.AssembledField {
	return assemble.AssembledField{Key: key, Status: assemble.StatusMissing}
}

// conflictField builds a CONFLICT field with its matching ConflictEntry.
func conflictField(key string, distinct []string, sources ...assemble.FieldSource) (assemble.AssembledField, assemble.ConflictEntry) {
	f := assemble.AssembledField{Key: key, Status: assemble.StatusConflict, Sources: sources}
	c := assemble.ConflictEntry{Key: key, Distinct: distinct, Sources: sources}
	return f, c
}

func reviewItem(docID, docType string, status extract.FieldStatus, value string) assemble.ReviewItem {
	return assemble.ReviewItem{
		DocumentID: docID, DocType: docType, Status: status,
		Value: value, Evidence: ev(docID, "b9", 1),
	}
}

func needsReview(key, agreedVal string, sources []assemble.FieldSource, items ...assemble.ReviewItem) assemble.AssembledField {
	return assemble.AssembledField{
		Key: key, Status: assemble.StatusNeedsReview,
		Agreed: agreedVal, Sources: sources, NeedsReview: items,
	}
}

// claimWith builds a CanonicalClaim from fields, conflicts, and present
// doctypes. Pass no docsPresent for an empty DocsPresent map.
func claimWith(fields []assemble.AssembledField, conflicts []assemble.ConflictEntry, docsPresent ...string) assemble.CanonicalClaim {
	c := assemble.CanonicalClaim{
		Fields:      make(map[string]assemble.AssembledField, len(fields)),
		DocsPresent: make(map[string]bool),
	}
	for _, f := range fields {
		c.Fields[f.Key] = f
	}
	c.Conflicts = conflicts
	for _, d := range docsPresent {
		c.DocsPresent[d] = true
	}
	return c
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse("2006-01-02", s)
	if err != nil {
		t.Fatalf("parse date %q: %v", s, err)
	}
	return tm
}

func mustMap(t *testing.T, c assemble.CanonicalClaim, ext verifywrap.Externals) (verify.Input, []verifywrap.Unresolved) {
	t.Helper()
	in, un, err := verifywrap.Map(c, ext)
	if err != nil {
		t.Fatalf("Map = error %v", err)
	}
	return in, un
}

func codes(ex []verify.Exception) []string {
	got := make([]string, 0, len(ex))
	for _, e := range ex {
		got = append(got, e.Code)
	}
	return got
}

func hasCode(ex []verify.Exception, code string) bool {
	for _, e := range ex {
		if e.Code == code {
			return true
		}
	}
	return false
}

// extActive returns fully-supplied externals that fire neither R4 nor R9.
func extActive() verifywrap.Externals {
	return verifywrap.Externals{
		PolicyNumber: "POL-1001", PolicyPatient: "Aarav Sharma",
		PolicyActive: true, ExternalPolicyOK: true,
	}
}

// epresent builds a PRESENT extract observation for assemble-driven tests.
func epresent(key, value, normalized, docID string) extract.ExtractedField {
	return extract.ExtractedField{
		Key: key, Value: value, Normalized: normalized,
		Evidence: ev(docID, "b1", 1), Status: extract.StatusPresent,
		Extractor: "stub", ExtractorVersion: "1.0.0",
	}
}

func efacts(docID, docType string, fields ...extract.ExtractedField) extract.DocumentFacts {
	m := make(map[string]extract.ExtractedField, len(fields))
	for _, f := range fields {
		m[f.Key] = f
	}
	return extract.DocumentFacts{DocumentID: docID, DocType: docType, Fields: m}
}

// ---------------------------------------------------------------------------
// 1-2. AGREED single + multiple sources
// ---------------------------------------------------------------------------

// 1: AGREED single source populates every typed slot it owns.
func TestMapAgreedSingle(t *testing.T) {
	c := claimWith([]assemble.AssembledField{
		agreed("policy_number", "POL-1001", "POL-1001", fsrc("doc-a", "CLAIM_FORM", "POL-1001", "POL-1001")),
		agreed("patient_name", "aarav sharma", "Aarav Sharma", fsrc("doc-a", "CLAIM_FORM", "Aarav Sharma", "aarav sharma")),
		agreed("hospital_name", "fortis hospital", "Fortis Hospital", fsrc("doc-a", "CLAIM_FORM", "Fortis Hospital", "fortis hospital")),
		agreed("admission_date", "2026-08-10", "2026-08-10", fsrc("doc-a", "CLAIM_FORM", "2026-08-10", "2026-08-10")),
		agreed("discharge_date", "2026-08-14", "2026-08-14", fsrc("doc-a", "CLAIM_FORM", "2026-08-14", "2026-08-14")),
		// CLAIM_FORM-sourced amount routes to ClaimedPaise only (F11a).
		agreed("total_amount_paise", "7950000", "79500.00", fsrc("doc-a", "CLAIM_FORM", "79500.00", "7950000")),
	}, nil, "CLAIM_FORM")

	in, un := mustMap(t, c, verifywrap.Externals{})
	if len(un) != 0 {
		t.Fatalf("unresolved = %+v, want none", un)
	}
	if in.ClaimPolicyNumber != "POL-1001" {
		t.Fatalf("ClaimPolicyNumber = %q, want POL-1001", in.ClaimPolicyNumber)
	}
	if in.ClaimPatientName != "aarav sharma" {
		t.Fatalf("ClaimPatientName = %q, want agreed verbatim", in.ClaimPatientName)
	}
	if in.ClaimHospitalName != "fortis hospital" {
		t.Fatalf("ClaimHospitalName = %q, want agreed verbatim", in.ClaimHospitalName)
	}
	if !in.HasAdmission || !in.Admission.Equal(mustTime(t, "2026-08-10")) {
		t.Fatalf("Admission = %v has=%v, want 2026-08-10 true", in.Admission, in.HasAdmission)
	}
	if !in.HasDischarge || !in.Discharge.Equal(mustTime(t, "2026-08-14")) {
		t.Fatalf("Discharge = %v has=%v, want 2026-08-14 true", in.Discharge, in.HasDischarge)
	}
	if in.ClaimedPaise != 7950000 {
		t.Fatalf("ClaimedPaise = %d, want 7950000", in.ClaimedPaise)
	}
	if in.HasBillTotal || in.BillTotalPaise != 0 {
		t.Fatalf("BillTotal = %d has=%v, want 0 false (no HOSPITAL_BILL source)", in.BillTotalPaise, in.HasBillTotal)
	}
	if !reflect.DeepEqual(in.DocsPresent, map[string]bool{"CLAIM_FORM": true}) {
		t.Fatalf("DocsPresent = %+v, want verbatim copy", in.DocsPresent)
	}
	wantEv := map[string]map[string]string{
		"CLAIM_FORM": {"patient_name": "aarav sharma", "admission_date": "2026-08-10"},
	}
	if !reflect.DeepEqual(in.DocEvidence, wantEv) {
		t.Fatalf("DocEvidence = %+v, want %+v", in.DocEvidence, wantEv)
	}
	if in.BillLinePaise != nil {
		t.Fatalf("BillLinePaise = %+v, want nil (F11b)", in.BillLinePaise)
	}
}

// 2: AGREED across multiple sources routes both amount slots when both
// DocTypes agree on one value.
func TestMapAgreedMultipleSources(t *testing.T) {
	c := claimWith([]assemble.AssembledField{
		agreed("policy_number", "POL-1001", "POL-1001",
			fsrc("doc-a", "CLAIM_FORM", "POL-1001", "POL-1001"),
			fsrc("doc-b", "DISCHARGE_SUMMARY", "POL-1001", "POL-1001")),
		agreed("patient_name", "aarav sharma", "Aarav Sharma",
			fsrc("doc-a", "CLAIM_FORM", "Aarav Sharma", "aarav sharma"),
			fsrc("doc-b", "DISCHARGE_SUMMARY", "Aarav Sharma", "aarav sharma")),
		agreed("admission_date", "2026-08-10", "2026-08-10",
			fsrc("doc-a", "CLAIM_FORM", "2026-08-10", "2026-08-10"),
			fsrc("doc-b", "DISCHARGE_SUMMARY", "2026-08-10", "2026-08-10")),
		// One agreed value with HOSPITAL_BILL + CLAIM_FORM sources:
		// both F11a routes fire on the same paise value.
		agreed("total_amount_paise", "7950000", "79500.00",
			fsrc("doc-a", "CLAIM_FORM", "79500.00", "7950000"),
			fsrc("doc-c", "HOSPITAL_BILL", "79500.00", "7950000")),
	}, nil, "CLAIM_FORM", "DISCHARGE_SUMMARY", "HOSPITAL_BILL")

	in, un := mustMap(t, c, verifywrap.Externals{})
	if len(un) != 0 {
		t.Fatalf("unresolved = %+v, want none", un)
	}
	if in.BillTotalPaise != 7950000 || !in.HasBillTotal {
		t.Fatalf("BillTotal = %d has=%v, want 7950000 true", in.BillTotalPaise, in.HasBillTotal)
	}
	if in.ClaimedPaise != 7950000 {
		t.Fatalf("ClaimedPaise = %d, want 7950000", in.ClaimedPaise)
	}
	for _, dt := range []string{"CLAIM_FORM", "DISCHARGE_SUMMARY"} {
		m, ok := in.DocEvidence[dt]
		if !ok {
			t.Fatalf("DocEvidence missing DocType %q: %+v", dt, in.DocEvidence)
		}
		if m["patient_name"] != "aarav sharma" || m["admission_date"] != "2026-08-10" {
			t.Fatalf("DocEvidence[%q] = %+v, want agreed values", dt, m)
		}
	}
}

// ---------------------------------------------------------------------------
// 3. MISSING stays missing
// ---------------------------------------------------------------------------

func TestMapMissingStaysMissing(t *testing.T) {
	c := claimWith([]assemble.AssembledField{
		missing("policy_number"),
		missing("patient_name"),
		missing("admission_date"),
		missing("discharge_date"),
		missing("total_amount_paise"),
	}, nil)

	in, un := mustMap(t, c, verifywrap.Externals{})
	if len(un) != 0 {
		t.Fatalf("unresolved = %+v, want none for MISSING", un)
	}
	if in.ClaimPolicyNumber != "" || in.ClaimPatientName != "" || in.ClaimHospitalName != "" {
		t.Fatalf("identity = %q/%q/%q, want zeros", in.ClaimPolicyNumber, in.ClaimPatientName, in.ClaimHospitalName)
	}
	if in.HasAdmission || in.HasDischarge {
		t.Fatalf("Has flags = %v/%v, want false/false", in.HasAdmission, in.HasDischarge)
	}
	if in.ClaimedPaise != 0 || in.HasBillTotal || in.BillTotalPaise != 0 {
		t.Fatalf("amounts = %d/%d/%v, want zeros", in.ClaimedPaise, in.BillTotalPaise, in.HasBillTotal)
	}
	if in.DocEvidence != nil {
		t.Fatalf("DocEvidence = %+v, want nil", in.DocEvidence)
	}
}

// ---------------------------------------------------------------------------
// 4-6. CONFLICT / NEEDS_REVIEW never scalar
// ---------------------------------------------------------------------------

// 4: CONFLICT leaves the typed field zero and carries the ConflictEntry.
func TestMapConflictNeverScalar(t *testing.T) {
	f, ce := conflictField("policy_number", []string{"POL-1001", "POL-1002"},
		fsrc("doc-a", "CLAIM_FORM", "POL-1001", "POL-1001"),
		fsrc("doc-b", "DISCHARGE_SUMMARY", "POL-1002", "POL-1002"))
	c := claimWith([]assemble.AssembledField{f}, []assemble.ConflictEntry{ce}, "CLAIM_FORM", "DISCHARGE_SUMMARY")

	in, un := mustMap(t, c, verifywrap.Externals{})
	if in.ClaimPolicyNumber != "" {
		t.Fatalf("ClaimPolicyNumber = %q, want zero under CONFLICT", in.ClaimPolicyNumber)
	}
	if len(un) != 1 {
		t.Fatalf("unresolved = %+v, want exactly one entry", un)
	}
	u := un[0]
	if u.Key != "policy_number" || u.Status != assemble.StatusConflict {
		t.Fatalf("unresolved = %+v, want policy_number/CONFLICT", u)
	}
	if u.Conflict == nil {
		t.Fatal("Conflict pointer is nil, want the ConflictEntry")
	}
	if !reflect.DeepEqual(u.Conflict.Distinct, []string{"POL-1001", "POL-1002"}) {
		t.Fatalf("Distinct = %q, want sorted distinct values", u.Conflict.Distinct)
	}
	if len(u.Conflict.Sources) != 2 {
		t.Fatalf("Conflict sources = %d, want 2", len(u.Conflict.Sources))
	}
	if len(u.Review) != 0 {
		t.Fatalf("Review = %+v, want empty for CONFLICT", u.Review)
	}
}

// 5: NEEDS_REVIEW with no votes leaves the typed field zero and carries
// the review items verbatim.
func TestMapNeedsReviewNeverScalar(t *testing.T) {
	c := claimWith([]assemble.AssembledField{
		needsReview("admission_date", "", nil,
			reviewItem("doc-a", "CLAIM_FORM", extract.StatusAmbiguous, "10th Aug 2026")),
	}, nil, "CLAIM_FORM")

	in, un := mustMap(t, c, verifywrap.Externals{})
	if in.HasAdmission || !in.Admission.IsZero() {
		t.Fatalf("Admission = %v has=%v, want zero/false", in.Admission, in.HasAdmission)
	}
	if len(un) != 1 {
		t.Fatalf("unresolved = %+v, want exactly one entry", un)
	}
	u := un[0]
	if u.Key != "admission_date" || u.Status != assemble.StatusNeedsReview {
		t.Fatalf("unresolved = %+v, want admission_date/NEEDS_REVIEW", u)
	}
	if u.Conflict != nil {
		t.Fatalf("Conflict = %+v, want nil for NEEDS_REVIEW", u.Conflict)
	}
	if len(u.Review) != 1 || u.Review[0].Value != "10th Aug 2026" {
		t.Fatalf("Review = %+v, want the verbatim ambiguous raw", u.Review)
	}
	if _, ok := in.DocEvidence["CLAIM_FORM"]; ok {
		t.Fatalf("DocEvidence carries review-only key: %+v", in.DocEvidence)
	}
}

// 6: NEEDS_REVIEW with shadowed agreement (Agreed set, votes + review)
// still never feeds typed input or DocEvidence.
func TestMapShadowedAgreementNeverScalar(t *testing.T) {
	c := claimWith([]assemble.AssembledField{
		{
			Key: "patient_name", Status: assemble.StatusNeedsReview,
			Agreed: "aarav sharma", AgreedRaw: "Aarav Sharma",
			Sources:     []assemble.FieldSource{fsrc("doc-a", "CLAIM_FORM", "Aarav Sharma", "aarav sharma")},
			NeedsReview: []assemble.ReviewItem{reviewItem("doc-b", "DISCHARGE_SUMMARY", extract.StatusAmbiguous, "A. Sharma?")},
		},
	}, nil, "CLAIM_FORM", "DISCHARGE_SUMMARY")

	in, un := mustMap(t, c, verifywrap.Externals{})
	if in.ClaimPatientName != "" {
		t.Fatalf("ClaimPatientName = %q, want zero under shadowed agreement", in.ClaimPatientName)
	}
	if len(un) != 1 || un[0].Status != assemble.StatusNeedsReview {
		t.Fatalf("unresolved = %+v, want one NEEDS_REVIEW entry", un)
	}
	if len(un[0].Review) != 1 {
		t.Fatalf("Review = %+v, want the shadowing item", un[0].Review)
	}
	if len(in.DocEvidence) != 0 {
		t.Fatalf("DocEvidence = %+v, want empty for shadowed agreement", in.DocEvidence)
	}
}

// ---------------------------------------------------------------------------
// 7-9. Evidence grouping, case variants, deterministic ordering
// ---------------------------------------------------------------------------

// 7: DocEvidence groups agreed patient_name + admission_date by DocType
// and carries ONLY the R2/R10 keys.
func TestMapEvidenceGroupingByDocType(t *testing.T) {
	c := claimWith([]assemble.AssembledField{
		agreed("patient_name", "aarav sharma", "Aarav Sharma",
			fsrc("doc-a", "CLAIM_FORM", "Aarav Sharma", "aarav sharma"),
			fsrc("doc-b", "DISCHARGE_SUMMARY", "Aarav Sharma", "aarav sharma"),
			fsrc("doc-c", "HOSPITAL_BILL", "Aarav Sharma", "aarav sharma")),
		agreed("admission_date", "2026-08-10", "2026-08-10",
			fsrc("doc-a", "CLAIM_FORM", "2026-08-10", "2026-08-10"),
			fsrc("doc-c", "HOSPITAL_BILL", "2026-08-10", "2026-08-10")),
		// Typed-only: hospital_name must NOT enter DocEvidence.
		agreed("hospital_name", "fortis hospital", "Fortis Hospital",
			fsrc("doc-a", "CLAIM_FORM", "Fortis Hospital", "fortis hospital")),
	}, nil, "CLAIM_FORM", "DISCHARGE_SUMMARY", "HOSPITAL_BILL")

	in, _ := mustMap(t, c, verifywrap.Externals{})
	want := map[string]map[string]string{
		"CLAIM_FORM":        {"patient_name": "aarav sharma", "admission_date": "2026-08-10"},
		"DISCHARGE_SUMMARY": {"patient_name": "aarav sharma"},
		"HOSPITAL_BILL":     {"patient_name": "aarav sharma", "admission_date": "2026-08-10"},
	}
	if !reflect.DeepEqual(in.DocEvidence, want) {
		t.Fatalf("DocEvidence = %+v, want %+v", in.DocEvidence, want)
	}
}

// 8: case-variant ID agreement maps the first-source Agreed verbatim.
func TestMapCaseVariantAgreementVerbatim(t *testing.T) {
	c := claimWith([]assemble.AssembledField{
		agreed("policy_number", "POL-1001", "POL-1001",
			fsrc("doc-a", "CLAIM_FORM", "POL-1001", "POL-1001"),
			fsrc("doc-b", "DISCHARGE_SUMMARY", "pol-1001", "pol-1001"),
			fsrc("doc-c", "HOSPITAL_BILL", "  POL-1001 ", "POL-1001")),
	}, nil, "CLAIM_FORM", "DISCHARGE_SUMMARY", "HOSPITAL_BILL")

	in, _ := mustMap(t, c, verifywrap.Externals{})
	if in.ClaimPolicyNumber != "POL-1001" {
		t.Fatalf("ClaimPolicyNumber = %q, want first-source Agreed verbatim", in.ClaimPolicyNumber)
	}
}

// 9: Unresolved entries iterate in sorted key order (maps randomize
// iteration; the sort closes that). Repeated to defeat luck.
func TestMapDeterministicOrdering(t *testing.T) {
	build := func() assemble.CanonicalClaim {
		f1, ce1 := conflictField("policy_number", []string{"POL-1001", "POL-1002"},
			fsrc("doc-a", "CLAIM_FORM", "POL-1001", "POL-1001"),
			fsrc("doc-b", "DISCHARGE_SUMMARY", "POL-1002", "POL-1002"))
		f2, ce2 := conflictField("patient_name", []string{"aarav sharma", "vivaan rao"},
			fsrc("doc-a", "CLAIM_FORM", "Aarav Sharma", "aarav sharma"),
			fsrc("doc-b", "DISCHARGE_SUMMARY", "Vivaan Rao", "vivaan rao"))
		f3, ce3 := conflictField("admission_date", []string{"2026-08-10", "2026-08-11"},
			fsrc("doc-a", "CLAIM_FORM", "2026-08-10", "2026-08-10"),
			fsrc("doc-b", "DISCHARGE_SUMMARY", "2026-08-11", "2026-08-11"))
		return claimWith(
			[]assemble.AssembledField{f1, f2, f3,
				needsReview("diagnosis", "", nil, reviewItem("doc-a", "CLAIM_FORM", extract.StatusAmbiguous, " Sjögren? "))},
			[]assemble.ConflictEntry{ce1, ce2, ce3},
			"CLAIM_FORM", "DISCHARGE_SUMMARY")
	}
	want := []string{"admission_date", "diagnosis", "patient_name", "policy_number"}
	for i := 0; i < 20; i++ {
		_, un := mustMap(t, build(), verifywrap.Externals{})
		if len(un) != len(want) {
			t.Fatalf("iter %d: unresolved = %d entries, want %d", i, len(un), len(want))
		}
		for j, k := range want {
			if un[j].Key != k {
				t.Fatalf("iter %d: unresolved order = %q, want %q", i, keys(un), want)
			}
		}
	}
}

func keys(un []verifywrap.Unresolved) []string {
	got := make([]string, 0, len(un))
	for _, u := range un {
		got = append(got, u.Key)
	}
	return got
}

// ---------------------------------------------------------------------------
// 10. Reversal-identical Input (assemble-driven)
// ---------------------------------------------------------------------------

// 10: assembling the same documents in reversed input order yields an
// identical Input (byte-identical agreed values everywhere).
func TestMapReversalIdentical(t *testing.T) {
	docs := []extract.DocumentFacts{
		efacts("doc-a", "CLAIM_FORM",
			epresent("policy_number", "POL-1001", "POL-1001", "doc-a"),
			epresent("patient_name", "Aarav Sharma", "aarav sharma", "doc-a"),
			epresent("admission_date", "2026-08-10", "2026-08-10", "doc-a"),
			epresent("total_amount_paise", "79500.00", "7950000", "doc-a")),
		efacts("doc-b", "HOSPITAL_BILL",
			epresent("policy_number", "POL-1001", "POL-1001", "doc-b"),
			epresent("patient_name", "Aarav Sharma", "aarav sharma", "doc-b"),
			epresent("admission_date", "2026-08-10", "2026-08-10", "doc-b"),
			epresent("total_amount_paise", "79500.00", "7950000", "doc-b")),
	}
	rev := []extract.DocumentFacts{docs[1], docs[0]}
	fwd, err := assemble.Assemble(docs)
	if err != nil {
		t.Fatalf("Assemble fwd = error %v", err)
	}
	bwd, err := assemble.Assemble(rev)
	if err != nil {
		t.Fatalf("Assemble rev = error %v", err)
	}
	ext := extActive()
	inFwd, unFwd := mustMap(t, fwd, ext)
	inBwd, unBwd := mustMap(t, bwd, ext)
	if !reflect.DeepEqual(inFwd, inBwd) {
		t.Fatalf("reversed Input differs:\n fwd=%+v\n bwd=%+v", inFwd, inBwd)
	}
	if !reflect.DeepEqual(unFwd, unBwd) {
		t.Fatalf("reversed Unresolved differs:\n fwd=%+v\n bwd=%+v", unFwd, unBwd)
	}
}

// ---------------------------------------------------------------------------
// 11. Unsupported DocType passthrough
// ---------------------------------------------------------------------------

// 11: a POLICY_SCHEDULE-only agreement contributes identity but no
// amount and passes DocsPresent through verbatim.
func TestMapUnsupportedDocTypePassthrough(t *testing.T) {
	c := claimWith([]assemble.AssembledField{
		agreed("policy_number", "POL-1001", "POL-1001", fsrc("doc-p", "POLICY_SCHEDULE", "POL-1001", "POL-1001")),
		agreed("total_amount_paise", "5000000", "50000.00", fsrc("doc-p", "POLICY_SCHEDULE", "50000.00", "5000000")),
	}, nil, "POLICY_SCHEDULE")

	in, un := mustMap(t, c, verifywrap.Externals{})
	if len(un) != 0 {
		t.Fatalf("unresolved = %+v, want none", un)
	}
	if in.ClaimPolicyNumber != "POL-1001" {
		t.Fatalf("ClaimPolicyNumber = %q, want POL-1001 (identity passes through)", in.ClaimPolicyNumber)
	}
	if in.HasBillTotal || in.BillTotalPaise != 0 || in.ClaimedPaise != 0 {
		t.Fatalf("amounts = %d/%d/%v, want zeros (POLICY_SCHEDULE routes nowhere)",
			in.ClaimedPaise, in.BillTotalPaise, in.HasBillTotal)
	}
	if !reflect.DeepEqual(in.DocsPresent, map[string]bool{"POLICY_SCHEDULE": true}) {
		t.Fatalf("DocsPresent = %+v, want verbatim passthrough", in.DocsPresent)
	}
}

// ---------------------------------------------------------------------------
// 12. Descriptions-only: BillLinePaise nil + R5 absent
// ---------------------------------------------------------------------------

// 12: bill_lines_N description keys never produce line amounts; R5 stays
// silent on mapped input even for a bill with lines.
func TestMapDescriptionsOnlyBillLinesNil(t *testing.T) {
	c := claimWith([]assemble.AssembledField{
		agreed("policy_number", "POL-1001", "POL-1001", fsrc("doc-a", "CLAIM_FORM", "POL-1001", "POL-1001")),
		agreed("patient_name", "aarav sharma", "Aarav Sharma", fsrc("doc-a", "CLAIM_FORM", "Aarav Sharma", "aarav sharma")),
		agreed("admission_date", "2026-08-10", "2026-08-10", fsrc("doc-a", "CLAIM_FORM", "2026-08-10", "2026-08-10")),
		agreed("total_amount_paise", "7950000", "79500.00", fsrc("doc-b", "HOSPITAL_BILL", "79500.00", "7950000")),
		agreed("bill_lines_0", "Room rent", "Room rent", fsrc("doc-b", "HOSPITAL_BILL", "Room rent", "Room rent")),
		agreed("bill_lines_1", "Pharmacy", "Pharmacy", fsrc("doc-b", "HOSPITAL_BILL", "Pharmacy", "Pharmacy")),
	}, nil, "CLAIM_FORM", "DISCHARGE_SUMMARY", "HOSPITAL_BILL")

	in, _ := mustMap(t, c, extActive())
	if in.BillLinePaise != nil {
		t.Fatalf("BillLinePaise = %+v, want nil (F11b: descriptions only)", in.BillLinePaise)
	}
	got := verify.Verify(in)
	if hasCode(got.Exceptions, verify.CodeAmountReconciliationFailure) {
		t.Fatalf("R5 fired on descriptions-only input: %+v", got.Exceptions)
	}
}

// ---------------------------------------------------------------------------
// 13-14. Empty claim, duplicate provenance
// ---------------------------------------------------------------------------

// 13: an empty claim maps to the zero Input with no error.
func TestMapEmptyClaim(t *testing.T) {
	in, un, err := verifywrap.Map(assemble.CanonicalClaim{}, verifywrap.Externals{})
	if err != nil {
		t.Fatalf("Map empty = error %v, want nil", err)
	}
	if len(un) != 0 {
		t.Fatalf("unresolved = %+v, want none", un)
	}
	if !reflect.DeepEqual(in, verify.Input{}) {
		t.Fatalf("Input = %+v, want zero Input", in)
	}
}

// 14: duplicate identical provenance votes twice but maps once: the Input
// matches the single-document mapping with no duplicated evidence.
func TestMapDuplicateProvenance(t *testing.T) {
	doc := efacts("doc-a", "CLAIM_FORM",
		epresent("policy_number", "POL-1001", "POL-1001", "doc-a"),
		epresent("patient_name", "Aarav Sharma", "aarav sharma", "doc-a"),
		epresent("admission_date", "2026-08-10", "2026-08-10", "doc-a"),
		epresent("total_amount_paise", "79500.00", "7950000", "doc-a"))
	single, err := assemble.Assemble([]extract.DocumentFacts{doc})
	if err != nil {
		t.Fatalf("Assemble single = error %v", err)
	}
	dup, err := assemble.Assemble([]extract.DocumentFacts{doc, doc})
	if err != nil {
		t.Fatalf("Assemble dup = error %v", err)
	}
	ext := extActive()
	inSingle, _ := mustMap(t, single, ext)
	inDup, _ := mustMap(t, dup, ext)
	if !reflect.DeepEqual(inSingle, inDup) {
		t.Fatalf("duplicate Input differs:\n single=%+v\n dup=%+v", inSingle, inDup)
	}
	if len(inDup.DocEvidence) != 1 || inDup.DocEvidence["CLAIM_FORM"]["patient_name"] != "aarav sharma" {
		t.Fatalf("DocEvidence = %+v, want one CLAIM_FORM entry", inDup.DocEvidence)
	}
}

// ---------------------------------------------------------------------------
// 15. Invalid assembly rejected (+ F11a routing table + externals verbatim)
// ---------------------------------------------------------------------------

// 15: terminal errors fire only on internal inconsistency.
func TestMapInvalidAssemblyRejected(t *testing.T) {
	badDate := agreed("admission_date", "10/08/2026", "10/08/2026", fsrc("doc-a", "CLAIM_FORM", "10/08/2026", "10/08/2026"))
	badAmount := agreed("total_amount_paise", "12.345", "12.345", fsrc("doc-b", "HOSPITAL_BILL", "12.345", "12.345"))
	confNoEntry := assemble.AssembledField{Key: "policy_number", Status: assemble.StatusConflict,
		Sources: []assemble.FieldSource{fsrc("doc-a", "CLAIM_FORM", "POL-1001", "POL-1001")}}

	tests := []struct {
		name      string
		claim     assemble.CanonicalClaim
		conflicts []assemble.ConflictEntry
	}{
		{"blank map key", assemble.CanonicalClaim{Fields: map[string]assemble.AssembledField{"": missing("x")}}, nil},
		{"blank field key", assemble.CanonicalClaim{Fields: map[string]assemble.AssembledField{"policy_number": {Status: assemble.StatusMissing}}}, nil},
		{"key mismatch", assemble.CanonicalClaim{Fields: map[string]assemble.AssembledField{"policy_number": missing("patient_name")}}, nil},
		{"unknown status", assemble.CanonicalClaim{Fields: map[string]assemble.AssembledField{"policy_number": {Key: "policy_number", Status: "BOGUS"}}}, nil},
		{"AGREED empty Agreed", claimWith([]assemble.AssembledField{{Key: "policy_number", Status: assemble.StatusAgreed}}, nil), nil},
		{"AGREED with review", claimWith([]assemble.AssembledField{{
			Key: "policy_number", Status: assemble.StatusAgreed, Agreed: "POL-1001",
			NeedsReview: []assemble.ReviewItem{reviewItem("doc-a", "CLAIM_FORM", extract.StatusAmbiguous, "POL-1001?")},
		}}, nil), nil},
		{"CONFLICT with Agreed", claimWith([]assemble.AssembledField{{
			Key: "policy_number", Status: assemble.StatusConflict, Agreed: "POL-1001",
		}}, []assemble.ConflictEntry{{Key: "policy_number"}}), nil},
		{"CONFLICT without ConflictEntry", claimWith([]assemble.AssembledField{{
			Key: "policy_number", Status: assemble.StatusConflict,
			Sources: []assemble.FieldSource{fsrc("doc-a", "CLAIM_FORM", "POL-1001", "POL-1001")},
		}}, nil), nil},
		{"MISSING with Agreed", claimWith([]assemble.AssembledField{{
			Key: "policy_number", Status: assemble.StatusMissing, Agreed: "POL-1001",
		}}, nil), nil},
		{"MISSING with sources", claimWith([]assemble.AssembledField{{
			Key: "policy_number", Status: assemble.StatusMissing,
			Sources: []assemble.FieldSource{fsrc("doc-a", "CLAIM_FORM", "POL-1001", "POL-1001")},
		}}, nil), nil},
		{"NEEDS_REVIEW without review", claimWith([]assemble.AssembledField{{
			Key: "patient_name", Status: assemble.StatusNeedsReview, Agreed: "aarav sharma",
			Sources: []assemble.FieldSource{fsrc("doc-a", "CLAIM_FORM", "Aarav Sharma", "aarav sharma")},
		}}, nil), nil},
		{"NEEDS_REVIEW voteless with Agreed", claimWith([]assemble.AssembledField{
			needsReview("patient_name", "aarav sharma", nil, reviewItem("doc-a", "CLAIM_FORM", extract.StatusAmbiguous, "???")),
		}, nil), nil},
		{"blank conflict key", claimWith(nil, nil), []assemble.ConflictEntry{{Key: ""}}},
		{"duplicate conflict entries", claimWith(nil, nil),
			[]assemble.ConflictEntry{{Key: "policy_number"}, {Key: "policy_number"}}},
		{"unparseable agreed date", claimWith([]assemble.AssembledField{badDate}, nil), nil},
		{"unparseable routed amount", claimWith([]assemble.AssembledField{badAmount}, nil), nil},
		{"orphan conflict entry unused is fine but conflict field", claimWith([]assemble.AssembledField{confNoEntry}, nil), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := tt.claim
			if tt.conflicts != nil {
				c.Conflicts = tt.conflicts
			}
			in, un, err := verifywrap.Map(c, verifywrap.Externals{})
			if err == nil {
				t.Fatalf("Map = (%+v, %+v, nil), want error", in, un)
			}
			if !reflect.DeepEqual(in, verify.Input{}) {
				t.Fatalf("error path Input = %+v, want zero Input", in)
			}
		})
	}
}

// F11a: amount routing by source DocType.
func TestMapAmountRoutingByDocType(t *testing.T) {
	tests := []struct {
		name      string
		docTypes  []string
		wantBill  int64
		wantHas   bool
		wantClaim int64
	}{
		{"bill only", []string{"HOSPITAL_BILL"}, 7950000, true, 0},
		{"claim only", []string{"CLAIM_FORM"}, 0, false, 7950000},
		{"preauth only", []string{"PREAUTH_FORM"}, 0, false, 7950000},
		{"bill and claim", []string{"HOSPITAL_BILL", "CLAIM_FORM"}, 7950000, true, 7950000},
		{"discharge only routes nowhere", []string{"DISCHARGE_SUMMARY"}, 0, false, 0},
		{"bill and discharge", []string{"HOSPITAL_BILL", "DISCHARGE_SUMMARY"}, 7950000, true, 0},
		{"lab only routes nowhere", []string{"LAB_REPORT"}, 0, false, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var sources []assemble.FieldSource
			for i, dt := range tt.docTypes {
				sources = append(sources, fsrc("doc-"+string(rune('a'+i)), dt, "79500.00", "7950000"))
			}
			c := claimWith([]assemble.AssembledField{
				agreed("total_amount_paise", "7950000", "79500.00", sources...),
			}, nil)
			in, _ := mustMap(t, c, verifywrap.Externals{})
			if in.BillTotalPaise != tt.wantBill || in.HasBillTotal != tt.wantHas {
				t.Fatalf("BillTotal = %d has=%v, want %d %v", in.BillTotalPaise, in.HasBillTotal, tt.wantBill, tt.wantHas)
			}
			if in.ClaimedPaise != tt.wantClaim {
				t.Fatalf("ClaimedPaise = %d, want %d", in.ClaimedPaise, tt.wantClaim)
			}
		})
	}
}

// Externals copy verbatim (required params; zero values fire R4/R9).
func TestMapExternalsVerbatim(t *testing.T) {
	c := claimWith([]assemble.AssembledField{
		agreed("policy_number", "POL-1001", "POL-1001", fsrc("doc-a", "CLAIM_FORM", "POL-1001", "POL-1001")),
	}, nil, "CLAIM_FORM")
	ext := verifywrap.Externals{
		PolicyNumber: "POL-999", PolicyPatient: "Vivaan Rao",
		PolicyActive: false, ExternalPolicyOK: false,
		ExternalMismatch: "plan tier mismatch",
		Duplicates:       []string{"doc-1", "doc-2"},
	}
	in, _ := mustMap(t, c, ext)
	if in.PolicyNumber != "POL-999" || in.PolicyPatient != "Vivaan Rao" {
		t.Fatalf("policy externals = %q/%q, want verbatim", in.PolicyNumber, in.PolicyPatient)
	}
	if in.PolicyActive || in.ExternalPolicyOK {
		t.Fatalf("flags = %v/%v, want false/false verbatim", in.PolicyActive, in.ExternalPolicyOK)
	}
	if in.ExternalMismatch != "plan tier mismatch" {
		t.Fatalf("mismatch = %q, want verbatim", in.ExternalMismatch)
	}
	if !reflect.DeepEqual(in.Duplicates, []string{"doc-1", "doc-2"}) {
		t.Fatalf("Duplicates = %+v, want verbatim", in.Duplicates)
	}
	ext.Duplicates[0] = "mutated"
	if in.Duplicates[0] != "doc-1" {
		t.Fatal("Duplicates aliases caller slice; Map must copy")
	}
}

// ---------------------------------------------------------------------------
// End-to-end regression: assemble -> Map -> Verify
// ---------------------------------------------------------------------------

func TestEndToEndCleanPass(t *testing.T) {
	docs := []extract.DocumentFacts{
		efacts("doc-claim", "CLAIM_FORM",
			epresent("claim_number", "CLM-7", "CLM-7", "doc-claim"),
			epresent("policy_number", "POL-1001", "POL-1001", "doc-claim"),
			epresent("patient_name", "Aarav Sharma", "aarav sharma", "doc-claim"),
			epresent("hospital_name", "Fortis Hospital", "fortis hospital", "doc-claim"),
			epresent("admission_date", "2026-08-10", "2026-08-10", "doc-claim"),
			epresent("discharge_date", "2026-08-14", "2026-08-14", "doc-claim"),
			epresent("total_amount_paise", "79500.00", "7950000", "doc-claim")),
		efacts("doc-ds", "DISCHARGE_SUMMARY",
			epresent("claim_number", "CLM-7", "CLM-7", "doc-ds"),
			epresent("policy_number", "POL-1001", "POL-1001", "doc-ds"),
			epresent("patient_name", "Aarav Sharma", "aarav sharma", "doc-ds"),
			epresent("admission_date", "2026-08-10", "2026-08-10", "doc-ds"),
			epresent("discharge_date", "2026-08-14", "2026-08-14", "doc-ds"),
			epresent("diagnosis", "Viral fever", "viral fever", "doc-ds")),
		efacts("doc-bill", "HOSPITAL_BILL",
			epresent("policy_number", "POL-1001", "POL-1001", "doc-bill"),
			epresent("patient_name", "Aarav Sharma", "aarav sharma", "doc-bill"),
			epresent("admission_date", "2026-08-10", "2026-08-10", "doc-bill"),
			epresent("total_amount_paise", "79500.00", "7950000", "doc-bill")),
	}
	claim, err := assemble.Assemble(docs)
	if err != nil {
		t.Fatalf("Assemble = error %v", err)
	}
	in, un, err := verifywrap.Map(claim, extActive())
	if err != nil {
		t.Fatalf("Map = error %v", err)
	}
	if len(un) != 0 {
		t.Fatalf("unresolved = %+v, want none on clean claim", un)
	}
	if in.ClaimedPaise != 7950000 || in.BillTotalPaise != 7950000 || !in.HasBillTotal {
		t.Fatalf("amounts = claimed %d bill %d has=%v, want 7950000/7950000/true",
			in.ClaimedPaise, in.BillTotalPaise, in.HasBillTotal)
	}
	got := verify.Verify(in)
	if !got.Passed {
		t.Fatalf("Verify = %+v, want PASS", got.Exceptions)
	}
	if len(got.Exceptions) != 0 {
		t.Fatalf("exceptions = %+v, want none", got.Exceptions)
	}
}

// A 2-document claim whose claim-side policy number disagrees with the
// policy record fires the known R1 exception (R8 also fires for the
// absent third required document, in R1-R10 emission order).
func TestEndToEndPolicyConflict(t *testing.T) {
	docs := []extract.DocumentFacts{
		efacts("doc-claim", "CLAIM_FORM",
			epresent("policy_number", "POL-1001", "POL-1001", "doc-claim"),
			epresent("patient_name", "Aarav Sharma", "aarav sharma", "doc-claim"),
			epresent("admission_date", "2026-08-10", "2026-08-10", "doc-claim"),
			epresent("discharge_date", "2026-08-14", "2026-08-14", "doc-claim"),
			epresent("total_amount_paise", "79500.00", "7950000", "doc-claim")),
		efacts("doc-bill", "HOSPITAL_BILL",
			epresent("policy_number", "POL-1001", "POL-1001", "doc-bill"),
			epresent("patient_name", "Aarav Sharma", "aarav sharma", "doc-bill"),
			epresent("admission_date", "2026-08-10", "2026-08-10", "doc-bill"),
			epresent("total_amount_paise", "79500.00", "7950000", "doc-bill")),
	}
	claim, err := assemble.Assemble(docs)
	if err != nil {
		t.Fatalf("Assemble = error %v", err)
	}
	ext := extActive()
	ext.PolicyNumber = "POL-999"
	in, _, err := verifywrap.Map(claim, ext)
	if err != nil {
		t.Fatalf("Map = error %v", err)
	}
	got := verify.Verify(in)
	if got.Passed {
		t.Fatal("Verify Passed = true, want false")
	}
	want := []string{verify.CodePolicyNumberConflict, verify.CodeMissingRequiredDocument}
	if !reflect.DeepEqual(codes(got.Exceptions), want) {
		t.Fatalf("codes = %v, want %v", codes(got.Exceptions), want)
	}
}

// Differing admission dates across documents surface as a CONFLICT
// Unresolved entry (HITL), never as R10 input: by binding design only
// AGREED dates enter DocEvidence, and every DocEvidence entry for one
// key holds the same Agreed string, so mapped input cannot fork R10.
// R10 remains covered by the untouched verify suite on hand-built Input.
func TestEndToEndDateConflictSurfacesAsUnresolved(t *testing.T) {
	docs := []extract.DocumentFacts{
		efacts("doc-claim", "CLAIM_FORM",
			epresent("policy_number", "POL-1001", "POL-1001", "doc-claim"),
			epresent("patient_name", "Aarav Sharma", "aarav sharma", "doc-claim"),
			epresent("admission_date", "2026-08-10", "2026-08-10", "doc-claim"),
			epresent("total_amount_paise", "79500.00", "7950000", "doc-claim")),
		efacts("doc-ds", "DISCHARGE_SUMMARY",
			epresent("policy_number", "POL-1001", "POL-1001", "doc-ds"),
			epresent("patient_name", "Aarav Sharma", "aarav sharma", "doc-ds"),
			epresent("admission_date", "2026-08-11", "2026-08-11", "doc-ds")),
	}
	claim, err := assemble.Assemble(docs)
	if err != nil {
		t.Fatalf("Assemble = error %v", err)
	}
	in, un, err := verifywrap.Map(claim, extActive())
	if err != nil {
		t.Fatalf("Map = error %v", err)
	}
	found := false
	for _, u := range un {
		if u.Key == "admission_date" {
			found = true
			if u.Status != assemble.StatusConflict || u.Conflict == nil {
				t.Fatalf("admission_date unresolved = %+v, want CONFLICT with entry", u)
			}
		}
	}
	if !found {
		t.Fatalf("no admission_date Unresolved in %+v", un)
	}
	if in.HasAdmission {
		t.Fatalf("HasAdmission = true, want false under CONFLICT")
	}
	for _, m := range in.DocEvidence {
		if _, ok := m["admission_date"]; ok {
			t.Fatalf("DocEvidence carries conflicted date: %+v", in.DocEvidence)
		}
	}
	got := verify.Verify(in)
	if hasCode(got.Exceptions, verify.CodeDateConflict) {
		t.Fatalf("R10 fired on conflict-mapped input (must stay HITL): %+v", got.Exceptions)
	}
}
