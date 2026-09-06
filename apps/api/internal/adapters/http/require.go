// Shared wire-validation helpers for the HTTP adapters.
package httpadapter

import (
	"encoding/json"
	"fmt"
	"strings"

	"claimops-api/internal/ports"
)

// requireKeys asserts a decoded JSON object carries each required field.
// Each group lists accepted spellings (snake_case wire keys and Go-style
// keys); the first present spelling satisfies the group. This detects
// truncated payloads that typed decoding would silently zero-fill.
func requireKeys(what string, raw map[string]json.RawMessage, groups ...[]string) error {
	for _, alts := range groups {
		found := false
		for _, k := range alts {
			if _, ok := raw[k]; ok {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("%w: %s: missing required field %q", ports.ErrContract, what, alts[0])
		}
	}
	return nil
}

// rawObject decodes body as a JSON object for key-presence checks.
func rawObject(what string, body []byte) (map[string]json.RawMessage, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("%w: %s: decode: %v", ports.ErrContract, what, err)
	}
	return raw, nil
}

// rawArrayItems decodes body as a JSON array of per-item raw objects for
// key-presence and per-item tenant checks.
func rawArrayItems(what string, body []byte) ([]map[string]json.RawMessage, error) {
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(body, &items); err != nil {
		return nil, fmt.Errorf("%w: %s: decode: %v", ports.ErrContract, what, err)
	}
	return items, nil
}

// itemTenant extracts a per-item tenant marker, accepting common
// spellings. Blank means absent.
func itemTenant(raw map[string]json.RawMessage) string {
	for _, k := range []string{"tenant_id", "tenantId", "tenantID", "tenant"} {
		if v, ok := raw[k]; ok {
			var s string
			if err := json.Unmarshal(v, &s); err == nil && strings.TrimSpace(s) != "" {
				return s
			}
		}
	}
	return ""
}

// checkItemTenant enforces per-item tenant consistency: blank markers are
// contract failures (items must carry tenancy), drift is a mismatch.
func checkItemTenant(what, expected string, raw map[string]json.RawMessage) error {
	got := itemTenant(raw)
	if got == "" {
		return fmt.Errorf("%w: %s: item missing tenant marker", ports.ErrContract, what)
	}
	if got != expected {
		return fmt.Errorf("%w: %s: upstream tenant %q != expected %q", ports.ErrTenantMismatch, what, got, expected)
	}
	return nil
}
