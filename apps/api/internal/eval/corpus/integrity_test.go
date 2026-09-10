package corpus

import (
	"os"
	"path/filepath"
	"testing"
)

// realFixturesRoot probes both plausible homes for the sibling-generated
// corpus: apps/api/fixtures/parser_eval and the repo-root
// fixtures/parser_eval. It skips when neither exists — the sibling agent
// owns generation.
func realFixturesRoot(t *testing.T) string {
	t.Helper()
	for _, c := range []string{"../../../fixtures/parser_eval", "../../../../fixtures/parser_eval", "../../../../../fixtures/parser_eval"} {
		if st, err := os.Stat(filepath.Join(c, "manifest.yaml")); err == nil && !st.IsDir() {
			abs, err := filepath.Abs(c)
			if err != nil {
				t.Fatal(err)
			}
			return abs
		}
	}
	t.Skip("sibling agent has not generated fixtures/parser_eval yet")
	return ""
}

func TestVerifyCorpus_RealFixtures(t *testing.T) {
	if err := VerifyCorpus(realFixturesRoot(t)); err != nil {
		t.Fatalf("VerifyCorpus: %v", err)
	}
}

func TestLoadTrustedDocument_RealFixtures(t *testing.T) {
	m, err := LoadManifest(realFixturesRoot(t))
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if len(m.Cases) == 0 {
		t.Fatal("manifest lists zero cases")
	}
	doc, err := LoadTrustedDocument(m.Cases[0], "eval-tenant", "eval-claim-1")
	if err != nil {
		t.Fatalf("LoadTrustedDocument: %v", err)
	}
	if doc.MediaType != "application/pdf" {
		t.Fatalf("media type = %q, want application/pdf", doc.MediaType)
	}
	if doc.SHA256 != m.Cases[0].SHA256 {
		t.Fatalf("trusted sha %q != manifest %q", doc.SHA256, m.Cases[0].SHA256)
	}
	if doc.DocumentID != m.Cases[0].ID {
		t.Fatalf("document id = %q, want %q", doc.DocumentID, m.Cases[0].ID)
	}
}

// TestRealGoldensValidate asserts every real case's expected.json satisfies
// the golden invariants for its document type. Corrupted/blank fixtures
// (CASE-045/CASE-043) carry truth for their bytes as-is; the unknown-type
// exemption covers the blank case.
func TestRealGoldensValidate(t *testing.T) {
	root := realFixturesRoot(t)
	m, err := LoadManifest(root)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	for _, c := range m.Cases {
		g, err := LoadGolden(c.Dir())
		if err != nil {
			t.Fatalf("%s: LoadGolden: %v", c.ID, err)
		}
		if err := g.Validate(c.DocumentType); err != nil {
			t.Fatalf("%s (%s): %v", c.ID, c.DocumentType, err)
		}
	}
}

func TestVerifyCorpus_TempCorpus(t *testing.T) {
	if err := VerifyCorpus(seedTempCorpus(t)); err != nil {
		t.Fatalf("VerifyCorpus: %v", err)
	}
}

func TestVerifyCorpus_TamperedPDF(t *testing.T) {
	root := seedTempCorpus(t)
	pdf := filepath.Join(root, "v1", "CASE-001", "document.pdf")
	b, err := os.ReadFile(pdf)
	if err != nil {
		t.Fatal(err)
	}
	b[0] ^= 0xff
	if err := os.WriteFile(pdf, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyCorpus(root); err == nil {
		t.Fatal("expected mismatch error for tampered PDF, got nil")
	}
}

func TestVerifyCorpus_MissingPDF(t *testing.T) {
	root := seedTempCorpus(t)
	if err := os.Remove(filepath.Join(root, "v1", "CASE-002", "document.pdf")); err != nil {
		t.Fatal(err)
	}
	if err := VerifyCorpus(root); err == nil {
		t.Fatal("expected missing-file error, got nil")
	}
}

func TestVerifyCorpus_MissingSidecar(t *testing.T) {
	root := seedTempCorpus(t)
	if err := os.Remove(filepath.Join(root, "v1", "CASE-001", "expected.json")); err != nil {
		t.Fatal(err)
	}
	if err := VerifyCorpus(root); err == nil {
		t.Fatal("expected missing-sidecar error, got nil")
	}
}

func TestVerifyCorpus_ExtraFile(t *testing.T) {
	root := seedTempCorpus(t)
	writeFile(t, filepath.Join(root, "v1", "CASE-001", "notes.txt"), "stray")
	if err := VerifyCorpus(root); err == nil {
		t.Fatal("expected extra-file error, got nil")
	}
}
