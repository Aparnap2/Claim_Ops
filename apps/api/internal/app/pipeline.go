package app

import (
	"time"

	httpadapter "claimops-api/internal/adapters/http"
	workeradapter "claimops-api/internal/adapters/worker"
	"claimops-api/internal/extract"
	"claimops-api/internal/invest"
	"claimops-api/internal/parser"
	"claimops-api/internal/ports"
	"claimops-api/internal/verifywrap"
	"claimops-api/internal/worker"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ProcessorDeps bundles what the deterministic document pipeline needs.
// Both entrypoints (cmd/api pull loop, cmd/worker push endpoint) build
// the identical processor from these deps — the only difference between
// runtimes is transport, never application behavior.
//
// The #78 full-chain fields (Parser, Extractors, PolicyExternals,
// ScopeAllowTools, ScopeMaxCalls, ScopeDeadlineMs) are optional and read
// ONLY by BuildFullProcessor: nil/zero selects the documented adapter
// defaults. BuildProcessor ignores them, so Tier-1 behavior is unchanged.
type ProcessorDeps struct {
	Blobs         ports.BlobStore
	Pool          *pgxpool.Pool
	PolicyBaseURL string
	PolicyTimeout time.Duration

	// Parser is the canonical parser boundary implementation. Nil
	// selects the LiteParse-only adapter (#30, OCR off per ADR-007).
	Parser parser.Parser
	// Extractors maps document-type string (xdoc.Doc*, the same key
	// space verify consumes) to its deterministic extractor (#45).
	// Nil selects the six-extractor xdoc registry.
	Extractors map[string]extract.Extractor
	// PolicyExternals is the policy-side judgment metadata verifywrap
	// copies verbatim into verify.Input (PolicyNumber/PolicyPatient/
	// PolicyActive/ExternalPolicyOK/ExternalMismatch/Duplicates).
	// Zero values intentionally fire R4/R9 by engine design, so callers
	// MUST supply them before mapping.
	PolicyExternals verifywrap.Externals
	// ScopeAllowTools is the per-investigation least-privilege tool
	// subset. Nil selects the read-only default (writer excluded: the
	// orchestrate loop never calls it).
	ScopeAllowTools []invest.ToolName
	// ScopeMaxCalls bounds one investigation's logical tool calls. <= 0
	// selects DefaultScopeMaxCalls (5, matching the eval harness).
	ScopeMaxCalls int
	// ScopeDeadlineMs bounds one investigation's wall clock. <= 0
	// selects DefaultScopeDeadlineMs (60000, matching the corpus).
	ScopeDeadlineMs int64
	// WebhookSecret enables the worker's pre-signed workflow timeout
	// credential (S5/APA-26 webauth mint). Empty disables minting; the
	// launch argument then omits expire fields.
	WebhookSecret string
}

// BuildProcessor wires the Tier-1 pipeline: GCS-backed fetch, idempotent
// postgres store, claim loader, Mockoon-backed policy check.
func BuildProcessor(d ProcessorDeps) *worker.Processor {
	store := workeradapter.NewStoreBridge(d.Pool)
	timeout := d.PolicyTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return worker.NewProcessor(
		workeradapter.NewBlobFetchBridge(d.Blobs, store),
		store,
		workeradapter.NewClaimBridge(d.Pool),
		workeradapter.NewPolicyBridge(httpadapter.NewPolicyClient(
			httpadapter.New(d.PolicyBaseURL, timeout))),
	)
}

// FullProcessor is the #78 full-chain wiring: the Tier-1 worker.Processor
// carrying the deterministic chain components on its resolved seams:
// canonical parser, DocType-routed extractor registry, assembly-ready
// facts contract (assemble.Assemble is a pure function over the registry
// output — no state to hold), verifywrap externals, and validated
// investigation scope budgets. Handle routes through runNewPipeline
// whenever Processor.Parser is non-nil (Tier-1 regex path otherwise);
// BuildFullProcessor always injects the resolved seams below, so the
// embedded Processor executes the full chain.
type FullProcessor struct {
	Processor *worker.Processor
	Parser    parser.Parser
	// Extractors maps document-type string to extractor, validated
	// non-empty with recorded Name/Version on every entry.
	Extractors map[string]extract.Extractor
	// Externals is the policy-side judgment metadata for verifywrap.Map.
	Externals verifywrap.Externals
	// ScopeAllowTools is the resolved least-privilege tool subset
	// (allowlisted, duplicate-free).
	ScopeAllowTools []invest.ToolName
	// ScopeMaxCalls is the resolved per-investigation tool-call budget.
	ScopeMaxCalls int
	// ScopeDeadlineMs is the resolved per-investigation deadline.
	ScopeDeadlineMs int64
}

// BuildFullProcessor wires the full deterministic chain through the
// actual worker construction: liteparse adapter (default), xdoc extractor
// registry keyed by parser DocType (default), assembly contract (pure —
// carried by the registry output shape, no wiring state), verifywrap
// externals (caller-supplied policy metadata), and invest scope budgets
// (validated against the invest allowlist). The embedded Processor is
// built by BuildProcessor, so Tier-1 behavior is identical; the chain
// components ride alongside for the worker seam.
//
// BLOCKER RESOLVED (seam exists): worker.Processor now carries the
// Parser/Extractors/ScopeAllowTools/ScopeMaxCalls/ScopeDeadlineMs seams
// and process() routes through runNewPipeline whenever Parser is non-nil
// (Tier-1 regex path otherwise). BuildFullProcessor injects the resolved
// seams below, so full-chain EXECUTION runs through the embedded
// Processor with all dependencies already constructed and validated here.
func BuildFullProcessor(d ProcessorDeps) (*FullProcessor, error) {
	p := d.Parser
	if p == nil {
		p = workeradapter.NewDefaultParser()
	}
	reg := d.Extractors
	if reg == nil {
		reg = workeradapter.DefaultExtractorRegistry()
	}
	if err := workeradapter.ValidateExtractorRegistry(reg); err != nil {
		return nil, err
	}
	tools, maxCalls, deadlineMs, err := workeradapter.ResolveScopeDefaults(
		d.ScopeAllowTools, d.ScopeMaxCalls, d.ScopeDeadlineMs)
	if err != nil {
		return nil, err
	}
	cp := make(map[string]extract.Extractor, len(reg))
	for k, v := range reg {
		cp[k] = v
	}
	proc := BuildProcessor(d)
	proc.Parser = p
	proc.Extractors = cp
	proc.ScopeAllowTools = tools
	proc.ScopeMaxCalls = maxCalls
	proc.ScopeDeadlineMs = deadlineMs
	proc.WebhookSecret = d.WebhookSecret
	return &FullProcessor{
		Processor:       proc,
		Parser:          p,
		Extractors:      cp,
		Externals:       d.PolicyExternals,
		ScopeAllowTools: tools,
		ScopeMaxCalls:   maxCalls,
		ScopeDeadlineMs: deadlineMs,
	}, nil
}
