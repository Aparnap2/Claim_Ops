package admission

import (
	"fmt"
	"strings"
)

// isFilenameChar reports whether r is in the file-name allowlist
// ^[a-zA-Z0-9._-]+$. Anything else — slashes, backslashes, null bytes,
// spaces, control characters, non-ASCII — is rejected.
func isFilenameChar(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z':
		return true
	case r >= 'A' && r <= 'Z':
		return true
	case r >= '0' && r <= '9':
		return true
	case r == '.' || r == '_' || r == '-':
		return true
	default:
		return false
	}
}

// ValidateFileName enforces the admission file-name rules on name:
// non-blank after trimming, byte length within maxBytes, characters
// from ^[a-zA-Z0-9._-]+$ only, no ".." path segment (either separator),
// and never "." or ".." exactly.
func ValidateFileName(name string, maxBytes int) error {
	t := strings.TrimSpace(name)
	if t == "" {
		return fmt.Errorf("%w: blank file name", ErrAdmission)
	}
	if len(t) > maxBytes {
		return fmt.Errorf("%w: file name too long (%d bytes, max %d)", ErrAdmission, len(t), maxBytes)
	}
	if t == "." || t == ".." {
		return fmt.Errorf("%w: file name %q is not allowed", ErrAdmission, t)
	}
	// Treat both separators as segment boundaries so ".." cannot hide
	// behind a backslash on any platform.
	for _, seg := range strings.Split(strings.ReplaceAll(t, "\\", "/"), "/") {
		if seg == ".." {
			return fmt.Errorf("%w: file name %q contains parent path segment", ErrAdmission, t)
		}
	}
	for _, r := range t {
		if !isFilenameChar(r) {
			return fmt.Errorf("%w: file name %q has disallowed character %q", ErrAdmission, t, r)
		}
	}
	return nil
}
