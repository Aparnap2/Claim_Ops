# ClaimOps — Known Limitations Register

Last reconciled: 2026-10-08 (against `main@dffa160`)

## Scope

A register of limitations that are **known, evidenced, and
release-relevant**: things that stop ClaimOps from being qualified,
deployable, or trustworthy to a standard a reader would otherwise assume
it meets.

This is not a security checklist, not a risk register, and not a wish
list. Every row is independently verifiable; a row whose evidence cannot
be produced does not belong here.

Excluded by design:

- Ordinary engineering debt with no release consequence.
- Findings already closed in `docs/specs/09_DECISION_LOG.md`.
- Work in flight, which lives in `docs/specs/06_PROGRESS.md`.
- Anything unevidenced. A suspected limitation belongs in the issue
  tracker until it can be measured; guessing here would defeat the file.

## How to read this file

| Column | Meaning |
|---|---|
| Limitation | What is true, stated so it can be disproved |
| Impact | What cannot be concluded or shipped because of it |
| Owner / dependency | Who or what has to change before it clears |
| State | This repo's ability to *prove* a claim — not the quality of the thing |
| Evidence | Minimum artefact a reviewer needs: repo path, SHA, or Linear id |

`State` values:

| Value | Meaning |
|---|---|
| `Blocked` | Cannot be qualified from this repo at all; an external party holds the unlock |
| `Pending` | Qualifiable here, but the qualification has not been run or did not complete |
| `Operational` | Not a correctness defect; a standing operational constraint on throughput or capacity |
| `Accepted` | A real cost, consciously accepted and recorded, with a stated reason |
| `Measured` | Behaviour is known and asserted by a test, not tolerated silently |

## Conflict resolution

`AGENTS.md` governs this file. Hierarchy on conflict: GitHub issue →
ADRs/contracts → `docs/specs/` → tests/fixtures → judgment. If sources
conflict, STOP and report; never silently choose. Where this register
records a conflict it names the conflict (KL-13) rather than resolving it
by fiat.

---

## Register

| ID | Limitation | Impact | Owner / dependency | State | Evidence |
|---|---|---|---|---|---|
| KL-01 | GCW emulator v0.5.0 short-circuits any successful HTTP step carrying a `retry:` block; execution ends `SUCCEEDED`/null instead of continuing. Bisected upstream of ClaimOps: the same step without `retry:` continues. The frozen `workflows/claim-investigation.yaml` carries `retry:` on the investigate POST and on the decision/expire POSTs, so the `check` switch never runs. | The `ESCALATED` branch, HITL callback creation, HITL callback resume, and every switch-dependent failure-matrix case are unqualifiable locally. APA-37 workflow conformance cannot be proven. | Upstream `lemonberrylabs/gcw-emulator`. An upstream patch exists (`4b64b32`, held locally) but needs an account with push access, an upstream PR, and a published image. | `Blocked` | Linear APA-37; Linear APA-46 (resumption checklist); PR #102 conformance gate; `workflows/claim-investigation.yaml:20,70,90,132,152`; `infra/gcw/README.md:19` (v0.5.0) |
| KL-02 | GCW execution adoption (`StartExecution` create-or-return on a create-409) is unreachable against the emulator: v0.5.0 ignores `executionId` and never returns 409. | The 409 → create-or-return half of the launch/idempotency contract is covered by unit tests with a stub server only. APA-40 crash-window adoption cannot be qualified locally. | Real Cloud Workflows credentials, or a provider whose 409 semantics match the contract. | `Blocked` | Linear APA-40; `apps/api/internal/workflow/gcw_provider.go:117-129,182-183`; `apps/api/internal/workflow/provider_test.go:207-242`; `apps/api/internal/workflow/apa41_resource_name_test.go:184-201` |
| KL-03 | The worker parser image (LiteParse) build, redeploy, and one real production-like parse cannot be verified from this repo. The GitHub integration workflow does not build or deploy the worker image, so the build context moved to the repo root without any in-repo caller exercising it. | APA-48 is code-complete but not deployment-verified. Until the external pipeline rebuilds from the new context, redeploys, and parses one real document, the production parse outage is not closed. | External deployment pipeline: `docker build -f apps/api/Dockerfile.worker .` from the repo root, then redeploy and parse. | `Blocked` | Linear APA-48; `d47a73c` / PR #115 → `main@104d46c`; `tools/parsers/liteparse/container_smoke.sh` |
| KL-04 | Live cognitive qualification throughput is sensitive to Groq quota. A capacity probe at the start of a run does not predict the run: the APA-55 matrix saw three consecutive HTTP 200 responses at +0s/+25s/+50s, then 45 planned calls exceeded the remaining quota and four of five scenarios recorded zero measurements. | Qualification runs can go `INCOMPLETE` for capacity reasons. This affects throughput, not correctness — no boundary was ever shown to fail under throttling. | Groq quota / credential tier. | `Operational` | Linear APA-64; Linear APA-63 ("earlier 429s were sustained-quota exhaustion, not request incompatibility"); `infra/postgres/qualify.sh` `QUAL_LLM_SCENARIOS` gate |
| KL-05 | The APA-56 causal trajectory (`policy_number` → `kind:"field"` → validator accepts → grounding executes) has not been observed on a live model. The S1 envelope seeds no `MissingEvidence`, so the additive grounding check is vacuous there and cannot witness the fix. | Release evidence gap. APA-56 is a prompt-only correction whose causal claim is unproven; `d778ddc` shipped it with no real-LLM run by design. | APA-56 causal qualification — a live run against an envelope that actually carries `MissingEvidence`. | `Pending` | Linear APA-56; `d778ddc` / PR #122; `apps/api/internal/investigate/orchestrate/apa52_s1_pg_live_test.go:402-440` (`s1Envelope` sets no `MissingEvidence`); `apps/api/internal/investigate/orchestrate/grounding.go:144` |
| KL-06 | Go `encoding/json` v1 does not reject duplicate object keys; it is last-wins. A payload carrying `"action":"call_tool","action":"submit_report"` decodes to the second value. | A model cannot smuggle an unvalidated action through this: the decoded action is still subject to the full `ValidateModelAction` boundary. The cost is that the decoder is not a duplicate-key detector, so the boundary must be. | None — measured, asserted, and tolerated. | `Measured` | `apps/api/internal/investigate/orchestrate/apa55_live_qualification_test.go:231-289` (`TestAPA55_C_DuplicateKeys_Corrected`); `apps/api/internal/investigate/orchestrate/apa55_phase2_qualification_live_test.go:150-164` |
| KL-07 | The worker image grew 48.3MB → 132MB to carry a pinned Python + LiteParse parse runtime. | A ~2.7× image size increase, accepted deliberately. It is a standing argument for pursuing a Go-native parser boundary later, never a reason to weaken the parser or evidence contract. | Accepted trade-off; revisit under APA-47. | `Accepted` | Linear APA-48; `d47a73c` ("Image: 48.3MB -> 132MB. Correctness of the artifact over size") |
| KL-08 | The closed external-evidence vocabulary — the four keys `policy`, `tpa`, `provider`, `risk` — is written twice: once as `externalSources` in `invest/exception.go` and once as an inline `switch` in `orchestrate/action.go`. | Adding a fifth external source can be applied to one site and missed at the other. Deliberately left as debt: it is a refactor, not a bug fix, and folding it into a prompt-contract fix would have contaminated the change. | Engineering, unscheduled. | `Accepted` | `apps/api/internal/invest/exception.go:758` (`externalSources`) and `apps/api/internal/investigate/orchestrate/action.go:218` (inline `switch`); `d778ddc` commit message ("recorded as technical debt rather than folded in here") |
| KL-09 | The APA-55 live qualification matrix is **not qualified**. The last measured run was INCOMPLETE: 1/5 scenarios measured. The control measured 3/3 as `ESCALATED(DEADLINE)` with `toolExecutions=0`, and the other four scenarios recorded zero measurements under provider throttling. | No claim about model behaviour on insufficient / fabricated / stale / valid-tool scenarios. `violations=0` on the one measured scenario means nothing leaked — it is not a pass. | APA-64 Cause 1 adjudication (fixture deadline vs harness pacing), then provider capacity, then a live re-run. | `Pending` | Linear APA-64; Linear APA-55; `dffa160` / PR #130 (deadline/pacing adjudication); `apps/api/internal/investigate/orchestrate/apa64_deadline_pacing_adjudication_test.go` |
| KL-10 | The APA-37 conformance gate (PR #102) cannot go green until KL-01 clears, and must not be edited to accommodate the emulator defect. | The gate is a standing red, not a passing test. Treat a green APA-37 as a signal to check that the gate was not weakened. | Same upstream unlock as KL-01. | `Blocked` | Linear APA-46 (hard rules); Linear APA-37; PR #102 |
| KL-11 | The Fake-vs-Groq comparison on the frozen eval-v1 harness (hard gates `fabricated=0`, `crossTenant=0`, `unauthorized=0`) is a manual live-key gate, not a CI gate, and has not been run. | Live-key evaluation cannot be claimed from CI evidence. The seam itself is qualified offline (KL-12 aside) and no key material exists in the repo. | `GROQ_API_KEY` plus a manual live-key run per ADR-002. | `Pending` | `docs/adr/002-groq-llm-provider.md:53-54`; Linear APA-45 ("NOT qualified: real Fake-vs-Groq comparison … stays a manual live-key gate") |
| KL-12 | The production cognitive path did not reach `REPORT_READY` until five model-facing contract corrections landed: APA-49 (`act` vs `action` — 100% of real-model turns failed strict decode), APA-50 (act-transition and no-repeat policy), APA-54 (tool/knob ownership), APA-56 (`missing_additive` kind↔key vocabulary), APA-59 (per-tool limit bounds). | Each was a defect in the prompt contract, not in the validator, and each was found by a live run rather than by inspection. The practical consequence is that prompt/contract parity with the deterministic validator is not self-enforcing; the divergence tests added alongside them are the only guard, and they are tests, not types. | Engineering discipline: every new model-facing contract needs a divergence test pinned to the authoritative constant. | `Accepted` | Linear APA-49, APA-50, APA-54, APA-56, APA-59; `apps/api/internal/investigate/orchestrate/apa59_production_fidelity_test.go:30`; `d778ddc`, `3238ddf`, `6a308c6` |
| KL-13 | Identifier conflict, unresolved. Commits `ad0d631` and PR #114 label the Groq→Qwen re-adjudication "(APA-47)", and ADR-002 line 28 repeats it. Linear APA-47 is a different slice — "Architecture: replace LiteParse Python subprocess with Go OCRProvider boundary (audit first)", still `Backlog`. | Citations of "APA-47" resolve to two unrelated pieces of work. Anyone following the ADR's citation lands on the wrong issue. | Linear/project owner: renumber the re-adjudication or re-point ADR-002 and the commit history at the correct identifier. | `Pending` | `ad0d631` / PR #114; `docs/adr/002-groq-llm-provider.md:28`; Linear APA-47 (Backlog, OCRProvider architecture) |
| KL-14 | Linear APA-45's body records `canonical llama-3.1-8b-instant is what goes outbound` as one of its five qualified areas. That string was retired from Groq and answers HTTP 404 `model_not_found`; it is no longer what goes outbound. | APA-45 is cited as evidence elsewhere, and its model-string claim is now false. The unit tests it qualified were renamed to the new value in `ad0d631`; the issue body was not. | Linear: annotate APA-45 as superseded on this point. | `Measured` | Linear APA-45; `ad0d631` ("The two APA-45 test literals that pin the canonical string are renamed to the new value"); `docs/adr/002-groq-llm-provider.md:28-34` |

---

## Notes on the rows most likely to be misread

**KL-01 is not a workflow defect.** `workflows/claim-investigation.yaml` is
frozen and its `retry:` blocks are correct GCW syntax. The defect is in the
emulator. The standing rule is not to reopen APA-37 with a ClaimOps
workaround, not to mirror the workflow YAML, and not to fork-and-pin the
emulator (Linear APA-46).

**KL-02's code half is qualified; only the provider half is not.** The
409 → create-or-return mapping and the guard that rejects a 409 with no
requested execution name both have unit coverage against a stub server
(`provider_test.go`). What is missing is the real provider behaviour.

**KL-05 is an evidence gap, not a suspected defect.** APA-56's validator
is correct and is unchanged; the regression test pins that
`policy_number` under `kind:"external"` is still rejected. The prompt was
corrected. What has not happened is a live observation that the corrected
prompt produces the corrected trajectory.

**KL-06 is tolerance, not acceptance of risk.** The behaviour is asserted
rather than merely logged, so a future decoder that changes which action
wins fails the test instead of passing quietly.

**KL-12's five corrections are the argument for prompt/contract tests,
not for relaxing a validator.** In every case the deterministic bound was
kept and the prompt was corrected. The five divergence tests added are the
durable artefact.

## Update rule

IDs are stable once this file is merged. Renumber freely only before the
first merge; after that, retire an ID rather than reuse it, and say in the
closing decision why.

Add a row when a limitation is measured or an upstream block is confirmed.
Change a row's state only when the evidence changes. Delete a row when the
limitation is closed, and record the closing decision in
`docs/specs/09_DECISION_LOG.md` so the history survives the deletion.

Re-reconcile this file whenever a blocked item clears, and at minimum
whenever `docs/specs/06_PROGRESS.md` changes milestone status.
