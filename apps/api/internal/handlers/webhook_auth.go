// Package handlers — HITL webhook authentication (APA-9).
//
// The HITL decision endpoint is an authenticated webhook ingress: every
// request must carry an HMAC-SHA256 signature in the X-Signature header
// (lowercase hex) covering the full logical request:
//
//	method + "\n" + path + "\n" + tenantID + "\n" + rawBody
//
// Authenticated: HTTP method, request path (including the claim ID),
// X-Tenant-ID, and the exact raw body bytes. Anything outside that
// material (e.g. other headers) is not bound. Verification is mandatory
// and fail-closed in every environment — no APP_ENV bypass, no "none"
// mode, no always-succeed provider.
//
// Tenant identity is derived from the X-Tenant-ID header only, never
// from a hardcoded "default" and never from the body — and because the
// tenant is part of the signed material, a caller cannot take a valid
// signature for tenant A and replay it as tenant B. Cross-tenant access
// is additionally rejected by the RLS-scoped lookup plus the tid check
// in DecisionHandler. No secret or raw payload is ever logged.
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

// signedMaterial renders the exact bytes covered by the webhook MAC:
// upper-cased method, request path, tenant, then the raw body, each of
// the first three followed by "\n". The tenant is bound byte-for-byte:
// no trimming happens here, so a padded tenant never silently matches
// its trimmed form. Canonicalization (trim + reject padded) happens once
// at the HTTP boundary in DecisionHandler, which passes the single
// canonical value to both verification and RLS.
func signedMaterial(method, path, tenantID string, body []byte) []byte {
	var b []byte
	b = append(b, []byte(strings.ToUpper(strings.TrimSpace(method)))...)
	b = append(b, '\n')
	b = append(b, []byte(path)...)
	b = append(b, '\n')
	b = append(b, []byte(tenantID)...)
	b = append(b, '\n')
	b = append(b, body...)
	return b
}

// verifyWebhookRequest reports whether sigHex is the lowercase-hex
// HMAC-SHA256 of the signed material under secret. Empty secret, empty
// tenant, empty signature, or malformed hex all fail closed (false).
// Comparison is constant-time.
func verifyWebhookRequest(secret, method, path, tenantID string, body []byte, sigHex string) bool {
	if secret == "" {
		return false
	}
	if tenantID == "" {
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
	mac.Write(signedMaterial(method, path, tenantID, body))
	want := mac.Sum(nil)
	if len(got) != len(want) {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

// SignWebhookRequest renders the lowercase-hex HMAC-SHA256 of the signed
// material under secret. Test and workflow-signer helper only —
// production never signs.
func SignWebhookRequest(secret, method, path, tenantID string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(signedMaterial(method, path, tenantID, body))
	return hex.EncodeToString(mac.Sum(nil))
}
