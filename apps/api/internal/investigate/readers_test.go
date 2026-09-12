// Reader seam tests (issue #54): pure helper units plus a live RLS round
// gated on TEST_POSTGRES_DSN.
//
// Pure: truncateSnippet (rune-aware, byte-safe), escapeLike (LIKE
// metacharacters), checkEvidenceSource (closed source-type set). Live:
// tenant-scoped reads against a seeded claim — own-tenant header, empty
// listings, unknown cursor fail-closed, and cross-tenant invisibility
// (RLS hides the row: ErrNotFound; without RLS the row-tenant guard still
// refuses loudly with ErrTenantMismatch — either way no cross-tenant read
// succeeds). Skips when TEST_POSTGRES_DSN is unset or PG is unreachable.
package investigate

import (
	"context"
	"errors"
	"strings"
	"testing"

	"claimops-api/internal/invest"
)

func TestTruncateSnippet(t *testing.T) {
	if got := truncateSnippet("short"); got != "short" {
		t.Errorf("short = %q, want unchanged", got)
	}
	exact := strings.Repeat("a", MaxSnippetRunes)
	if got := truncateSnippet(exact); got != exact {
		t.Errorf("exact-cap changed: len %d", len([]rune(got)))
	}
	long := strings.Repeat("b", MaxSnippetRunes+1)
	if got := truncateSnippet(long); len([]rune(got)) != MaxSnippetRunes {
		t.Errorf("long cut to %d runes, want %d", len([]rune(got)), MaxSnippetRunes)
	}
	// Multibyte: rune-aware cut, still valid UTF-8, no marker appended.
	wide := strings.Repeat("é", MaxSnippetRunes+50)
	got := truncateSnippet(wide)
	if len([]rune(got)) != MaxSnippetRunes {
		t.Errorf("wide cut to %d runes, want %d", len([]rune(got)), MaxSnippetRunes)
	}
	if !strings.HasPrefix(wide, got) {
		t.Error("truncation is not a prefix cut")
	}
	for _, r := range got {
		if r == '�' {
			t.Fatal("truncation split a multi-byte rune")
		}
	}
}

func TestEscapeLike(t *testing.T) {
	if got := escapeLike("plain"); got != "plain" {
		t.Errorf("plain = %q, want unchanged", got)
	}
	if got := escapeLike(`100%_x\y`); got != `100\%\_x\\y` {
		t.Errorf("escaped = %q, want %q", got, `100\%\_x\\y`)
	}
}

func TestCheckEvidenceSource(t *testing.T) {
	if err := checkEvidenceSource(""); err != nil {
		t.Errorf("empty filter: %v, want nil (all sources)", err)
	}
	for _, s := range []invest.EvidenceSourceType{
		invest.EvidenceSourceDocument, invest.EvidenceSourceField,
		invest.EvidenceSourcePolicy, invest.EvidenceSourceTPA,
		invest.EvidenceSourceProvider, invest.EvidenceSourceRisk,
	} {
		if err := checkEvidenceSource(string(s)); err != nil {
			t.Errorf("source %q: %v, want nil", string(s), err)
		}
	}
	if err := checkEvidenceSource("sql"); err == nil {
		t.Error("unknown source accepted, want rejection")
	} else if !errors.Is(err, ErrContract) {
		t.Errorf("unknown source err = %v, want ErrContract", err)
	}
}

func TestReadersLiveRLS(t *testing.T) {
	pool := requireInvestigatePool(t)
	tenant, claim := nextInvestigateClaim("readers")
	seedInvestigateClaim(t, pool, tenant, claim)
	r := NewPGReaders(pool)
	ctx := context.Background()

	t.Run("own tenant reads header", func(t *testing.T) {
		hdr, err := r.LoadClaim(ctx, tenant, claim)
		if err != nil {
			t.Fatalf("LoadClaim: %v", err)
		}
		if hdr.ClaimID != claim {
			t.Errorf("ClaimID = %q, want %q", hdr.ClaimID, claim)
		}
	})

	t.Run("cross tenant reads nothing", func(t *testing.T) {
		_, err := r.LoadClaim(ctx, tenant+"-other", claim)
		if err == nil {
			t.Fatal("cross-tenant LoadClaim succeeded, want rejection")
		}
		// RLS hides the row (ErrNotFound); without RLS the row-tenant
		// guard still refuses loudly (ErrTenantMismatch). Both prove no
		// cross-tenant read succeeds.
		if !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrTenantMismatch) {
			t.Fatalf("cross-tenant err = %v, want ErrNotFound or ErrTenantMismatch", err)
		}
	})

	t.Run("empty listings", func(t *testing.T) {
		docs, err := r.ListDocuments(ctx, tenant, claim, 0, "")
		if err != nil {
			t.Fatalf("ListDocuments: %v", err)
		}
		if len(docs.Documents) != 0 || docs.Truncated || docs.NextCursor != "" {
			t.Errorf("documents page = %+v, want empty untruncated", docs)
		}
		ev, err := r.ListEvidence(ctx, tenant, claim, 0, "", "")
		if err != nil {
			t.Fatalf("ListEvidence: %v", err)
		}
		if len(ev.Rows) != 0 || ev.Truncated || ev.NextCursor != "" {
			t.Errorf("evidence page = %+v, want empty untruncated", ev)
		}
	})

	t.Run("unknown cursor fails closed", func(t *testing.T) {
		if _, err := r.ListDocuments(ctx, tenant, claim, 0, "doc-nope"); !errors.Is(err, ErrContract) {
			t.Fatalf("documents cursor err = %v, want ErrContract", err)
		}
		if _, err := r.ListEvidence(ctx, tenant, claim, 0, "ev-nope", ""); !errors.Is(err, ErrContract) {
			t.Fatalf("evidence cursor err = %v, want ErrContract", err)
		}
	})

	t.Run("absent claim is not found", func(t *testing.T) {
		if _, err := r.LoadClaim(ctx, tenant, "clm-absent-54-readers"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("absent claim err = %v, want ErrNotFound", err)
		}
	})
}
