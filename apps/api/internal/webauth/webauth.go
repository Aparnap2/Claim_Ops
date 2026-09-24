// Package webauth owns the HITL webhook authentication core (extracted
// from handlers, APA-9, so non-HTTP producers can mint signatures without
// importing the handler layer or duplicating the MAC construction).
//
// Every decision request carries an HMAC-SHA256 signature in the
// X-Signature header (lowercase hex) covering the full logical request:
//
//	method + "\n" + path + "\n" + tenantID + "\n" + rawBody
//
// Verification is mandatory and fail-closed in every environment.
// MintExpireAuth is the single approved producer of workflow timeout
// signatures: the worker mints the canonical EXPIRE body + signature at
// envelope time (it knows method, path, tenant, and exact body bytes);
// the workflow forwards both strings opaquely and never constructs auth
// material itself.
package webauth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strings"
)

// SignatureHeader is the only carrier for the webhook signature.
const SignatureHeader = "X-Signature"

// SignedMaterial renders the exact bytes covered by the webhook MAC:
// upper-cased method, request path, tenant, then the raw body, each of
// the first three followed by "\n". The tenant is bound byte-for-byte.
func SignedMaterial(method, path, tenantID string, body []byte) []byte {
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

// VerifyWebhookRequest reports whether sigHex is the lowercase-hex
// HMAC-SHA256 of the signed material under secret. Empty secret, empty
// tenant, empty signature, or malformed hex all fail closed (false).
// Comparison is constant-time.
func VerifyWebhookRequest(secret, method, path, tenantID string, body []byte, sigHex string) bool {
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
	mac.Write(SignedMaterial(method, path, tenantID, body))
	want := mac.Sum(nil)
	if len(got) != len(want) {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

// SignWebhookRequest renders the lowercase-hex HMAC-SHA256 of the signed
// material under secret. Test helper and approved producer primitive —
func SignWebhookRequest(secret, method, path, tenantID string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(SignedMaterial(method, path, tenantID, body))
	return hex.EncodeToString(mac.Sum(nil))
}

// ExpireBody renders the canonical timeout-decision body bytes.
// Field order is load-bearing (byte-exact for the MAC): keep in sync
// with the DecisionRequest JSON names (action, reason, actor,
// request_id, event_id). The workflow must POST these bytes verbatim (as
// an opaque string) or the signature will not verify. Tenant and claim
// travel in the MAC material (header + path), not the body.
func ExpireBody(investigationID string) []byte {
	return []byte(fmt.Sprintf(
		`{"action":"EXPIRE","reason":"hitl-timeout","actor":"system","request_id":"","event_id":"hitl-expiry:%s"}`,
		investigationID))
}

// MintExpireAuth mints the approved workflow timeout credential: the
// canonical EXPIRE body plus its HMAC for POST /v1/claims/<claim>/decision.
// Empty secret/tenant/claim/investigation fail closed. The caller (worker)
// knows method, path, tenant, and exact body bytes, so the workflow never
// constructs auth material — it forwards both strings opaquely.
func MintExpireAuth(secret, tenantID, claimID, investigationID string) (body, sig string, err error) {
	if strings.TrimSpace(secret) == "" {
		return "", "", fmt.Errorf("webauth: expire mint needs a secret")
	}
	if strings.TrimSpace(tenantID) == "" || strings.TrimSpace(claimID) == "" || strings.TrimSpace(investigationID) == "" {
		return "", "", fmt.Errorf("webauth: expire mint needs tenant, claim, investigation")
	}
	path := "/v1/claims/" + claimID + "/decision"
	b := ExpireBody(investigationID)
	return string(b), SignWebhookRequest(secret, "POST", path, tenantID, b), nil
}
