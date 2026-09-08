package admission

import "fmt"

// CheckSize rejects empty bodies and bodies larger than maxBytes.
func CheckSize(body []byte, maxBytes int64) error {
	if len(body) == 0 {
		return fmt.Errorf("%w: empty document content", ErrAdmission)
	}
	if int64(len(body)) > maxBytes {
		return fmt.Errorf("%w: document too large (%d bytes, max %d)", ErrAdmission, len(body), maxBytes)
	}
	return nil
}
