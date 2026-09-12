package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"claimops-api/internal/ports"
)

func validProviderRequest(encounterID string) ProviderRequest {
	return ProviderRequest{
		TenantID:          toolsTenant,
		ClaimID:           toolsClaim,
		InvestigationID:   toolsInv,
		RequestID:         toolsReq,
		EncounterID:       encounterID,
		AllowedEncounters: []string{encounterID},
	}
}

func TestProviderValidAttestsNothing(t *testing.T) {
	ctx := context.Background()
	enc := validEncounter("enc-01")
	port := &fakeProviderPort{encounter: enc}
	pin := &fakePinner{id: "ev-provider-01"}
	tool := NewProviderTool(port, pin)

	req := validProviderRequest("enc-01")
	got, err := tool(ctx, req)
	if err != nil {
		t.Fatalf("valid provider call: %v", err)
	}
	if got.TenantAttested {
		t.Fatalf("TenantAttested must be false: DTO is unattributed, tool attests nothing")
	}
	if got.EvidenceID != "ev-provider-01" {
		t.Fatalf("EvidenceID = %q, want %q", got.EvidenceID, "ev-provider-01")
	}
	if got.Truncated {
		t.Fatalf("Truncated must be false for short list")
	}
	if got.Encounter.EncounterID != "enc-01" {
		t.Fatalf("EncounterID = %q, want enc-01", got.Encounter.EncounterID)
	}
	if len(got.Encounter.Documents) != 1 {
		t.Fatalf("Documents len = %d, want 1", len(got.Encounter.Documents))
	}
	if got.Encounter.Documents[0].DocumentID != "doc-54-1" {
		t.Fatalf("Documents[0] = %q, want doc-54-1", got.Encounter.Documents[0].DocumentID)
	}
	if len(got.Encounter.Diagnosis) != 1 || got.Encounter.Diagnosis[0].Code != "A01" {
		t.Fatalf("diagnosis not verbatim: %+v", got.Encounter.Diagnosis)
	}
	if got.ContentHash == "" {
		t.Fatalf("ContentHash must be set")
	}
	// Pinner proves provenance: tenant/claim echo, source type/id.
	if pin.calls != 1 {
		t.Fatalf("pinner calls = %d, want 1", pin.calls)
	}
	if pin.gotTenant != toolsTenant || pin.gotClaim != toolsClaim {
		t.Fatalf("pinner tenant/claim = %q/%q, want %q/%q", pin.gotTenant, pin.gotClaim, toolsTenant, toolsClaim)
	}
	if pin.gotType != "provider" || pin.gotSource != "enc-01" {
		t.Fatalf("pinner type/source = %q/%q, want provider/enc-01", pin.gotType, pin.gotSource)
	}
	// Pinned bytes are the full pre-truncation payload: re-marshal must match.
	raw, err := json.Marshal(enc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(pin.gotBytes) != string(raw) {
		t.Fatalf("pinned bytes mismatch:\n got %s\nwant %s", pin.gotBytes, raw)
	}
	sum := sha256.Sum256(raw)
	if got.ContentHash != hex.EncodeToString(sum[:]) {
		t.Fatalf("ContentHash = %q, want sha256 of canonical", got.ContentHash)
	}
}

func TestProviderUnattributedSuccessNoTenantInDTO(t *testing.T) {
	ctx := context.Background()
	enc := validEncounter("enc-02")
	port := &fakeProviderPort{encounter: enc}
	pin := &fakePinner{id: "ev-provider-02"}
	tool := NewProviderTool(port, pin)

	req := validProviderRequest("enc-02")
	got, err := tool(ctx, req)
	if err != nil {
		t.Fatalf("unattributed success: %v", err)
	}
	// The DTO carries no tenant field: success can never attest tenancy.
	if got.TenantAttested {
		t.Fatalf("TenantAttested must be false for unattributed DTO")
	}
}

func TestProviderWrongTenantEnvelopeMismatch(t *testing.T) {
	ctx := context.Background()
	port := &fakeProviderPort{err: toolsErr(ports.ErrTenantMismatch, "envelope drift")}
	pin := &fakePinner{id: "ev-unused"}
	tool := NewProviderTool(port, pin)

	req := validProviderRequest("enc-01")
	_, err := tool(ctx, req)
	if err == nil {
		t.Fatalf("want TenantMismatch, got nil")
	}
	requireToolsSentinel(t, err, ports.ErrTenantMismatch)
}

func TestProviderUnlistedEncounterContract(t *testing.T) {
	ctx := context.Background()
	port := &fakeProviderPort{encounter: validEncounter("enc-01")}
	pin := &fakePinner{id: "ev-unused"}
	tool := NewProviderTool(port, pin)

	req := validProviderRequest("enc-01")
	req.AllowedEncounters = []string{"enc-other"}
	_, err := tool(ctx, req)
	if err == nil {
		t.Fatalf("want Contract for unlisted encounter, got nil")
	}
	requireToolsSentinel(t, err, ports.ErrContract)

	// Empty allowlist is also out of scope.
	req.AllowedEncounters = nil
	_, err = tool(ctx, req)
	if err == nil {
		t.Fatalf("want Contract for empty allowlist, got nil")
	}
	requireToolsSentinel(t, err, ports.ErrContract)
}

func TestProviderDocsCapTruncateFlag(t *testing.T) {
	ctx := context.Background()
	enc := validEncounter("enc-cap")
	for i := 0; i < MaxProviderDocs+5; i++ {
		enc.Documents = append(enc.Documents, ports.ProviderDoc{
			DocumentID: string(rune('a'+i%26)) + "-extra",
			Type:       "bill",
			SHA256:     "hh",
		})
	}
	total := len(enc.Documents)
	if total <= MaxProviderDocs {
		t.Fatalf("fixture must exceed cap: len=%d cap=%d", total, MaxProviderDocs)
	}
	port := &fakeProviderPort{encounter: enc}
	pin := &fakePinner{id: "ev-provider-cap"}
	tool := NewProviderTool(port, pin)

	got, err := tool(ctx, validProviderRequest("enc-cap"))
	if err != nil {
		t.Fatalf("cap call: %v", err)
	}
	if !got.Truncated {
		t.Fatalf("Truncated must be true when upstream exceeds cap")
	}
	if len(got.Encounter.Documents) != MaxProviderDocs {
		t.Fatalf("Documents len = %d, want cap %d", len(got.Encounter.Documents), MaxProviderDocs)
	}
	// Verbatim upstream order in the returned prefix.
	for i := 0; i < MaxProviderDocs; i++ {
		if got.Encounter.Documents[i].DocumentID != enc.Documents[i].DocumentID {
			t.Fatalf("order drift at %d: got %q want %q", i, got.Encounter.Documents[i].DocumentID, enc.Documents[i].DocumentID)
		}
	}
	// Pinned bytes remain the FULL pre-truncation payload.
	var pinned ports.Encounter
	if err := json.Unmarshal(pin.gotBytes, &pinned); err != nil {
		t.Fatalf("unmarshal pinned: %v", err)
	}
	if len(pinned.Documents) != total {
		t.Fatalf("pinned docs = %d, want full %d", len(pinned.Documents), total)
	}
}

func TestProviderDeterministic(t *testing.T) {
	ctx := context.Background()
	enc := validEncounter("enc-det")
	call := func() ProviderResponse {
		port := &fakeProviderPort{encounter: enc}
		pin := &fakePinner{id: "ev-provider-det"}
		tool := NewProviderTool(port, pin)
		got, err := tool(ctx, validProviderRequest("enc-det"))
		if err != nil {
			t.Fatalf("deterministic call: %v", err)
		}
		return got
	}
	a, b := call(), call()
	if a.ContentHash != b.ContentHash {
		t.Fatalf("ContentHash not deterministic:\n a %s\n b %s", a.ContentHash, b.ContentHash)
	}
	rawA, _ := json.Marshal(a.Encounter)
	rawB, _ := json.Marshal(b.Encounter)
	if string(rawA) != string(rawB) {
		t.Fatalf("encounter projection not deterministic")
	}
	if a.Truncated != b.Truncated || a.EvidenceID != b.EvidenceID || a.TenantAttested != b.TenantAttested {
		t.Fatalf("response flags not deterministic: %+v vs %+v", a, b)
	}
}
