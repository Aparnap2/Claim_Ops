# Release contract — machine evidence map for `docs/ci/RELEASE_GATE.md`

_Last updated: 2026-10-24. Owner: release process. Implementation:
`.github/workflows/release-gate.yml` + `scripts/ci/release_gate.py`._

This document maps every criterion in `docs/ci/RELEASE_GATE.md` to the
machine evidence the release gate consumes, and names the criteria that
cannot be machine-checked and therefore require a recorded human
attestation. The gate is a **witness**, not a judge.

## 1. What the gate is and is not

- The gate **consumes** existing CI evidence via the GitHub API. It never
  re-executes tests, never rebuilds, never re-runs scanners.
- The gate is **dispatch-only** (`workflow_dispatch`). It has no push or
  pull_request trigger, so its output can never appear as — and must never
  be added to — a required PR check. A green gate run on a non-release SHA
  is expected and mints nothing.
- Gate green is **NECESSARY-NOT-SUFFICIENT** for a release. The decisive
  release action is a human merge/tag that records the gate run id. The
  gate's JSON carries `release_decision: null` in every verdict.
- **PR checks green ≠ qualified.** A PR's required checks (deterministic,
  terraform, static) prove the branch is mergeable; they say nothing about
  live qualification, integration evidence on the merged SHA, or posture
  freshness. Only the criteria below, evaluated on the release SHA, do.

## 2. Machine criteria (M) — evidence map

For every criterion the gate selects, per workflow, the most recent
**completed** run whose `head_sha` equals the target SHA (push and
workflow_dispatch runs preferred over pull_request runs when both exist
for the same workflow). A criterion passes only if that run's conclusion
is `success` (and, where noted, job-level conclusions are `success`).
Missing run, non-success conclusion, or API failure ⇒ the criterion fails
or is INDETERMINATE; both block (§4).

| #  | Criterion (RELEASE_GATE.md)                  | Evidence source | Pass rule |
|----|----------------------------------------------|-----------------|-----------|
| M1 | `deterministic` green on the SHA             | `ci.yml`, job `deterministic` | run exists for SHA, run + job `success` |
| M2 | `terraform` green on the SHA                 | `ci.yml`, job `terraform` | same run as M1, job `success` |
| M3 | `static` green on the SHA                    | `security.yml`, job `static` | run exists for SHA, run + job `success` |
| M4 | `govulncheck` green on the SHA               | `security.yml`, job `govulncheck` | same run as M3, job `success` |
| M5 | `pip-audit` green on the SHA                 | `security.yml`, job `pip-audit` | same run as M3, job `success` |
| M6 | `security-delta` N/A or satisfied            | `security.yml`, job `security-delta` | on a push/workflow_dispatch run the job is PR-only and absent ⇒ **N/A** (recorded, not silently dropped); on a pull_request run the job must exist and be `success` (exit 2 = indeterminate ⇒ fail) |
| M7 | `integration` green on the SHA               | `integration.yml`, job `services` | run exists for SHA, run + job `success`. The workflow's own sentinel steps (`check_pg_gated_packages.py`, `check_mockoon_gated_tests.py`, assert-services) make the job conclusion the skip-armor carrier |
| M8 | live qualification on the SHA                | `qualification.yml` run + its log | run exists for SHA with `success`, **and** its log contains `QUALIFIED: full suite green against a genuinely fresh, ephemeral database`, **and** `postgres-backed passing tests: N` with N ≥ 1. If `LLM cases measured: M/K` is present then M must equal K (a 429/skip is never a measurement); its absence means a PG-only run (recorded as scope). Missing/unreadable log or missing sentinel lines ⇒ INDETERMINATE ⇒ fail |
| M9 | security posture record fresh                | dated comments on the `Security posture runs` tracking issue | latest comment age ≤ `posture_max_age_hours` (input, default 168 = weekly cadence). No issue, no comments, or unparseable timestamp ⇒ INDETERMINATE ⇒ fail |

SHA handling: the `target_sha` input may be short; the gate resolves it to
a full SHA via the commits API before filtering runs. An ambiguous or
unresolvable SHA ⇒ INDETERMINATE ⇒ fail.

## 3. Human criteria (H) — recorded attestation

These cannot be machine-checked and are satisfied by the attestation
inputs (`attestation_statement`, `attested_by`, `attested_date`), which
the gate validates for form and echoes verbatim into the JSON:

| #  | Criterion (RELEASE_GATE.md) | Why not machine-checkable | Attestation surface |
|----|------------------------------|---------------------------|---------------------|
| H1 | `docs/KNOWN_LIMITATIONS.md` reviewed: every release-relevant row accounted for in the release note; a row without evidence, or a claim contradicted by a row, blocks the release | judging evidence sufficiency is judgment, not lookup | `attestation_statement` must attest the review |
| H2 | scanner findings remediated by reviewed commits; no gate auto-fixes or upgrades dependencies | the machine part (scanners green) is M4/M5; "reviewed commit" is human | `attestation_statement` |
| H3 | qualification scope sufficiency: whether the qualification run's declared scope (PG-only vs LLM matrix, recorded by M8) is sufficient for this release | sufficiency is a release judgment | `attestation_statement` (M8 records the scope being attested) |
| H4 | the release decision itself | by design outside the gate | human merge/tag recording the gate run id |

Attestation form rules: all three inputs non-blank and `attested_date` a
valid ISO `YYYY-MM-DD` date. A **missing** attestation is not an error —
it caps the verdict at `EVIDENCE_COMPLETE_NOT_A_RELEASE` (§5). A
**malformed** attestation (some fields set, others blank, or an
unparseable date) is a hard fail ⇒ `BLOCKED`.

## 4. Fail-closed rules

The gate blocks (`BLOCKED`, exit 2) when any of:

- a required run is missing for the target SHA;
- a required run or job conclusion is not `success` (the exact workflow,
  job, run id, and conclusion are named in the JSON);
- the qualification run's log is unavailable or lacks the sentinel lines
  (INDETERMINATE — never an implicit pass);
- the posture record is missing, stale, or unparseable (INDETERMINATE);
- the attestation is malformed;
- any API query fails (INDETERMINATE).

Every failure names the criterion and the reason. There is no path from
missing or ambiguous evidence to a pass.

## 5. Verdict vocabulary and JSON artifact

Exactly one machine-readable JSON decision is emitted — to the step
summary and as a workflow artifact. Verdicts:

| Verdict | Exit | Meaning |
|---------|------|---------|
| `BLOCKED` | 2 | at least one machine criterion failed or is INDETERMINATE, or the attestation is malformed |
| `EVIDENCE_COMPLETE_NOT_A_RELEASE` | 0 | every machine criterion passed on the target SHA and **no attestation was provided** — the strongest verdict possible without a human; explicitly not a release decision |
| `PASSED_WITH_ATTESTATION` | 0 | every machine criterion passed **and** a well-formed attestation was recorded — still not a release decision: the decisive release action is a human merge/tag recording this gate run id |

There is no verdict that mints a release. `release_decision` is `null` in
every verdict, and every JSON carries the disclaimer that the artifact is
a machine witness of machine-checkable criteria plus a recorded human
attestation — necessary, not sufficient, for a release.

JSON schema (v1):

```json
{
  "schema_version": 1,
  "verdict": "BLOCKED | EVIDENCE_COMPLETE_NOT_A_RELEASE | PASSED_WITH_ATTESTATION",
  "release_decision": null,
  "disclaimer": "Machine witness of machine-checkable criteria plus a recorded human attestation. NECESSARY-NOT-SUFFICIENT for a release; a human release action (merge/tag) must record this gate run id.",
  "gate": {"run_id": 0, "actor": "", "gate_workflow_sha": "", "generated_at": ""},
  "target_sha": "",
  "release_label": "",
  "evidence": [
    {"criterion": "M1", "workflow": "ci", "job": "deterministic", "run_id": 0,
     "conclusion": "success", "url": "", "head_sha": "", "timestamp": "",
     "status": "pass | fail | na | indeterminate", "reason": ""}
  ],
  "qualification": {"run_id": 0, "conclusion": "", "scope": "pg-only | llm-matrix",
                    "pg_backed_passing_tests": 0, "llm_measured": 0, "llm_expected": 0,
                    "log_evidence": true},
  "posture": {"issue_number": 0, "latest_comment_at": "", "age_hours": 0.0,
              "max_age_hours": 168, "status": "pass | fail | indeterminate"},
  "attestation": {"provided": false, "statement": "", "attested_by": "",
                  "attested_date": "", "status": "pass | missing | malformed"},
  "failures": [{"criterion": "", "reason": ""}]
}
```

`gate.gate_workflow_sha` is the head SHA of the ref the gate run executed
from — it identifies the gate definition version that produced the
artifact.

## 6. Permissions (least privilege, enumerated)

| Permission | Justification |
|------------|---------------|
| `actions: read` | list workflow runs and jobs for the target SHA (M1–M8) |
| `issues: read` | read the `Security posture runs` tracking issue comments (M9) |
| `contents: read` | resolve a short `target_sha` to a full SHA via the commits API; checkout of the gate definition. No writes anywhere |

## 7. Concurrency

`concurrency.group = release-gate-<target_sha>`, `cancel-in-progress:
false` — evaluations of the same SHA serialize; a cancelled gate run is
itself indeterminate and must not be produced by design.

## 8. Negative-control plan (dispatch-only, no releases)

| Control | Target | Expected verdict |
|---------|--------|------------------|
| (a) failed run | a main SHA whose `security` push run failed | `BLOCKED`, naming `security` + run id + conclusion |
| (b) green but unqualified | a main SHA with green ci/security/integration and no qualification run | `BLOCKED`, naming missing qualification (and missing posture record) |
| (c) stale posture | fresh posture record exists; gate dispatched with `posture_max_age_hours=0` | `BLOCKED`, naming posture staleness; default bound ⇒ posture passes |
| (d) attestation | no attestation / malformed attestation / full attestation | missing ⇒ attestation `missing`; malformed ⇒ `BLOCKED`; full ⇒ attestation `pass` while verdict stays `BLOCKED` on missing qualification (attestation alone mints nothing) |
| (e) skip-prevention | M8 log parsing: `postgres-backed passing tests: N ≥ 1`, `LLM cases measured: M/K` with M = K; missing counts ⇒ INDETERMINATE | unit-tested on fixtures built from real run evidence; the real gate blocks every historical SHA today because `qualification.yml` has never been dispatched — the fail-closed behavior working as designed |

The `EVIDENCE_COMPLETE_NOT_A_RELEASE` and `PASSED_WITH_ATTESTATION`
verdict paths are proven at the decision-logic level (unit tests on real
evidence plus a clearly-marked fixture qualification record). Their first
real-workflow demonstration requires dispatching `qualification.yml` on a
release-candidate SHA, which is deliberately out of scope for this PR.
