package webauth

import (
	"strings"
	"testing"
)

func TestMintExpireAuth_Deterministic(t *testing.T) {
	b1, s1, err := MintExpireAuth("sec", "tnt-1", "clm-1", "inv-00000000000000000000000000000001")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	b2, s2, err := MintExpireAuth("sec", "tnt-1", "clm-1", "inv-00000000000000000000000000000001")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if b1 != b2 || s1 != s2 {
		t.Fatal("mint must be deterministic for identical inputs")
	}
	if !strings.Contains(b1, `"action":"EXPIRE"`) {
		t.Fatalf("body = %s, want EXPIRE action", b1)
	}
	if !VerifyWebhookRequest("sec", "POST", "/v1/claims/clm-1/decision", "tnt-1", []byte(b1), s1) {
		t.Fatal("minted pair must verify (self-consistency)")
	}
}

func TestMintExpireAuth_FailClosed(t *testing.T) {
	for name, tc := range map[string][4]string{
		"empty secret": {"", "t", "c", "inv-00000000000000000000000000000001"},
		"empty tenant": {"s", "", "c", "inv-00000000000000000000000000000001"},
		"empty claim":  {"s", "t", "", "inv-00000000000000000000000000000001"},
		"empty inv":    {"s", "t", "c", ""},
	} {
		if _, _, err := MintExpireAuth(tc[0], tc[1], tc[2], tc[3]); err == nil {
			t.Fatalf("%s: want error, got success", name)
		}
	}
}
