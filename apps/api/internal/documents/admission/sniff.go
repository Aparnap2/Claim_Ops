package admission

import (
	"fmt"
	"net/http"
)

// SniffAndMatch enforces the admission content rules with strict equality
// and no equivalences: the declared type must be in the allowed set, the
// sniffed type (net/http.DetectContentType) must be in the allowed set,
// and the two must be equal. Empty bodies are rejected.
//
// TIFF callers are the exception: DetectContentType reports TIFF bytes as
// "application/octet-stream", so TIFF is accepted on declared-type plus
// file-extension basis by Validate, which bypasses this function. Direct
// callers of SniffAndMatch get strict sniff matching only.
func SniffAndMatch(declared string, body []byte, allowed map[string]struct{}) (string, error) {
	if len(body) == 0 {
		return "", fmt.Errorf("%w: empty document content", ErrAdmission)
	}
	if _, ok := allowed[declared]; !ok {
		return "", fmt.Errorf("%w: unsupported media type %q", ErrAdmission, declared)
	}
	detected := http.DetectContentType(body)
	if _, ok := allowed[detected]; !ok {
		return "", fmt.Errorf("%w: sniffed media type %q is not allowed (declared %q)", ErrAdmission, detected, declared)
	}
	if detected != declared {
		return "", fmt.Errorf("%w: declared media type %q does not match sniffed %q", ErrAdmission, declared, detected)
	}
	return detected, nil
}
