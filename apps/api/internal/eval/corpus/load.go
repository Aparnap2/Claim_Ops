package corpus

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"claimops-api/internal/claims"
	"claimops-api/internal/documents/admission"
	"claimops-api/internal/parser"
)

// fixtureMediaType maps a fixture file extension onto exactly the admission
// allowlist set. Extensions outside the allowlist are rejected fail-closed
// rather than mislabeled. Fixtures are PDFs per the locked contract; the
// image branches exist only if the sibling agent emits allowlisted images.
func fixtureMediaType(fileName string) (string, error) {
	switch strings.ToLower(filepath.Ext(fileName)) {
	case ".pdf":
		return "application/pdf", nil
	case ".jpg", ".jpeg":
		return "image/jpeg", nil
	case ".png":
		return "image/png", nil
	case ".tif", ".tiff":
		return "image/tiff", nil
	default:
		return "", fmt.Errorf("corpus: fixture %q has extension outside the admission allowlist", fileName)
	}
}

// LoadTrustedDocument is the ONLY sanctioned path from fixture to parser
// input. It reads the case PDF, verifies sha256 against the manifest FIRST
// (mismatch returns before admission is ever consulted), then routes the
// bytes through admission.Validate with the default policy, and finally
// builds the parser input with the ADMITTED values (filename, media type,
// hash, size). Failures from admission keep their errors.Is(err,
// admission.ErrAdmission) identity through %w wrapping.
func LoadTrustedDocument(manifestCase Case, tenant, claimID string) (parser.TrustedDocument, error) {
	if strings.TrimSpace(manifestCase.ID) == "" {
		return parser.TrustedDocument{}, errors.New("corpus: blank case id")
	}
	if strings.TrimSpace(manifestCase.PDFPath) == "" {
		return parser.TrustedDocument{}, fmt.Errorf("corpus: case %s: blank PDF path", manifestCase.ID)
	}
	content, err := os.ReadFile(manifestCase.PDFPath)
	if err != nil {
		return parser.TrustedDocument{}, fmt.Errorf("corpus: case %s: read fixture: %w", manifestCase.ID, err)
	}
	return loadTrustedDocumentBytes(manifestCase.ID, filepath.Base(manifestCase.PDFPath), content, manifestCase.SHA256, tenant, claimID)
}

func loadTrustedDocumentBytes(caseID, fileName string, content []byte, expectedSHA256, tenant, claimID string) (parser.TrustedDocument, error) {
	expected := strings.ToLower(strings.TrimSpace(expectedSHA256))
	if expected == "" {
		return parser.TrustedDocument{}, fmt.Errorf("corpus: case %s: blank manifest sha256", caseID)
	}
	sum := sha256.Sum256(content)
	if actual := hex.EncodeToString(sum[:]); actual != expected {
		return parser.TrustedDocument{}, fmt.Errorf("corpus: case %s: sha256 mismatch (manifest %s != file %s): integrity failure, admission not consulted", caseID, expected, actual)
	}
	mediaType, err := fixtureMediaType(fileName)
	if err != nil {
		return parser.TrustedDocument{}, err
	}
	validated, err := admission.Validate(fileName, mediaType, content, admission.DefaultPolicy())
	if err != nil {
		return parser.TrustedDocument{}, fmt.Errorf("corpus: case %s: admission rejected fixture: %w", caseID, err)
	}
	// Defense in depth: the hash was already checked above, so this branch
	// is unreachable unless admission hashing ever diverges.
	if strings.ToLower(validated.SHA256) != expected {
		return parser.TrustedDocument{}, fmt.Errorf("corpus: case %s: admitted sha256 %s != manifest %s", caseID, validated.SHA256, expected)
	}
	doc, err := parser.NewTrustedDocument(caseID, claims.TenantID(tenant), claims.ClaimID(claimID), validated.Filename, validated.MediaType, validated.SHA256, validated.SizeBytes, content)
	if err != nil {
		return parser.TrustedDocument{}, fmt.Errorf("corpus: case %s: build trusted document: %w", caseID, err)
	}
	return doc, nil
}
