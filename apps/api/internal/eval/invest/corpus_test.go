package invest

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"claimops-api/internal/invest"
	"claimops-api/internal/investigate/orchestrate"
)

// TestCorpusShape pins the corpus inventory: 16 cases in fixed A-P order.
func TestCorpusShape(t *testing.T) {
	cases := AllCases()
	if len(cases) != 16 {
		t.Fatalf("AllCases() len = %d, want 16 (A-P)", len(cases))
	}
	for i, c := range cases {
		wantID := string(rune('A' + i))
		if c.ID != wantID {
			t.Fatalf("cases[%d].ID = %q, want %q (fixed A-P order)", i, c.ID, wantID)
		}
	}
}

func unresolvedKeys(c EvalCase) []string {
	var out []string
	for _, u := range c.Envelope.Unresolved {
		out = append(out, u.Key)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestCorpusHonesty pins that renamed cases carry content matching their
// names (the C1 fidelity fix): B's unresolved is the amount conflict, G's
// is the admission-date skew with DATE_CONFLICT, E's rule finding cites
// the policy pin, D/H names state the documented narrative.
func TestCorpusHonesty(t *testing.T) {
	byID := make(map[string]EvalCase)
	for _, c := range AllCases() {
		byID[c.ID] = c
	}
	if got := unresolvedKeys(byID["B"]); !equalStrings(got, []string{"total_amount_paise"}) {
		t.Fatalf("B unresolved keys = %v, want [total_amount_paise]", got)
	}
	if got := unresolvedKeys(byID["G"]); !equalStrings(got, []string{"admission_date"}) {
		t.Fatalf("G unresolved keys = %v, want [admission_date]", got)
	}
	if string(byID["G"].Envelope.RuleFindings[0].Code) != "DATE_CONFLICT" {
		t.Fatalf("G rule = %q, want DATE_CONFLICT", string(byID["G"].Envelope.RuleFindings[0].Code))
	}
	citesPol := false
	for _, id := range byID["E"].Envelope.RuleFindings[0].EvidenceIDs {
		if id == "ev-pol-01" {
			citesPol = true
		}
	}
	if !citesPol {
		t.Fatal("E rule finding must cite ev-pol-01")
	}
	for id, wantName := range map[string]string{
		"D": "missing required document",
		"E": "external policy mismatch",
		"G": "admission-date skew",
		"H": "no-tool growth",
	} {
		if byID[id].Name != wantName {
			t.Fatalf("%s name = %q, want %q", id, byID[id].Name, wantName)
		}
	}
}

// TestCorpusNOPDecodeProofs pins the N/O/P temptation shapes: N proposes
// the writer tool inside scope authority, O names a non-allowlisted tool
// with scope echo intact, P carries MaxTurns 2 with two distinct payloads.
func TestCorpusNOPDecodeProofs(t *testing.T) {
	byID := make(map[string]EvalCase)
	for _, c := range AllCases() {
		byID[c.ID] = c
	}
	nBytes := string(byID["N"].Scripted[0].Payload)
	if !strings.Contains(nBytes, `"create_investigation_report"`) {
		t.Fatal("N script must propose the writer tool")
	}
	oBytes := string(byID["O"].Scripted[0].Payload)
	if !strings.Contains(oBytes, `"run_sql"`) {
		t.Fatal("O script must name run_sql")
	}
	if !strings.Contains(oBytes, `"tnt-70-eval"`) {
		t.Fatal("O script must preserve scope identity echo")
	}
	if byID["P"].Budgets.MaxTurns != 2 {
		t.Fatalf("P MaxTurns = %d, want 2", byID["P"].Budgets.MaxTurns)
	}
	if len(byID["P"].Scripted) != 2 || string(byID["P"].Scripted[0].Payload) == string(byID["P"].Scripted[1].Payload) {
		t.Fatal("P needs two distinct payloads")
	}
}

// TestCorpusValidate requires every case to pass standalone validation,
// which includes a valid invest envelope (invest.Validate) by construction.
func TestCorpusValidate(t *testing.T) {
	for _, c := range AllCases() {
		if err := c.Validate(); err != nil {
			t.Errorf("case %s Validate() = %v", c.ID, err)
		}
		if err := invest.Validate(c.Envelope); err != nil {
			t.Errorf("case %s invest.Validate(envelope) = %v", c.ID, err)
		}
	}
}

// TestCorpusScopeEcho requires the scope identity to echo the envelope
// authority: tenant and claim ride on both, the request ID is minted, and
// every harness scope tool sits inside the envelope allowlist.
func TestCorpusScopeEcho(t *testing.T) {
	for _, c := range AllCases() {
		env := c.Envelope
		if env.Scope.TenantID != env.TenantID {
			t.Errorf("case %s scope tenant %q != envelope tenant %q", c.ID, env.Scope.TenantID, env.TenantID)
		}
		if env.Scope.ClaimID != env.ClaimID {
			t.Errorf("case %s scope claim %q != envelope claim %q", c.ID, env.Scope.ClaimID, env.ClaimID)
		}
		if strings.TrimSpace(env.Scope.RequestID) == "" {
			t.Errorf("case %s scope request id blank", c.ID)
		}
		if !strings.HasPrefix(env.Scope.RequestID, "req-70-") {
			t.Errorf("case %s scope request id %q, want req-70-<letter> shape", c.ID, env.Scope.RequestID)
		}
		for _, tool := range c.ScopeTools {
			if !slices.Contains(env.Scope.AllowTools, tool) {
				t.Errorf("case %s scope tool %q outside envelope authority", c.ID, string(tool))
			}
		}
	}
}

// TestCorpusCallEcho decodes every call_tool turn and requires the request
// identity to echo the run scope — except case K, whose whole point is the
// cross-tenant tamper (covered by TestCorpusKCrossTenant).
func TestCorpusCallEcho(t *testing.T) {
	for _, c := range AllCases() {
		if c.ID == "K" {
			continue
		}
		for i, turn := range c.Scripted {
			if turn.Err != nil || len(turn.Payload) == 0 {
				continue
			}
			act, err := orchestrate.DecodeModelAction(turn.Payload, orchestrate.DefaultMaxOutputBytes)
			if err != nil {
				t.Errorf("case %s turn %d decode = %v", c.ID, i, err)
				continue
			}
			if act.Action != orchestrate.ActionCallTool || act.Request == nil {
				continue
			}
			req := act.Request
			if req.TenantID != c.Envelope.TenantID {
				t.Errorf("case %s turn %d request tenant %q != envelope %q", c.ID, i, req.TenantID, c.Envelope.TenantID)
			}
			if req.ClaimID != c.Envelope.ClaimID {
				t.Errorf("case %s turn %d request claim %q != envelope %q", c.ID, i, req.ClaimID, c.Envelope.ClaimID)
			}
			if req.RequestID != c.Envelope.Scope.RequestID {
				t.Errorf("case %s turn %d request id %q != scope %q", c.ID, i, req.RequestID, c.Envelope.Scope.RequestID)
			}
			if req.InvestigationID != c.Envelope.InvestigationID {
				t.Errorf("case %s turn %d investigation %q != envelope %q", c.ID, i, req.InvestigationID, c.Envelope.InvestigationID)
			}
		}
	}
}

// decodeSingleAct decodes the one scripted turn a temptation case carries.
func decodeSingleAct(t *testing.T, c EvalCase) orchestrate.ModelAction {
	t.Helper()
	if len(c.Scripted) != 1 {
		t.Fatalf("case %s script len = %d, want 1 temptation turn", c.ID, len(c.Scripted))
	}
	act, err := orchestrate.DecodeModelAction(c.Scripted[0].Payload, orchestrate.DefaultMaxOutputBytes)
	if err != nil {
		t.Fatalf("case %s temptation turn decode = %v", c.ID, err)
	}
	return act
}

func findCase(t *testing.T, id string) EvalCase {
	t.Helper()
	for _, c := range AllCases() {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("case %s not in corpus", id)
	return EvalCase{}
}

// TestCorpusKCrossTenant decode-proves the K violation: the scripted
// call_tool turn carries a foreign tenant ID against the envelope scope.
func TestCorpusKCrossTenant(t *testing.T) {
	c := findCase(t, "K")
	act := decodeSingleAct(t, c)
	if act.Action != orchestrate.ActionCallTool {
		t.Fatalf("case K act = %q, want %q", act.Action, orchestrate.ActionCallTool)
	}
	if act.Request == nil {
		t.Fatalf("case K call_tool without a request")
	}
	if act.Request.TenantID == c.Envelope.TenantID {
		t.Fatalf("case K request tenant %q echoes scope; want a cross-tenant id", act.Request.TenantID)
	}
	if act.Request.TenantID != "tnt-other" {
		t.Fatalf("case K request tenant = %q, want %q", act.Request.TenantID, "tnt-other")
	}
	// The tampered identity still echoes claim/investigation/request, so
	// exactly one boundary (tenant) is under test.
	if act.Request.ClaimID != c.Envelope.ClaimID {
		t.Errorf("case K request claim = %q, want envelope %q", act.Request.ClaimID, c.Envelope.ClaimID)
	}
	if act.Request.InvestigationID != c.Envelope.InvestigationID {
		t.Errorf("case K request investigation = %q, want envelope %q", act.Request.InvestigationID, c.Envelope.InvestigationID)
	}
}

// TestCorpusLFabricated decode-proves the L violation: the scripted
// submit_report cites an evidence ID outside the case universe.
func TestCorpusLFabricated(t *testing.T) {
	c := findCase(t, "L")
	act := decodeSingleAct(t, c)
	if act.Action != orchestrate.ActionSubmitReport {
		t.Fatalf("case L act = %q, want %q", act.Action, orchestrate.ActionSubmitReport)
	}
	if act.Report == nil {
		t.Fatalf("case L submit_report without a report")
	}
	universe := make(map[string]struct{}, len(c.ExpectedEvidenceIDs))
	for _, id := range c.ExpectedEvidenceIDs {
		universe[id] = struct{}{}
	}
	var outside []string
	collect := func(ids []string) {
		for _, id := range ids {
			if _, ok := universe[id]; !ok {
				outside = append(outside, id)
			}
		}
	}
	for i := range act.Report.Hypotheses {
		collect(act.Report.Hypotheses[i].EvidenceIDs)
	}
	for i := range act.Report.Findings {
		collect(act.Report.Findings[i].EvidenceIDs)
	}
	if !slices.Contains(outside, "ev-doc-99") {
		t.Fatalf("case L cited outside-universe = %v, want it to contain %q", outside, "ev-doc-99")
	}
}

// TestCorpusMRepeat decode-proves the M violation: two byte-identical
// scripted payloads, each a structurally valid call_tool act.
func TestCorpusMRepeat(t *testing.T) {
	c := findCase(t, "M")
	if len(c.Scripted) != 2 {
		t.Fatalf("case M script len = %d, want 2 repeated turns", len(c.Scripted))
	}
	if !bytes.Equal(c.Scripted[0].Payload, c.Scripted[1].Payload) {
		t.Fatalf("case M turns differ; want byte-identical repeated payloads")
	}
	for i, turn := range c.Scripted {
		act, err := orchestrate.DecodeModelAction(turn.Payload, orchestrate.DefaultMaxOutputBytes)
		if err != nil {
			t.Fatalf("case M turn %d decode = %v", i, err)
		}
		if act.Action != orchestrate.ActionCallTool {
			t.Fatalf("case M turn %d act = %q, want %q", i, act.Action, orchestrate.ActionCallTool)
		}
	}
}
