// Package invest defines the typed investigation boundary for ClaimOps
// (issue #53, contracts only).
//
// Boundary position:
//
//	extract.DocumentFacts (observation, #44/#45)
//	  -> assemble.CanonicalClaim (agreement, #46)
//	    -> verify.Input / verify.Result + verifywrap.Unresolved (judgment + HITL channel, #47)
//	      -> invest.UnresolvedException (THIS PACKAGE: united investigation input)
//	        -> invest.InvestigationInput (POST /investigate-equivalent payload)
//	          -> (future) InvestigationOutput — NOT defined here
//
// CONTRACTS ONLY. This package contains no agent, no model client, no tool
// implementation, no planning, and no prompts. It defines the shapes the
// deterministic side produces and the future cognitive side must consume,
// plus the validators that reject anything outside those shapes. There is
// intentionally no dependency on any ADK/LLM SDK (none exists in go.mod).
//
// Isolation precedent: like internal/parser contains vendor adapters in
// internal/parser/liteparse without letting vendor types cross the parser
// boundary, any future investigation executor/tools must live in a
// subpackage (e.g. internal/invest/executor) and must never widen the
// surface declared here. The allowlist in capability.go is the whole
// reachable world; the executor may only dispatch names from it.
//
// Epistemic model (structural, not advisory):
//
//		FACT -> EVIDENCE -> HYPOTHESIS -> FINDING -> RECOMMENDATION -> HUMAN -> MUTATION
//
//	  - FACT: agreed-clean values (AgreedField). Read-only.
//	  - EVIDENCE: pinned provenance rows (EvidenceRef). Cited, never invented.
//	  - HYPOTHESIS: candidate explanation with a REQUIRED falsifier
//	    (structural uncertainty instead of confidence floats).
//	  - FINDING: hypothesis surviving retrieval; cites evidence only.
//	  - RECOMMENDATION: closed 4-action enum; carries no transition, no
//	    verdict, no state mutation shape of any kind.
//	  - HUMAN: HumanDecision, the only record that can authorize a
//	    transition (actor + role + idempotency key + expected version).
//	  - MUTATION: existing claims.Transition / workflow.Apply only. There is
//	    no mutation type or helper in this package, and there never will be.
//
// HYPOTHESIS->FACT and RECOMMENDATION->STATE are impossible by
// construction: every stage is a distinct named type, all validation is
// package-level functions (no methods, hence no conversion helpers to
// discover), strict JSON decoding rejects stage-foreign fields, and the
// contract tests assert both impossibility directions via the
// zero-methods and cross-stage-decode subtests.
//
// G2 (schema resolution): new types exist only where resolution adds
// information — FieldSourceView / ReviewItemView / CandidateView (flattened
// provenance + resolved stored-evidence IDs) and the envelope itself.
// Everything else reuses domain types: verify rule codes/severities/doc
// keys, assemble statuses/conflict/review shapes, extract statuses/evidence
// pins, claims tenant/claim value objects, verifywrap.Unresolved as the
// Build input. No map[string]any in any serialized form. No confidence
// floats. No magic strings: every closed set is a typed constant block.
//
// G3 (exception enum migration): the smallest compatible change is NO
// change. The verify R1-R10 codes are already the single taxonomy in Go:
// worker/processor.go emits verify.Verify codes into Outcome.ExceptionCodes
// and metrics labels; nothing in Go reads or writes the legacy
// exceptions.type enum in migrations/001_init.sql (it is unreferenced DDL).
// This package therefore references the verify constants directly
// (RuleCode values ARE verify.Code* values) and REJECTS the legacy SQL
// enum strings as rule codes. No second taxonomy is created, no serialized
// value is renamed, and no migration is required for these contracts.
// Rule findings live on future report tables keyed by verify codes, never
// on the legacy exceptions table (spec §4.1 option (b)).
//
// Determinism: every sorted-contract slice in every serialized form is
// kept in sorted order; the two encounter-order contents are preserved
// verbatim and never re-sorted — review candidates (artifact encounter
// order) and rule findings (verify R1-R10 emission order). Marshal
// canonicalizes a copy before encoding so byte output is stable
// regardless of construction path, and pure representations carry no
// timestamps, no random IDs, and no floats. IDs (ex-, inv-) are
// caller-minted via NewExceptionID / NewInvestigationID and passed in,
// keeping Build pure. encoding/json v1 only (json/v2 is rejected for
// byte-sensitive paths per decision log).
//
// Field keys (UnresolvedField.Key, AgreedField.Key, FactRef.Key, and the
// MissingItem.Key of field-kind items) are intentionally open: they carry
// assemble/extract canonical keys, never a closed enum, so new pipeline
// fields flow through without a contract change. Validation requires
// non-blank keys only; the closed sets in this package are rule codes,
// severities, statuses, tool names, missing kinds, source types,
// recommendation actions, decisions, and roles.
//
// Tenancy: every envelope, scope block, and evidence ref carries tenant;
// cross-tenant references are rejected at build and at decode. Structured
// representations carry identifiers, hashes, counts, and codes only —
// never raw PII/PHI, document contents, secrets, or tokens.
package invest
