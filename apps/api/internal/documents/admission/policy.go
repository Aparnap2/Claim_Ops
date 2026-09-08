// Package admission validates untrusted upload input BEFORE it crosses
// into ingestion. Only a ValidatedDocument crosses the boundary.
// Invariant: anything past admission has passed structural (filename,
// size, MIME allowlist) and content (byte-sniff match) validation.
//
// The package is stdlib-only by design: no I/O, no logging, no clock.
package admission

// AdmissionPolicy bounds what the admission gate accepts. MaxBytes caps
// the request body, AllowedTypes is the closed MIME allowlist matched
// against both the declared type and the sniffed content type, and
// MaxFilenameBytes caps the file name in bytes.
type AdmissionPolicy struct {
	MaxBytes         int64
	AllowedTypes     map[string]struct{}
	MaxFilenameBytes int
}

// DefaultPolicy returns the production admission bounds: 10MiB bodies,
// PDF/JPEG/PNG/TIFF only, and 255-byte file names.
func DefaultPolicy() AdmissionPolicy {
	return AdmissionPolicy{
		MaxBytes: 10 << 20,
		AllowedTypes: map[string]struct{}{
			"application/pdf": {},
			"image/jpeg":      {},
			"image/png":       {},
			"image/tiff":      {},
		},
		MaxFilenameBytes: 255,
	}
}
