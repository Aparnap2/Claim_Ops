package corpus

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"claimops-api/internal/documents/admission"
)

// minPDF returns bytes that net/http.DetectContentType sniffs as
// application/pdf (prefix-driven), so admission accepts them.
func minPDF(filler string) []byte {
	return []byte("%PDF-1.4\n" + filler + "\n%%EOF\n")
}

func writeCasePDF(t *testing.T, content []byte) (dir, pdfPath string) {
	t.Helper()
	dir = t.TempDir()
	pdfPath = filepath.Join(dir, "document.pdf")
	if err := os.WriteFile(pdfPath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, pdfPath
}

func shaHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestLoadTrustedDocument_HappyPath(t *testing.T) {
	content := minPDF("happy-path corpus fixture")
	_, pdfPath := writeCasePDF(t, content)
	c := Case{ID: "CASE-001", DocumentType: "bill", PDFPath: pdfPath, SHA256: shaHex(content)}

	doc, err := LoadTrustedDocument(c, "tenant-a", "claim-1")
	if err != nil {
		t.Fatalf("LoadTrustedDocument: %v", err)
	}
	if doc.DocumentID != "CASE-001" {
		t.Fatalf("document id = %q", doc.DocumentID)
	}
	if doc.MediaType != "application/pdf" {
		t.Fatalf("media type = %q", doc.MediaType)
	}
	if doc.SHA256 != shaHex(content) {
		t.Fatalf("sha = %q", doc.SHA256)
	}
	if doc.SizeBytes != int64(len(content)) || string(doc.Content) != string(content) {
		t.Fatal("content/size mismatch")
	}
	// Admitted values flow through: filename is the admitted base name.
	if doc.FileName != "document.pdf" {
		t.Fatalf("file name = %q", doc.FileName)
	}
}

func TestLoadTrustedDocument_TamperedBytesFailBeforeAdmission(t *testing.T) {
	original := minPDF("tamper me")
	tampered := append([]byte(nil), original...)
	tampered[len(tampered)-2] ^= 0xff // flip one byte in the temp copy
	_, pdfPath := writeCasePDF(t, tampered)
	// Manifest still pins the ORIGINAL hash.
	c := Case{ID: "CASE-001", PDFPath: pdfPath, SHA256: shaHex(original)}

	_, err := LoadTrustedDocument(c, "tenant-a", "claim-1")
	if err == nil {
		t.Fatal("expected sha mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("expected sha256 mismatch error, got %v", err)
	}
	if errors.Is(err, admission.ErrAdmission) {
		t.Fatalf("tamper must fail BEFORE admission, but got admission error: %v", err)
	}
}

func TestLoadTrustedDocument_NoBypassOversizedRejectedViaAdmission(t *testing.T) {
	// This test pins the compile-time truth that the loader routes through
	// admission: it consults admission.DefaultPolicy().MaxBytes and demands
	// the rejection carry the admission sentinel.
	policy := admission.DefaultPolicy()
	if policy.MaxBytes <= 0 {
		t.Fatal("admission.DefaultPolicy().MaxBytes must be > 0: loader consults admission")
	}
	content := make([]byte, policy.MaxBytes+1)
	copy(content, "%PDF-1.4\n") // sniffable prefix; size alone must reject
	_, pdfPath := writeCasePDF(t, content)
	c := Case{ID: "CASE-BIG", PDFPath: pdfPath, SHA256: shaHex(content)}

	_, err := LoadTrustedDocument(c, "tenant-a", "claim-1")
	if err == nil {
		t.Fatal("expected admission rejection for oversized input, got nil")
	}
	if !errors.Is(err, admission.ErrAdmission) {
		t.Fatalf("expected errors.Is(err, admission.ErrAdmission), got %v", err)
	}
}

func TestLoadTrustedDocument_BlankTenantRejected(t *testing.T) {
	content := minPDF("blank tenant")
	_, pdfPath := writeCasePDF(t, content)
	c := Case{ID: "CASE-001", PDFPath: pdfPath, SHA256: shaHex(content)}

	if _, err := LoadTrustedDocument(c, "  ", "claim-1"); err == nil {
		t.Fatal("expected tenant validation error, got nil")
	}
}

func TestLoadTrustedDocument_MissingFile(t *testing.T) {
	c := Case{ID: "CASE-404", PDFPath: filepath.Join(t.TempDir(), "gone.pdf"), SHA256: shaHex(minPDF("x"))}
	if _, err := LoadTrustedDocument(c, "tenant-a", "claim-1"); err == nil {
		t.Fatal("expected read error, got nil")
	}
}

func TestLoadTrustedDocument_NonAllowlistedExtension(t *testing.T) {
	content := minPDF("wrong extension")
	dir := t.TempDir()
	p := filepath.Join(dir, "document.exe")
	if err := os.WriteFile(p, content, 0o600); err != nil {
		t.Fatal(err)
	}
	c := Case{ID: "CASE-EXE", PDFPath: p, SHA256: shaHex(content)}
	if _, err := LoadTrustedDocument(c, "tenant-a", "claim-1"); err == nil {
		t.Fatal("expected extension error, got nil")
	}
}
