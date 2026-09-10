package bench

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"claimops-api/internal/parser"
)

// ---------------------------------------------------------------------------
// Stub parser.Parser.
// ---------------------------------------------------------------------------

type stubParser struct {
	name    string
	version string
	parse   func(ctx context.Context, in parser.TrustedDocument) (parser.ParsedDocument, error)
}

func (s *stubParser) Name() string { return s.name }

func (s *stubParser) Version() string { return s.version }

func (s *stubParser) Parse(ctx context.Context, in parser.TrustedDocument) (parser.ParsedDocument, error) {
	return s.parse(ctx, in)
}

// validArtifact builds a canonical artifact that passes Validate: one page,
// one text block, metadata stamped from the stub parser + input sha.
func validArtifact(docID, parserName, parserVersion, sha string) parser.ParsedDocument {
	return parser.ParsedDocument{
		DocumentID: docID,
		Pages: []parser.ParsedPage{
			{
				Number: 1,
				Blocks: []parser.ContentBlock{
					{
						ID:         "b1",
						Type:       parser.BlockText,
						Text:       "synthetic fixture text",
						Evidence:   parser.EvidenceLocation{DocumentID: docID, Page: 1, BlockID: "b1"},
						Confidence: 1.0,
					},
				},
			},
		},
		Metadata: parser.DocumentMetadata{
			ParserName:    parserName,
			ParserVersion: parserVersion,
			SourceSHA256:  sha,
			SourceMedia:   "application/pdf",
			PageCount:     1,
		},
	}
}

// invalidArtifact breaks the canonical schema (page numbered 2 in a 1-page
// document) so Validate fails.
func invalidArtifact(docID string) parser.ParsedDocument {
	d := validArtifact(docID, "stub", "0.0-test", "sha")
	d.Pages[0].Number = 2
	return d
}

func okStub() *stubParser {
	return &stubParser{
		name:    "stub",
		version: "0.0-test",
		parse: func(ctx context.Context, in parser.TrustedDocument) (parser.ParsedDocument, error) {
			return validArtifact(in.DocumentID, "stub", "0.0-test", in.SHA256), nil
		},
	}
}

// ---------------------------------------------------------------------------
// Temp-corpus builder: manifest + admission-passing PDFs + goldens, with
// correct hashes computed at runtime. Nothing here echoes golden values.
// ---------------------------------------------------------------------------

const (
	testTenant      = "eval-tenant"
	testClaimPrefix = "eval-claim"
)

// benchPDF returns admission-passing PDF bytes: a %PDF prefix (sniffed as
// application/pdf) plus padding, mirroring existing corpus test fixtures.
func benchPDF(filler string) []byte {
	return []byte("%PDF-1.4\n" + filler + "\n" + strings.Repeat(" ", 600) + "\n%%EOF\n")
}

func benchSHA(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

type benchCaseSpec struct {
	id         string
	docType    string
	difficulty string
	// pdf overrides the fixture bytes (default: benchPDF("fixture "+id)).
	pdf []byte
	// pdfName overrides the fixture file name (default: document.pdf).
	// A non-allowlisted extension exercises the loader-reject path.
	pdfName string
	// golden overrides expected.json (default: valid golden for docType).
	golden string
	// corruptSHA writes a wrong sha256 into the manifest (VerifyCorpus fails).
	corruptSHA bool
}

func defaultGolden(docType, id string) string {
	if docType == "hospital_bill" {
		return `{"claim_number":"CLM-` + id + `","total_amount_paise":10000}`
	}
	return `{"claim_number":"CLM-` + id + `"}`
}

func defaultMetadata(id, docType, difficulty string) string {
	return "case_id: " + id + "\n" +
		"document_type: " + docType + "\n" +
		"difficulty: " + difficulty + "\n" +
		"source_type: synthetic\n" +
		"synthetic_seed: 1\n" +
		"pii_status: synthetic\n" +
		"license: internal-use\n" +
		"expected_fields: [claim_number]\n" +
		"expected_tables: false\n" +
		"expected_provenance: page\n"
}

// seedBenchCorpus builds a temp fixtures root from specs and returns the
// root plus the exact fixture bytes per case id (for InputBytes assertions).
func seedBenchCorpus(t *testing.T, specs []benchCaseSpec) (string, map[string][]byte) {
	t.Helper()
	root := t.TempDir()
	pdfs := map[string][]byte{}
	var manifest strings.Builder
	manifest.WriteString("version: vtest\ncases:\n")
	for _, s := range specs {
		pdf := s.pdf
		if pdf == nil {
			pdf = benchPDF("fixture " + s.id)
		}
		name := s.pdfName
		if name == "" {
			name = "document.pdf"
		}
		rel := "v1/" + s.id + "/" + name
		pdfs[s.id] = pdf
		if err := os.MkdirAll(filepath.Join(root, "v1", s.id), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, rel), pdf, 0o600); err != nil {
			t.Fatal(err)
		}
		sha := benchSHA(pdf)
		if s.corruptSHA {
			sha = benchSHA([]byte("tampered:" + s.id))
		}
		fmt.Fprintf(&manifest, "  - case_id: %s\n    path: %s\n    sha256: %s\n    document_type: %s\n    difficulty: %s\n",
			s.id, rel, sha, s.docType, s.difficulty)
		golden := s.golden
		if golden == "" {
			golden = defaultGolden(s.docType, s.id)
		}
		if err := os.WriteFile(filepath.Join(root, "v1", s.id, "expected.json"), []byte(golden), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "v1", s.id, "metadata.yaml"), []byte(defaultMetadata(s.id, s.docType, s.difficulty)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "manifest.yaml"), []byte(manifest.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return root, pdfs
}

func findCase(rep Report, id string) CaseResult {
	for _, c := range rep.Cases {
		if c.CaseID == id {
			return c
		}
	}
	return CaseResult{}
}

func hasFailure(cr CaseResult, code FailureCode) bool {
	for _, f := range cr.Failures {
		if f == code {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Run tests.
// ---------------------------------------------------------------------------

func TestRunHappyPathMeta(t *testing.T) {
	root, pdfs := seedBenchCorpus(t, []benchCaseSpec{
		{id: "CASE-001", docType: "discharge_summary", difficulty: "D0"},
		{id: "CASE-002", docType: "hospital_bill", difficulty: "D1"},
	})
	rep, err := Run(context.Background(), root, okStub(), testTenant, testClaimPrefix)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Meta.CorpusVersion != "vtest" {
		t.Fatalf("corpus version = %q, want vtest", rep.Meta.CorpusVersion)
	}
	if rep.Meta.CorpusCases != 2 {
		t.Fatalf("corpus cases = %d, want 2", rep.Meta.CorpusCases)
	}
	if rep.Meta.ParserName != "stub" || rep.Meta.ParserVersion != "0.0-test" || rep.Meta.AdapterName != "stub" {
		t.Fatalf("parser meta = %+v", rep.Meta)
	}
	if len(rep.Cases) != 2 || rep.Cases[0].CaseID != "CASE-001" || rep.Cases[1].CaseID != "CASE-002" {
		t.Fatalf("manifest order not preserved: %+v", rep.Cases)
	}
	for _, id := range []string{"CASE-001", "CASE-002"} {
		cr := findCase(rep, id)
		if !cr.ParseOK {
			t.Fatalf("%s: ParseOK=false, failures=%v", id, cr.Failures)
		}
		if cr.InputBytes != int64(len(pdfs[id])) {
			t.Fatalf("%s: InputBytes=%d, want %d", id, cr.InputBytes, len(pdfs[id]))
		}
		if cr.LatencyMs < 0 {
			t.Fatalf("%s: negative latency", id)
		}
		if cr.DocumentType == "" || cr.Difficulty == "" {
			t.Fatalf("%s: identity fields not enforced: %+v", id, cr)
		}
	}
	if rep.Overall.Cases != 2 || rep.Overall.ParseOK != 2 {
		t.Fatalf("overall = %+v", rep.Overall)
	}
}

func TestRunVerifyCorpusFailsFast(t *testing.T) {
	root, _ := seedBenchCorpus(t, []benchCaseSpec{
		{id: "CASE-001", docType: "discharge_summary", difficulty: "D0", corruptSHA: true},
	})
	if _, err := Run(context.Background(), root, okStub(), testTenant, testClaimPrefix); err == nil {
		t.Fatal("expected VerifyCorpus failure, got nil")
	}
}

func TestRunGoldenInvalidAborts(t *testing.T) {
	root, _ := seedBenchCorpus(t, []benchCaseSpec{
		{id: "CASE-BAD", docType: "discharge_summary", difficulty: "D0", golden: `{"patient_name":"nobody"}`},
	})
	_, err := Run(context.Background(), root, okStub(), testTenant, testClaimPrefix)
	if err == nil {
		t.Fatal("expected golden-invalid abort, got nil")
	}
	if !strings.Contains(err.Error(), "CASE-BAD") {
		t.Fatalf("error must carry case id, got: %v", err)
	}
}

func TestRunLoaderRejectClassifies(t *testing.T) {
	// Oversized fixture: sha matches (VerifyCorpus passes) but admission
	// rejects the 10MiB+1 body, so the loader rejects → FailParseFailure.
	// 10MiB is admission.DefaultPolicy().MaxBytes; hardcoded here to keep
	// test imports to stdlib + parser + corpus.
	big := make([]byte, (10<<20)+1)
	copy(big, "%PDF-1.4\n")
	root, _ := seedBenchCorpus(t, []benchCaseSpec{
		{id: "CASE-BIG", docType: "discharge_summary", difficulty: "D0", pdf: big},
		{id: "CASE-OK", docType: "discharge_summary", difficulty: "D0"},
	})
	rep, err := Run(context.Background(), root, okStub(), testTenant, testClaimPrefix)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	bad := findCase(rep, "CASE-BIG")
	if bad.ParseOK {
		t.Fatal("oversized fixture must not parse ok")
	}
	if !hasFailure(bad, FailParseFailure) || len(bad.Failures) != 1 {
		t.Fatalf("loader reject must map to exactly [PARSE_FAILURE], got %v", bad.Failures)
	}
	if good := findCase(rep, "CASE-OK"); !good.ParseOK {
		t.Fatalf("sibling case must still pass: %+v", good)
	}
}

func TestRunParseErrorClassifies(t *testing.T) {
	root, pdfs := seedBenchCorpus(t, []benchCaseSpec{
		{id: "CASE-FAIL", docType: "discharge_summary", difficulty: "D0"},
		{id: "CASE-UNSUP", docType: "discharge_summary", difficulty: "D0"},
	})
	p := &stubParser{
		name: "stub", version: "0.0-test",
		parse: func(ctx context.Context, in parser.TrustedDocument) (parser.ParsedDocument, error) {
			switch in.DocumentID {
			case "CASE-UNSUP":
				return parser.ParsedDocument{}, fmt.Errorf("stub cannot do images: %w", parser.ErrUnsupportedMediaType)
			default:
				return parser.ParsedDocument{}, fmt.Errorf("stub boom: %w", parser.ErrParseFailure)
			}
		},
	}
	rep, err := Run(context.Background(), root, p, testTenant, testClaimPrefix)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	fail := findCase(rep, "CASE-FAIL")
	if fail.ParseOK || !hasFailure(fail, FailParseFailure) {
		t.Fatalf("parse failure misclassified: %+v", fail)
	}
	if fail.InputBytes != int64(len(pdfs["CASE-FAIL"])) {
		t.Fatalf("InputBytes must be set on parse-error path, got %d", fail.InputBytes)
	}
	unsup := findCase(rep, "CASE-UNSUP")
	if unsup.ParseOK || !hasFailure(unsup, FailUnsupportedMedia) {
		t.Fatalf("unsupported media misclassified: %+v", unsup)
	}
}

func TestRunInvalidArtifactClassifies(t *testing.T) {
	root, _ := seedBenchCorpus(t, []benchCaseSpec{
		{id: "CASE-001", docType: "discharge_summary", difficulty: "D0"},
	})
	p := &stubParser{
		name: "stub", version: "0.0-test",
		parse: func(ctx context.Context, in parser.TrustedDocument) (parser.ParsedDocument, error) {
			return invalidArtifact(in.DocumentID), nil
		},
	}
	rep, err := Run(context.Background(), root, p, testTenant, testClaimPrefix)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	cr := findCase(rep, "CASE-001")
	if cr.ParseOK {
		t.Fatal("invalid artifact must not parse ok")
	}
	if !hasFailure(cr, FailPageStructure) || len(cr.Failures) != 1 {
		t.Fatalf("invalid artifact must map to exactly [PAGE_STRUCTURE_MISMATCH], got %v", cr.Failures)
	}
}

func TestRunContextCancelAborts(t *testing.T) {
	root, _ := seedBenchCorpus(t, []benchCaseSpec{
		{id: "CASE-001", docType: "discharge_summary", difficulty: "D0"},
		{id: "CASE-002", docType: "discharge_summary", difficulty: "D0"},
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Run(ctx, root, okStub(), testTenant, testClaimPrefix); err == nil {
		t.Fatal("expected context error, got nil")
	}
	// Mid-run cancellation: parser cancels during the first case; the run
	// must abort before completing the second.
	ctx2, cancel2 := context.WithCancel(context.Background())
	p := &stubParser{
		name: "stub", version: "0.0-test",
		parse: func(ctx context.Context, in parser.TrustedDocument) (parser.ParsedDocument, error) {
			cancel2()
			return validArtifact(in.DocumentID, "stub", "0.0-test", in.SHA256), nil
		},
	}
	if _, err := Run(ctx2, root, p, testTenant, testClaimPrefix); err == nil {
		t.Fatal("expected mid-run context abort, got nil")
	}
}

func TestRunWorstCasesOrdering(t *testing.T) {
	cases := []CaseResult{
		{CaseID: "B", ParseOK: true, Fields: []FieldScore{{Key: "a", Match: MatchMissing}}, Tables: TableScore{Verdict: "TABLE_PASS"}},
		{CaseID: "A", ParseOK: true, Fields: []FieldScore{{Key: "a", Match: MatchMissing}}, Tables: TableScore{Verdict: "TABLE_PASS"}},
		{CaseID: "C", ParseOK: false, Tables: TableScore{Verdict: "TABLE_MISSED"}},
		{CaseID: "D", ParseOK: true, Fields: []FieldScore{{Key: "a", Match: MatchExact}}, Tables: TableScore{Verdict: "TABLE_PASS"}},
	}
	got := worstCases(cases, 8)
	// C (0 bad fields but table missed + parse fail) sorts after the
	// 1-bad-field A/B pair; A before B by id tiebreak; D last.
	want := []string{"A", "B", "C", "D"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("worst = %v, want %v", got, want)
	}
	if got := worstCases(cases, 2); len(got) != 2 || got[0] != "A" || got[1] != "B" {
		t.Fatalf("top-2 worst = %v", got)
	}
}

func TestRunAggregateBuckets(t *testing.T) {
	rep := Report{Cases: []CaseResult{
		{CaseID: "X", ParseOK: true, Difficulty: "D0", DocumentType: "bill",
			Fields:   []FieldScore{{Key: "a", Match: MatchExact}, {Key: "b", Match: MatchIncorrect}},
			Tables:   TableScore{Verdict: "TABLE_PASS"},
			Failures: []FailureCode{FailFieldIncorrect}},
		{CaseID: "Y", ParseOK: false, Difficulty: "D0", DocumentType: "bill",
			Failures: []FailureCode{FailParseFailure}},
	}}
	aggregate(&rep)
	o := rep.Overall
	if o.Cases != 2 || o.ParseOK != 1 || o.FieldsExact != 1 || o.FieldsIncorrect != 1 {
		t.Fatalf("overall = %+v", o)
	}
	if o.TablesPass != 1 || o.TablesMissed != 0 {
		t.Fatalf("tables = %+v", o)
	}
	if o.Failures["FIELD_INCORRECT"] != 1 || o.Failures["PARSE_FAILURE"] != 1 {
		t.Fatalf("failures = %+v", o.Failures)
	}
	if len(rep.WorstCases) != 2 || rep.WorstCases[0] != "X" {
		t.Fatalf("worst = %v", rep.WorstCases)
	}
	if rep.ByDifficulty["D0"].Cases != 2 || rep.ByType["bill"].Cases != 2 {
		t.Fatalf("groups = %+v / %+v", rep.ByDifficulty, rep.ByType)
	}
}
