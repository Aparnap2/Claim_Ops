package main

import (
	"testing"

	"claimops-api/internal/invest"
)

// APA-11 — pins the actual production agent registry (buildAgentRegistry)
// rather than a duplicated test-only allowlist. If production ever wires
// an authoritative writer like create_investigation_report (T11), this
// test turns red before the mutation boundary is violated.

func TestAgentRegistry_IsReadOnly(t *testing.T) {
	registry := buildAgentRegistry(nil)
	if len(registry) != 5 {
		t.Fatalf("registry size = %d, want 5 read-only tools", len(registry))
	}
	// No authoritative writer may be wired. The sole writer today is T11;
	// any future writer must be added to this deny-list.
	for _, w := range []invest.ToolName{invest.ToolCreateInvestigationReport} {
		if _, ok := registry[w]; ok {
			t.Fatalf("agent registry must not contain authoritative writer %q", string(w))
		}
	}
	for tool := range registry {
		if !invest.IsAllowlisted(tool) {
			t.Fatalf("agent tool %q is not allowlisted", string(tool))
		}
	}
	want := map[invest.ToolName]struct{}{
		invest.ToolGetClaim:                {},
		invest.ToolGetDocuments:            {},
		invest.ToolGetEvidence:             {},
		invest.ToolSearchEvidence:          {},
		invest.ToolGetVerificationFindings: {},
	}
	for tool := range want {
		if _, ok := registry[tool]; !ok {
			t.Fatalf("agent registry missing required tool %q", string(tool))
		}
	}
	for tool := range registry {
		if _, ok := want[tool]; !ok {
			t.Fatalf("agent registry has unexpected tool %q", string(tool))
		}
	}
}
