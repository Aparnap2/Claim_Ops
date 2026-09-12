package investigate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"claimops-api/internal/claims"
	"claimops-api/internal/repository/postgres"
)

// TestPinEvidenceIDDeterministic pins ID shape + determinism + key sensitivity.
func TestPinEvidenceIDDeterministic(t *testing.T) {
	a := PinEvidenceID("t1", "c1", "policy", "pol-1")
	b := PinEvidenceID("t1", "c1", "policy", "pol-1")
	if a != b {
		t.Fatal("same key must give same ID")
	}
	if !strings.HasPrefix(a, "ev-") || len(a) != 35 {
		t.Fatalf("ID = %q, want ev- + 32 hex", a)
	}
	for _, other := range []string{
		PinEvidenceID("t2", "c1", "policy", "pol-1"),
		PinEvidenceID("t1", "c2", "policy", "pol-1"),
		PinEvidenceID("t1", "c1", "tpa", "pol-1"),
		PinEvidenceID("t1", "c1", "policy", "pol-2"),
	} {
		if other == a {
			t.Fatalf("different key gave same ID %q", other)
		}
	}
}

// TestPinContentHashVector pins sha256 correctness incl. empty input.
func TestPinContentHashVector(t *testing.T) {
	sum := sha256.Sum256([]byte("abc"))
	want := hex.EncodeToString(sum[:])
	if got := PinContentHash([]byte("abc")); got != want {
		t.Fatalf("hash = %q, want %q", got, want)
	}
	empty := sha256.Sum256(nil)
	if got := PinContentHash(nil); got != hex.EncodeToString(empty[:]) {
		t.Fatal("empty bytes must hash deterministically")
	}
}

// TestCheckPinKeyTable pins fail-closed validation.
func TestCheckPinKeyTable(t *testing.T) {
	if err := checkPinKey("t1", "c1", "policy", "pol-1"); err != nil {
		t.Fatalf("valid key: %v", err)
	}
	for name, args := range map[string][4]string{
		"blank tenant": {"", "c1", "policy", "pol-1"},
		"blank claim":  {"t1", "", "policy", "pol-1"},
		"blank type":   {"t1", "c1", "", "pol-1"},
		"bad type":     {"t1", "c1", "nope", "pol-1"},
		"blank id":     {"t1", "c1", "policy", ""},
		"padded id":    {"t1", "c1", "policy", " pol-1"},
	} {
		if err := checkPinKey(args[0], args[1], args[2], args[3]); err == nil {
			t.Fatalf("%s: want error", name)
		}
	}
}

// TestPinNilPoolFailsClosed pins the zero-value guard.
func TestPinNilPoolFailsClosed(t *testing.T) {
	p := NewPGPinner(nil)
	if _, err := p.Pin(context.Background(), "t1", "c1", "policy", "pol-1", []byte("{}")); err == nil {
		t.Fatal("nil pool must fail closed")
	}
}

// TestPinFirstWinsLive pins first-wins semantics against live PG.
func TestPinFirstWinsLive(t *testing.T) {
	pool := requireInvestigatePool(t)
	tenant, claim := nextInvestigateClaim("pin")
	seedInvestigateClaim(t, pool, tenant, claim)
	p := NewPGPinner(pool)
	ctx := postgres.WithTenant(context.Background(), claims.TenantID(tenant))

	first := []byte(`{"v":1}`)
	second := []byte(`{"v":2}`)
	id1, err := p.Pin(ctx, tenant, claim, "policy", "pol-live-01", first)
	if err != nil {
		t.Fatalf("first pin: %v", err)
	}
	if want := PinEvidenceID(tenant, claim, "policy", "pol-live-01"); id1 != want {
		t.Fatalf("ID = %q, want derived %q", id1, want)
	}
	id2, err := p.Pin(ctx, tenant, claim, "policy", "pol-live-01", second)
	if err != nil {
		t.Fatalf("repin: %v", err)
	}
	if id2 != id1 {
		t.Fatal("repin must return the stored ID")
	}
	// Read back: stored hash equals FIRST bytes (first-wins).
	tx, err := postgres.BeginTenantTx(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	var stored string
	if err := tx.QueryRow(ctx, `SELECT content_hash FROM evidence WHERE id = $1`, id1).Scan(&stored); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	wantHash := PinContentHash(first)
	if stored != wantHash {
		t.Fatalf("stored hash = %q, want first-bytes hash %q", stored, wantHash)
	}
}
