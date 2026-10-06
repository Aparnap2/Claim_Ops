// APA-54: divergence guard between the model-facing knob contract in the
// prompt and the authoritative ownership mapping enforced by
// investigate.Request.Validate (tool.go:229-247, checkKnob at
// tool.go:269-280).
//
// The prompt is the only place the model learns which knob belongs to
// which tool. If the prose drifts from the enforcement table, the model
// is taught a contract the decoder will refuse — the exact failure class
// APA-49/50 fixed for the act discriminator and state policy. This file
// makes the prose and the enforcement table the same fact: every knob the
// prompt names must be owned by the same tool in the authoritative table,
// and vice versa.
//
// The authoritative table is derived BEHAVIOURALLY: for each tool and
// each conditional knob, a request carrying that knob is run through the
// real Request.Validate. A knob is owned by a tool iff Validate accepts
// it. No source parsing, no restated table — the test follows the
// enforcement, not a copy of it.
//
// The prompt's stated table is parsed from the rendered prompt in the
// strict line format the template uses:
//
//   - "<knob>": <optional|required>; owned by <tool>, <tool>. <meaning>
//
// so the prose cannot drift without this test failing.
package orchestrate

import (
	"regexp"
	"sort"
	"strings"
	"testing"

	"claimops-api/internal/invest"
	"claimops-api/internal/investigate"
)

// apa54Knobs is the closed set of conditional knobs the contract covers.
var apa54Knobs = []string{"cursor", "query", "subject_id", "source_type", "hash", "payload"}

// apa54Tools is the closed set of tools the contract covers.
var apa54Tools = []invest.ToolName{
	invest.ToolGetClaim, invest.ToolGetPolicyContext, invest.ToolGetDocuments,
	invest.ToolGetEvidence, invest.ToolSearchEvidence, invest.ToolGetVerificationFindings,
	invest.ToolGetExternalPolicyStatus, invest.ToolGetTPACase,
	invest.ToolGetProviderEncounter, invest.ToolGetRiskSignals,
	invest.ToolCreateInvestigationReport,
}

// apa54BaseRequest builds a minimal valid envelope for tool: identity
// echoed, limit within MaxRows. No conditional knobs set.
func apa54BaseRequest(tool invest.ToolName) investigate.Request {
	return investigate.Request{
		Tool:            tool,
		TenantID:        "tnt-54",
		ClaimID:         "clm-54",
		InvestigationID: "inv-0123456789abcdef0123456789abcdef",
		RequestID:       "req-54",
		Limit:           1,
	}
}

// apa54SetKnob returns a copy of req with the named knob set to a valid
// value, so Validate's ownership check is the only thing that can reject
// it.
func apa54SetKnob(req investigate.Request, knob string) investigate.Request {
	switch knob {
	case "cursor":
		req.Cursor = "page-2"
	case "query":
		req.Query = "policy number"
	case "subject_id":
		req.SubjectID = "sub-0123456789abcdef0123456789abcdef"
	case "source_type":
		req.SourceType = string(invest.EvidenceSourceDocument)
	case "hash":
		req.Hash = strings.Repeat("a", 64)
	case "payload":
		req.Payload = []byte(`{"a":1}`)
	}
	return req
}

// apa54AuthoritativeTable derives the ownership mapping from the real
// validator: knob K is owned by tool T iff a request for T carrying K
// passes Validate.
func apa54AuthoritativeTable() map[string][]string {
	table := map[string][]string{}
	for _, knob := range apa54Knobs {
		for _, tool := range apa54Tools {
			req := apa54SetKnob(apa54BaseRequest(tool), knob)
			if err := req.Validate(); err == nil {
				table[knob] = append(table[knob], string(tool))
			}
		}
	}
	for _, owners := range table {
		sort.Strings(owners)
	}
	return table
}

// apa54PromptTable parses the prompt's stated ownership mapping from the
// strict line format the template uses.
func apa54PromptTable(t *testing.T, prose string) map[string][]string {
	t.Helper()
	re := regexp.MustCompile(`^- "(\w+)": (optional|required); owned by ([^.]+)\.`)
	table := map[string][]string{}
	for _, line := range strings.Split(prose, "\n") {
		m := re.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		knob := m[1]
		for _, tool := range strings.Split(m[3], ", ") {
			table[knob] = append(table[knob], strings.TrimSpace(tool))
		}
	}
	for _, owners := range table {
		sort.Strings(owners)
	}
	return table
}

// TestKnobOwnership_PromptMatchesAuthoritativeTable is the divergence
// guard: the prompt's stated knob→tool mapping must equal the mapping the
// real validator enforces, in both directions.
func TestKnobOwnership_PromptMatchesAuthoritativeTable(t *testing.T) {
	prose := apa49Prose(t, apa49RenderPrompt(t))
	promptTable := apa54PromptTable(t, prose)
	authoritative := apa54AuthoritativeTable()

	// Every knob the prompt names must be owned by the same tools in the
	// authoritative table.
	for knob, owners := range promptTable {
		got := strings.Join(owners, ",")
		want := strings.Join(authoritative[knob], ",")
		if got != want {
			t.Errorf("prompt says %q owned by [%s], authoritative table says [%s]", knob, got, want)
		}
	}
	// And vice versa: every knob the authoritative table owns must be
	// named in the prompt.
	for knob, owners := range authoritative {
		if _, ok := promptTable[knob]; !ok {
			t.Errorf("authoritative table owns %q (by %v) but the prompt does not name it", knob, owners)
		}
	}
}

// TestKnobOwnership_PromptStatesTheSubjectIDRule pins the rule that closes
// the observed Qwen failure: a DocumentID from get_documents is not a
// valid subject_id, get_documents produces no subject identifier, and
// get_evidence does not accept subject_id.
func TestKnobOwnership_PromptStatesTheSubjectIDRule(t *testing.T) {
	prose := apa49Prose(t, apa49RenderPrompt(t))
	for _, want := range []string{
		"DocumentID",
		"NOT a valid subject_id",
		"get_documents does not produce a subject identifier",
		"get_evidence does not accept subject_id",
	} {
		if !strings.Contains(prose, want) {
			t.Errorf("prompt does not state %q", want)
		}
	}
}

// TestKnobOwnership_PromptDefinesSubjectIDPerTool pins that subject_id is
// defined per owning tool, not as one ambiguous value.
func TestKnobOwnership_PromptDefinesSubjectIDPerTool(t *testing.T) {
	prose := apa49Prose(t, apa49RenderPrompt(t))
	for _, want := range []string{
		"policy identifier",
		"TPA case identifier",
		"provider encounter identifier",
		"exception subject identifier",
	} {
		if !strings.Contains(prose, want) {
			t.Errorf("prompt does not define subject_id as %q", want)
		}
	}
}

// TestKnobOwnership_PromptNamesHashAndPayload pins that hash and payload
// are in the model-facing contract (they are authoritative but were
// omitted from the prompt).
func TestKnobOwnership_PromptNamesHashAndPayload(t *testing.T) {
	prose := apa49Prose(t, apa49RenderPrompt(t))
	for _, want := range []string{`"hash"`, `"payload"`} {
		if !strings.Contains(prose, want) {
			t.Errorf("prompt does not name %s", want)
		}
	}
}
