package corpus

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func shaOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// seedTempCorpus builds a two-case corpus exercising block-style (CASE-001)
// and flow-style (CASE-002) expected_fields. It returns the fixtures root.
func seedTempCorpus(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	sha1, sha2 := shaOf("pdf-bytes-1"), shaOf("pdf-bytes-2")
	writeFile(t, filepath.Join(root, "manifest.yaml"), `version: v1
cases:
  - case_id: CASE-001
    path: v1/CASE-001/document.pdf
    sha256: `+sha1+`
    document_type: bill
    difficulty: D2
  - case_id: CASE-002
    path: v1/CASE-002/document.pdf
    sha256: `+sha2+`
    document_type: discharge_summary
    difficulty: D0
`)
	writeFile(t, filepath.Join(root, "v1/CASE-001/document.pdf"), "pdf-bytes-1")
	writeFile(t, filepath.Join(root, "v1/CASE-002/document.pdf"), "pdf-bytes-2")
	writeFile(t, filepath.Join(root, "v1/CASE-001/metadata.yaml"), `# block-style list
case_id: CASE-001
document_type: bill
difficulty: D2
source_type: synthetic
synthetic_seed: 42
pii_status: synthetic
license: internal-use
expected_fields:
  - claim_number
  - total_amount_paise
expected_tables: true
expected_provenance: page
`)
	writeFile(t, filepath.Join(root, "v1/CASE-002/metadata.yaml"), `case_id: CASE-002
document_type: discharge_summary
difficulty: D0
source_type: synthetic
synthetic_seed: 7
pii_status: synthetic
license: internal-use
expected_fields: [claim_number, patient_name]
expected_tables: false
expected_provenance: page
`)
	writeFile(t, filepath.Join(root, "v1/CASE-001/expected.json"), `{"claim_number":"CLM-1"}`)
	writeFile(t, filepath.Join(root, "v1/CASE-002/expected.json"), `{"claim_number":"CLM-2"}`)
	return root
}

func TestLoadManifest_HappyPath(t *testing.T) {
	m, err := LoadManifest(seedTempCorpus(t))
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if m.Version != "v1" {
		t.Fatalf("version = %q, want v1", m.Version)
	}
	if len(m.Cases) != 2 {
		t.Fatalf("cases = %d, want 2", len(m.Cases))
	}
	a, b := m.Cases[0], m.Cases[1]
	if a.ID != "CASE-001" || a.DocumentType != "bill" || a.Difficulty != "D2" {
		t.Fatalf("case 0 identity = %+v", a)
	}
	if a.SourceType != "synthetic" || a.Seed != "42" || a.PIIStatus != "synthetic" || a.License != "internal-use" {
		t.Fatalf("case 0 provenance = %+v", a)
	}
	if len(a.ExpectedFields) != 2 || a.ExpectedFields[0] != "claim_number" || a.ExpectedFields[1] != "total_amount_paise" {
		t.Fatalf("case 0 fields = %v", a.ExpectedFields)
	}
	if !a.ExpectedTables || a.ExpectedProvenance != "page" {
		t.Fatalf("case 0 tables/provenance = %v %q", a.ExpectedTables, a.ExpectedProvenance)
	}
	if !strings.HasSuffix(a.PDFPath, filepath.Join("v1", "CASE-001", "document.pdf")) {
		t.Fatalf("case 0 pdf path = %q", a.PDFPath)
	}
	if b.ID != "CASE-002" || len(b.ExpectedFields) != 2 || b.ExpectedTables {
		t.Fatalf("case 1 = %+v", b)
	}
	if filepath.Dir(a.PDFPath) == "" || a.SHA256 != shaOf("pdf-bytes-1") {
		t.Fatalf("case 0 path/sha = %q %q", a.PDFPath, a.SHA256)
	}
}

func TestLoadManifest_Errors(t *testing.T) {
	validSHA := shaOf("x")
	entry := func(extra string) string {
		return "version: v1\ncases:\n  - case_id: CASE-001\n    path: v1/CASE-001/document.pdf\n    sha256: " + validSHA + "\n" + extra
	}
	tests := []struct {
		name     string
		manifest string
		metadata string // "" means do not write metadata.yaml
	}{
		{"missing_case_id", "version: v1\ncases:\n  - path: p.pdf\n    sha256: " + validSHA + "\n", ""},
		{"missing_sha", "version: v1\ncases:\n  - case_id: C\n    path: p.pdf\n", ""},
		{"bad_sha", "version: v1\ncases:\n  - case_id: C\n    path: p.pdf\n    sha256: nothex\n", ""},
		{"absolute_path", entry("")[:0] + "version: v1\ncases:\n  - case_id: C\n    path: /etc/passwd\n    sha256: " + validSHA + "\n", ""},
		{"dotdot_path", "version: v1\ncases:\n  - case_id: C\n    path: ../escape.pdf\n    sha256: " + validSHA + "\n", ""},
		{"duplicate_id", "version: v1\ncases:\n  - case_id: C\n    path: a.pdf\n    sha256: " + validSHA + "\n  - case_id: C\n    path: b.pdf\n    sha256: " + validSHA + "\n", ""},
		{"zero_cases", "version: v1\ncases: []\n", ""},
		{"metadata_case_mismatch", entry(""), "case_id: OTHER\n"},
		{"metadata_type_drift", entry("    document_type: bill\n"), "case_id: CASE-001\ndocument_type: receipt\n"},
		{"metadata_bad_bool", entry(""), "case_id: CASE-001\nexpected_tables: maybe\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, filepath.Join(root, "manifest.yaml"), tt.manifest)
			if tt.metadata != "" {
				writeFile(t, filepath.Join(root, "v1/CASE-001/metadata.yaml"), tt.metadata)
			}
			if _, err := LoadManifest(root); err == nil {
				t.Fatalf("expected error, got nil")
			}
		})
	}
}

func TestLoadManifest_Defaults(t *testing.T) {
	// No version → "1"; no path → derived v1/{case_id}/document.pdf.
	// Both are the real generator-manifest shape.
	root := t.TempDir()
	sha := shaOf("x")
	writeFile(t, filepath.Join(root, "manifest.yaml"),
		"case_count: 1\ncases:\n- case_id: C\n  sha256: "+sha+"\n")
	m, err := LoadManifest(root)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if m.Version != "1" {
		t.Fatalf("version = %q, want default %q", m.Version, "1")
	}
	if len(m.Cases) != 1 || !strings.HasSuffix(m.Cases[0].PDFPath, filepath.Join("v1", "C", "document.pdf")) {
		t.Fatalf("derived path = %+v", m.Cases)
	}
}

func TestLoadManifest_MissingFile(t *testing.T) {
	if _, err := LoadManifest(t.TempDir()); err == nil {
		t.Fatal("expected error for missing manifest.yaml, got nil")
	}
}

func TestLoadManifest_NoMetadataStaysManifestOnly(t *testing.T) {
	root := t.TempDir()
	sha := shaOf("x")
	writeFile(t, filepath.Join(root, "manifest.yaml"), "version: v1\ncases:\n  - case_id: CASE-009\n    path: v1/CASE-009/document.pdf\n    sha256: "+sha+"\n    document_type: bill\n    difficulty: D1\n")
	m, err := LoadManifest(root)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if len(m.Cases) != 1 || m.Cases[0].SourceType != "" || m.Cases[0].ExpectedFields != nil {
		t.Fatalf("unexpected enrichment without metadata: %+v", m.Cases[0])
	}
}
