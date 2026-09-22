// Package handlers — HITL webhook authentication (APA-9).
//
// The HITL decision endpoint is an authenticated webhook ingress: every
// request must carry an HMAC-SHA256 signature over the exact raw body
// bytes in the X-Signature header (lowercase hex). Verification is
// mandatory and fail-closed in every environment — there is no
// APP_ENV bypass, no "none" mode, and no provider that always succeeds.
//
// Tenant identity is derived from the X-Tenant-ID header only, never
// from a hardcoded "default" and never from the body. Cross-tenant
// access is rejected by the RLS-scoped lookup plus the tid check in
// DecisionHandler. No secret or raw payload is ever logged: failures
// return stable codes only.
package handlers

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"strings"
)

// signatureHeader is the only carrier for the webhook signature.
const signatureHeader = "X-Signature"

// verifyWebhookSignature reports whether sigHex is the lowercase-hex
// HMAC-SHA256 of body under secret. Empty secret, empty signature, or
// malformed hex all fail closed (false). Comparison is constant-time.
func verifyWebhookSignature(secret string, body []byte, sigHex string) bool {
	if secret == "" {
		return false
	}
	sig := strings.TrimSpace(sigHex)
	if sig == "" {
		return false
	}
	got, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := mac.Sum(nil)
	if len(got) != len(want) {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

// signWebhookBody renders the lowercase-hex HMAC-SHA256 of body under
// secret. Test and workflow-signer helper only — production never signs.
func signWebhookBody(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}
