// Package admission_test covers the admission boundary with a
// deterministic, table-driven golden matrix: no I/O, no clock, no
// network. Magic-byte fixtures carry the minimal prefixes
// net/http.DetectContentType keys on, padded past 512 bytes for sniff
// reliability.
package admission_test

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"claimops-api/internal/documents/admission"
)

// padded returns a 600-byte body starting with prefix (spaces after).
// DetectContentType sniffs the first 512 bytes, so every fixture carries
// its magic well inside the window.
func padded(prefix []byte) []byte {
	out := make([]byte, 600)
	copy(out, prefix)
	for i := len(prefix); i < len(out); i++ {
		out[i] = ' '
	}
	return out
}

func pdfBody() []byte {
	return padded([]byte("%PDF-1.4\nvalid pdf content\n"))
}

func jpegBody() []byte {
	return padded([]byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F'})
}

func pngBody() []byte {
	return padded([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A})
}

func tiffBody() []byte {
	return padded([]byte{'I', 'I', '*', 0x00})
}

func elfBody() []byte {
	return padded([]byte{0x7F, 'E', 'L', 'F', 0x02, 0x01, 0x01, 0x00})
}

func htmlBody() []byte {
	return padded([]byte("<html><head><title>x</title></head><body>hello</body></html>"))
}

func exeBody() []byte {
	return padded([]byte("MZ\x90\x00Windows executable payload"))
}

// TestValidateGoldenMatrix is the admission acceptance matrix: valid
// PDF/JPEG/PNG/TIFF are accepted; everything else — oversize, empty,
// sniff mismatches, unsupported MIME, traversal, null bytes, overlong
// and non-allowlist names, valid extension with wrong bytes — is
// rejected with an error wrapping ErrAdmission.
func TestValidateGoldenMatrix(t *testing.T) {
	small := admission.AdmissionPolicy{
		MaxBytes:         16,
		AllowedTypes:     admission.DefaultPolicy().AllowedTypes,
		MaxFilenameBytes: 255,
	}
	longName := strings.Repeat("a", 296) + ".pdf" // 300 bytes > 255

	cases := []struct {
		name      string
		filename  string
		declared  string
		body      []byte
		policy    admission.AdmissionPolicy // zero value selects DefaultPolicy
		wantMedia string                    // set when the case must be accepted
	}{
		{"valid PDF accept", "claim_form.pdf", "application/pdf", pdfBody(), admission.AdmissionPolicy{}, "application/pdf"},
		{"valid JPEG accept", "photo.jpg", "image/jpeg", jpegBody(), admission.AdmissionPolicy{}, "image/jpeg"},
		{"valid PNG accept", "scan.png", "image/png", pngBody(), admission.AdmissionPolicy{}, "image/png"},
		{"valid TIFF accept", "scan.tiff", "image/tiff", tiffBody(), admission.AdmissionPolicy{}, "image/tiff"},
		{"valid TIFF short ext accept", "scan.tif", "image/tiff", tiffBody(), admission.AdmissionPolicy{}, "image/tiff"},
		{"padded filename trims", "  bill.pdf  ", "application/pdf", pdfBody(), admission.AdmissionPolicy{}, "application/pdf"},
		{"dots inside name accept", "archive..2024.pdf", "application/pdf", pdfBody(), admission.AdmissionPolicy{}, "application/pdf"},

		{"body over max reject", "bill.pdf", "application/pdf", pdfBody(), small, ""},
		{"empty body reject", "bill.pdf", "application/pdf", []byte{}, admission.AdmissionPolicy{}, ""},
		{"nil body reject", "bill.pdf", "application/pdf", nil, admission.AdmissionPolicy{}, ""},
		{"declared PDF with ELF bytes reject", "bill.pdf", "application/pdf", elfBody(), admission.AdmissionPolicy{}, ""},
		{"declared JPEG with HTML bytes reject", "photo.jpg", "image/jpeg", htmlBody(), admission.AdmissionPolicy{}, ""},
		{"unsupported executable MIME reject", "run.bin", "application/x-executable", exeBody(), admission.AdmissionPolicy{}, ""},
		{"unsupported HTML MIME reject", "page.html", "text/html", htmlBody(), admission.AdmissionPolicy{}, ""},
		{"octet-stream MIME reject", "blob.bin", "application/octet-stream", elfBody(), admission.AdmissionPolicy{}, ""},
		{"traversal reject", "../../foo.pdf", "application/pdf", pdfBody(), admission.AdmissionPolicy{}, ""},
		{"backslash traversal reject", `..\foo.pdf`, "application/pdf", pdfBody(), admission.AdmissionPolicy{}, ""},
		{"null byte filename reject", "a\x00b.pdf", "application/pdf", pdfBody(), admission.AdmissionPolicy{}, ""},
		{"300-char filename reject", longName, "application/pdf", pdfBody(), admission.AdmissionPolicy{}, ""},
		{"unicode filename reject", "rëport.pdf", "application/pdf", pdfBody(), admission.AdmissionPolicy{}, ""},
		{"space filename reject", "my report.pdf", "application/pdf", pdfBody(), admission.AdmissionPolicy{}, ""},
		{"dot filename reject", ".", "application/pdf", pdfBody(), admission.AdmissionPolicy{}, ""},
		{"dotdot filename reject", "..", "application/pdf", pdfBody(), admission.AdmissionPolicy{}, ""},
		{"blank filename reject", "   ", "application/pdf", pdfBody(), admission.AdmissionPolicy{}, ""},
		{"valid extension wrong bytes reject", "report.pdf", "application/pdf", pngBody(), admission.AdmissionPolicy{}, ""},
		{"declared JPEG with PNG bytes reject", "photo.jpg", "image/jpeg", pngBody(), admission.AdmissionPolicy{}, ""},
		{"TIFF declared without tiff extension reject", "scan.bin", "image/tiff", tiffBody(), admission.AdmissionPolicy{}, ""},
		{"blank MIME reject", "bill.pdf", "", pdfBody(), admission.AdmissionPolicy{}, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			policy := tc.policy
			if policy.AllowedTypes == nil {
				policy = admission.DefaultPolicy()
			}
			got, err := admission.Validate(tc.filename, tc.declared, tc.body, policy)
			if tc.wantMedia == "" {
				if err == nil {
					t.Fatalf("expected rejection, got accept %+v", got)
				}
				if !errors.Is(err, admission.ErrAdmission) {
					t.Fatalf("error %v does not wrap ErrAdmission", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected accept, got error %v", err)
			}
			if got.Filename != strings.TrimSpace(tc.filename) {
				t.Fatalf("filename = %q, want %q", got.Filename, strings.TrimSpace(tc.filename))
			}
			if got.MediaType != tc.wantMedia {
				t.Fatalf("media type = %q, want %q", got.MediaType, tc.wantMedia)
			}
			sum := sha256.Sum256(tc.body)
			if want := hex.EncodeToString(sum[:]); got.SHA256 != want {
				t.Fatalf("sha256 = %q, want %q", got.SHA256, want)
			}
			if got.SizeBytes != int64(len(tc.body)) {
				t.Fatalf("size = %d, want %d", got.SizeBytes, len(tc.body))
			}
		})
	}
}

// TestValidateExactMaxAccept pins the size boundary: a body of exactly
// MaxBytes passes, one byte more does not.
func TestValidateExactMaxAccept(t *testing.T) {
	body := pdfBody()
	policy := admission.AdmissionPolicy{
		MaxBytes:         int64(len(body)),
		AllowedTypes:     admission.DefaultPolicy().AllowedTypes,
		MaxFilenameBytes: 255,
	}
	if _, err := admission.Validate("bill.pdf", "application/pdf", body, policy); err != nil {
		t.Fatalf("exact-max body must be accepted: %v", err)
	}
	policy.MaxBytes = int64(len(body)) - 1
	if _, err := admission.Validate("bill.pdf", "application/pdf", body, policy); !errors.Is(err, admission.ErrAdmission) {
		t.Fatalf("over-max body must be rejected, got %v", err)
	}
}

// TestValidateFileName pins the file-name rules directly.
func TestValidateFileName(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		max     int
		wantErr bool
	}{
		{"simple", "bill.pdf", 255, false},
		{"dashes underscores", "scan-2024_01.final.png", 255, false},
		{"exact max", strings.Repeat("a", 251) + ".pdf", 255, false}, // 255 bytes
		{"one over max", strings.Repeat("a", 252) + ".pdf", 255, true},
		{"blank", "", 255, true},
		{"spaces only", "   ", 255, true},
		{"dot", ".", 255, true},
		{"dotdot", "..", 255, true},
		{"slash traversal", "../../foo.pdf", 255, true},
		{"nested slash", "a/b.pdf", 255, true},
		{"backslash", `a\b.pdf`, 255, true},
		{"null byte", "a\x00.pdf", 255, true},
		{"space", "a b.pdf", 255, true},
		{"control char", "a\x01b.pdf", 255, true},
		{"unicode", "rëport.pdf", 255, true},
		{"dots inside", "a..b.pdf", 255, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := admission.ValidateFileName(tc.input, tc.max)
			if tc.wantErr && !errors.Is(err, admission.ErrAdmission) {
				t.Fatalf("expected ErrAdmission rejection, got %v", err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected accept, got %v", err)
			}
		})
	}
}

// TestCheckSize pins the size rules directly.
func TestCheckSize(t *testing.T) {
	if err := admission.CheckSize([]byte("hello"), 10); err != nil {
		t.Fatalf("within-limit body must pass: %v", err)
	}
	if err := admission.CheckSize([]byte("0123456789"), 10); err != nil {
		t.Fatalf("exact-limit body must pass: %v", err)
	}
	if err := admission.CheckSize([]byte("0123456789!"), 10); !errors.Is(err, admission.ErrAdmission) {
		t.Fatalf("over-limit body must be rejected, got %v", err)
	}
	if err := admission.CheckSize(nil, 10); !errors.Is(err, admission.ErrAdmission) {
		t.Fatalf("nil body must be rejected, got %v", err)
	}
	if err := admission.CheckSize([]byte{}, 10); !errors.Is(err, admission.ErrAdmission) {
		t.Fatalf("empty body must be rejected, got %v", err)
	}
}

// TestSniffAndMatch pins strict sniff equality: both sides allowlisted
// and equal, with no equivalences.
func TestSniffAndMatch(t *testing.T) {
	allowed := admission.DefaultPolicy().AllowedTypes
	cases := []struct {
		name      string
		declared  string
		body      []byte
		want      string
		wantError bool
	}{
		{"pdf match", "application/pdf", pdfBody(), "application/pdf", false},
		{"jpeg match", "image/jpeg", jpegBody(), "image/jpeg", false},
		{"png match", "image/png", pngBody(), "image/png", false},
		{"declared pdf sniffed png", "application/pdf", pngBody(), "", true},
		{"declared pdf sniffed elf", "application/pdf", elfBody(), "", true},
		{"declared jpeg sniffed html", "image/jpeg", htmlBody(), "", true},
		{"declared not allowlisted", "application/x-executable", exeBody(), "", true},
		{"declared html not allowlisted", "text/html", htmlBody(), "", true},
		{"empty body", "application/pdf", nil, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := admission.SniffAndMatch(tc.declared, tc.body, allowed)
			if tc.wantError {
				if !errors.Is(err, admission.ErrAdmission) {
					t.Fatalf("expected ErrAdmission rejection, got %v / %q", err, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected match, got %v", err)
			}
			if got != tc.want {
				t.Fatalf("detected = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDefaultPolicy pins the production bounds so allowlist or cap drift
// fails loudly.
func TestDefaultPolicy(t *testing.T) {
	p := admission.DefaultPolicy()
	if p.MaxBytes != 10<<20 {
		t.Fatalf("MaxBytes = %d, want %d", p.MaxBytes, 10<<20)
	}
	if p.MaxFilenameBytes != 255 {
		t.Fatalf("MaxFilenameBytes = %d, want 255", p.MaxFilenameBytes)
	}
	for _, want := range []string{"application/pdf", "image/jpeg", "image/png", "image/tiff"} {
		if _, ok := p.AllowedTypes[want]; !ok {
			t.Fatalf("AllowedTypes missing %q", want)
		}
	}
	if len(p.AllowedTypes) != 4 {
		t.Fatalf("AllowedTypes has %d entries, want exactly 4", len(p.AllowedTypes))
	}
}
