package admission

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// ErrAdmission is the sentinel wrapped by every admission-gate rejection.
// Callers test membership with errors.Is(err, ErrAdmission).
var ErrAdmission = errors.New("admission: rejected")

// ValidatedDocument is the only value allowed past the admission
// boundary. Anything holding one has passed structural and content
// validation: safe file name, allowed size, allowlisted MIME, and
// sniffed bytes matching the declared type.
type ValidatedDocument struct {
	Filename  string
	MediaType string
	SHA256    string
	SizeBytes int64
}

// Validate orchestrates the admission gate in fixed order: file-name
// check, size check, declared-MIME allowlist, content sniff match, then
// SHA256 computation. The returned ValidatedDocument carries the trimmed
// file name, the matched media type, and the content hash. Every failure
// wraps ErrAdmission.
func Validate(filename, declaredContentType string, body []byte, policy AdmissionPolicy) (ValidatedDocument, error) {
	if err := ValidateFileName(filename, policy.MaxFilenameBytes); err != nil {
		return ValidatedDocument{}, err
	}
	if err := CheckSize(body, policy.MaxBytes); err != nil {
		return ValidatedDocument{}, err
	}
	name := strings.TrimSpace(filename)
	declared := strings.TrimSpace(declaredContentType)
	if _, ok := policy.AllowedTypes[declared]; !ok {
		return ValidatedDocument{}, fmt.Errorf("%w: unsupported media type %q", ErrAdmission, declaredContentType)
	}
	mediaType := declared
	// TIFF bypass: DetectContentType reports TIFF bytes as
	// "application/octet-stream", so a declared image/tiff with a .tif /
	// .tiff file name is accepted on declared-type plus extension basis
	// without a sniff match. Declared TIFF without such an extension, or
	// any other type, still goes through strict sniff matching.
	if !(declared == "image/tiff" && hasTiffExtension(name)) {
		detected, err := SniffAndMatch(declared, body, policy.AllowedTypes)
		if err != nil {
			return ValidatedDocument{}, err
		}
		mediaType = detected
	}
	sum := sha256.Sum256(body)
	return ValidatedDocument{
		Filename:  name,
		MediaType: mediaType,
		SHA256:    hex.EncodeToString(sum[:]),
		SizeBytes: int64(len(body)),
	}, nil
}

// hasTiffExtension reports whether name ends in .tif or .tiff
// (case-insensitive).
func hasTiffExtension(name string) bool {
	lower := strings.ToLower(name)
	return strings.HasSuffix(lower, ".tif") || strings.HasSuffix(lower, ".tiff")
}
