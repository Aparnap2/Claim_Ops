// Full-chain defaults for issue #78: the deterministic parser → extract
// → assemble → verify → exception → investigation path, made reachable
// through the actual worker wiring (app.BuildFullProcessor).
//
// Contents are pure defaults and validation only: stdlib plus the
// existing parser/extract/xdoc/liteparse/invest modules. No HTTP, no DB
// access, no logging, no clock reads. Transactions and tenant scoping
// stay in the bridges (store_bridge.go, claim_bridge.go,
// blob_fetch_bridge.go); this file owns no I/O and therefore no tenant
// context.
package workeradapter

import (
	"fmt"
	"strings"

	"claimops-api/internal/extract"
	"claimops-api/internal/extract/xdoc"
	"claimops-api/internal/invest"
	"claimops-api/internal/parser"
	"claimops-api/internal/parser/liteparse"
)

// Scope budget defaults. They mirror the evaluated baselines so app wiring
// and the eval harness cannot silently fork: MaxCalls 5 matches the
// E0-harness scope budget (eval/invest/harness.go DefaultScopeMaxCalls),
// DeadlineMs 60000 matches the corpus envelope default
// (eval/invest/corpus.go).
const (
	// DefaultScopeMaxCalls bounds one investigation's logical tool calls.
	DefaultScopeMaxCalls = 5
	// DefaultScopeDeadlineMs bounds one investigation's wall clock (60s).
	DefaultScopeDeadlineMs = int64(60000)
)

// DefaultScopeTools is the least-privilege read-only tool subset wired
// when the caller declares none. The writer
// (create_investigation_report) is deliberately excluded: the orchestrate
// loop never calls the writer, and report submission is a separate
// write-once stage outside the bounded read loop.
func DefaultScopeTools() []invest.ToolName {
	return []invest.ToolName{
		invest.ToolGetClaim,
		invest.ToolGetDocuments,
		invest.ToolGetEvidence,
		invest.ToolGetPolicyContext,
		invest.ToolGetVerificationFindings,
	}
}

// NewDefaultParser builds the LiteParse-only parser adapter (#30) with
// adapter defaults: the production ExecRunner (empty binary/shim select
// liteparse.DefaultPythonBin/DefaultShimPath) and DefaultTimeout. OCR
// stays off per ADR-007; managed OCR escalation (if ever evidenced) is a
// future adapter change, not a wiring change.
func NewDefaultParser() parser.Parser {
	return liteparse.New(liteparse.NewExecRunner("", "", 0), 0)
}

// DefaultExtractorRegistry maps every xdoc document type to its
// deterministic extractor (#45). Keys are the extractor-emitted DocType
// strings (xdoc.Doc*), which the assembler passes through verbatim and
// verifywrap routes by (F11a); the registry key space is therefore the
// same string set verify consumes, with no translation table to drift.
func DefaultExtractorRegistry() map[string]extract.Extractor {
	return map[string]extract.Extractor{
		xdoc.DocClaimForm:        xdoc.ClaimForm{},
		xdoc.DocDischargeSummary: xdoc.DischargeSummary{},
		xdoc.DocHospitalBill:     xdoc.HospitalBill{},
		xdoc.DocPolicySchedule:   xdoc.PolicySchedule{},
		xdoc.DocPreauthForm:      xdoc.PreauthForm{},
		xdoc.DocLabReport:        xdoc.LabReport{},
	}
}

// ValidateExtractorRegistry checks a DocType → Extractor registry
// standalone (fail closed): non-empty, non-blank keys, non-nil values
// with non-blank recorded Name/Version (the extract conformance runner
// requires both for provenance).
func ValidateExtractorRegistry(reg map[string]extract.Extractor) error {
	if len(reg) == 0 {
		return fmt.Errorf("workeradapter: extractor registry is empty (needs one extractor per document type)")
	}
	for key, ext := range reg {
		if strings.TrimSpace(key) == "" {
			return fmt.Errorf("workeradapter: extractor registry has a blank document-type key")
		}
		if ext == nil {
			return fmt.Errorf("workeradapter: extractor registry key %q has a nil extractor", key)
		}
		if strings.TrimSpace(ext.Name()) == "" {
			return fmt.Errorf("workeradapter: extractor registry key %q has a blank extractor name", key)
		}
		if strings.TrimSpace(ext.Version()) == "" {
			return fmt.Errorf("workeradapter: extractor registry key %q has a blank extractor version", key)
		}
	}
	return nil
}

// ResolveScopeDefaults applies the scope defaults (#78: allow-tools
// subset, max calls, deadline) and validates the result. Nil tools select
// DefaultScopeTools; non-positive MaxCalls/DeadlineMs select their
// defaults. Validation mirrors investigate.Scope for exactly these three
// fields — non-empty duplicate-free allowlist drawn from
// invest.Allowlist, MaxCalls >= 1, DeadlineMs >= 100ms — so app wiring
// can never construct a scope the executor would reject. Tenant/claim/
// request binding is per-investigation and stays downstream; this function
// owns only the app-level budget defaults.
func ResolveScopeDefaults(tools []invest.ToolName, maxCalls int, deadlineMs int64) ([]invest.ToolName, int, int64, error) {
	if tools == nil {
		tools = DefaultScopeTools()
	}
	if maxCalls <= 0 {
		maxCalls = DefaultScopeMaxCalls
	}
	if deadlineMs <= 0 {
		deadlineMs = DefaultScopeDeadlineMs
	}
	if len(tools) == 0 {
		return nil, 0, 0, fmt.Errorf("workeradapter: scope allow_tools is empty (least privilege needs an explicit subset)")
	}
	seen := make(map[invest.ToolName]struct{}, len(tools))
	for _, t := range tools {
		if !invest.IsAllowlisted(t) {
			return nil, 0, 0, fmt.Errorf("workeradapter: scope tool %q not in allowlist", string(t))
		}
		if _, dup := seen[t]; dup {
			return nil, 0, 0, fmt.Errorf("workeradapter: scope duplicates tool %q", string(t))
		}
		seen[t] = struct{}{}
	}
	if maxCalls < 1 {
		return nil, 0, 0, fmt.Errorf("workeradapter: scope max_calls must be >= 1")
	}
	if deadlineMs < 100 {
		return nil, 0, 0, fmt.Errorf("workeradapter: scope deadline_ms must be >= 100")
	}
	return append([]invest.ToolName(nil), tools...), maxCalls, deadlineMs, nil
}
