# Qualification record — 2026-11-18 (P3)

**Verdict: NOT QUALIFIED.** Fail-closed. This record binds exact-SHA evidence
to a candidate and enumerates what still blocks qualification. It is not a
release decision and mints nothing.

**Owner**: release process. **Scope**: docs only — no code, config, workflow,
or dependency change. **Companion documents**:
`docs/ci/RELEASE_GATE.md`, `docs/ci/RELEASE_CONTRACT.md`,
`docs/KNOWN_LIMITATIONS.md`.

---

## 1. What was evaluated, exactly

| Item | Value |
|---|---|
| Candidate SHA | `237466612957ea37971f222e0c246e53571130c8` (`feat/lifecycle-e2e` tip) |
| Candidate PR | #203 (draft, **unmerged**) |
| Base / merge-base | `main@029a4e6ce58fbc9c612cfd296dba5d99ca3a1877` |
| Candidate contents | P2 lifecycle E2E (`dc4acab`), dependency fix `92942e7`, PG 16.16 test fix `a4e8df1`, review hardening `5c0c5fe`/`9e6747e`/`b6d613c`/`2374666` |
| Independent review | two adversarial rounds, verdict MERGE-GO (§5) |
| Release-gate run on this SHA | **none** — `release-gate.yml` is dispatch-only and has never been run against this SHA |

**A candidate SHA is not a release SHA.** Per `RELEASE_CONTRACT.md` §1,
green PR checks prove a branch is mergeable; they say nothing about live
qualification on a merged SHA, posture freshness, or the human release
decision. Every M-criterion in §2 of that contract is defined on a **release
SHA**. This record therefore cannot, and does not, claim M1–M9 for a release.

## 2. Evidence bound to `2374666`

All runs below are on the exact candidate SHA and completed `success`.

| Layer | Workflow | Run | Job | Result |
|---|---|---|---|---|
| P0 deterministic gates (gofmt, go vet, unit tests, ruff, pytest) | `ci` | 44305412469 | `deterministic` 134805573474 | success |
| P0 infrastructure-as-code | `ci` | 44305412469 | `terraform` 134805573079 | success |
| Static analysis | `security` | 44305412065 | `static` 134805571954 | success |
| Go reachable-vulnerability scan | `security` | 44305412065 | `govulncheck` 134805572497 | success |
| Python dependency audit | `security` | 44305412065 | `pip-audit` 134805572246 | success |
| PR-introduced advisory delta | `security` | 44305412065 | `security-delta` 134805572350 | success |
| P1 emulator parity + P2 lifecycle | `emulator-services` | 44305412988 | `emulator-services` 134805575515 | success |
| PG-backed integration (RLS, cross-tenant) | `integration` | 44305412539 | `services` 134805573701 | success |
| Secret scanning | GitGuardian (external) | — | — | success (after rewrite, §6) |
| Review bot | CodeRabbit | — | — | skipped: draft PR (not evidence either way) |

Repository URL for every run:
`https://github.com/Aparnap2/Claim_Ops/actions/runs/<id>`.

### 2.1 Lifecycle E2E — what actually executed

`emulator-services` runs the four designated lifecycle tests and fails if any
does not emit `--- PASS:` (anti-fake-green). At `2374666`:

```
--- PASS: TestLifecycleE2E_SubmissionToDurableException (3.21s)
--- PASS: TestLifecycleE2E_Tier1NegativeControl (0.02s)
     restart redelivery converged: {Status:EXCEPTION Version:5 Events:5 Audit:4
       ClaimOutbox:2 Launches:1 Envelopes:1 Documents:1 FieldEvidence:0}
       (workflow executions=1, deliveries=1)
--- PASS: TestLifecycleE2E_RestartRedeliveryDurableIdempotency (20.73s)
     injected persistence failure surfaced as a Nack, as required:
       worker: intake progression: ERROR: permission denied for table claims
       (SQLSTATE 42501)
     persistence failure recovered with no duplicate effects: {Status:EXCEPTION
       Version:5 Events:5 Audit:4 ClaimOutbox:2 Launches:1 Envelopes:1
       Documents:1 FieldEvidence:0}
--- PASS: TestLifecycleE2E_PersistenceFailureNoAckRecovery (20.78s)
go test exit code: 0
```

Designated-package coverage from the same job (skips are failures):

```
ok  ./internal/adapters/pubsubadapter  pass=5 skip=0 exit=0
ok  ./internal/adapters/gcsblob        pass=2 skip=0 exit=0
Emulator-gated coverage OK: all designated tests passed.
```

### 2.2 What each proof establishes — and does not

| Proof | Establishes | Does not establish |
|---|---|---|
| `SubmissionToDurableException` | Real `POST /claims` → real `POST /claims/:id/documents` → real outbox dispatch → real Pub/Sub SDK (emulator) → real subscriber → production-constructed processor → durable `EXCEPTION` at version 5 with 5 events, 4 audit rows, 2 outbox rows, 1 investigation envelope, 1 launch row, 1 workflow execution | Real GCP IAM, quotas, or network behaviour; production launcher wiring (§4, KL-19) |
| `Tier1NegativeControl` | With `Parser == nil` the claim stays at `RECEIVED` (the pipeline is not vacuous) | Anything about the parser's production deployment (KL-03) |
| `RestartRedeliveryDurableIdempotency` | After a simulated crash between durable commit and ack, a **redelivery into a fresh `worker.Processor`** (empty in-memory state) converges to byte-identical durable state: zero duplicate events, audit rows, outbox rows, envelopes, launch rows, or workflow executions; exactly one delivery | Concurrent multi-consumer ordering (#60); real Pub/Sub redelivery timing |
| `PersistenceFailureNoAckRecovery` | A real `REVOKE`-induced `SQLSTATE 42501 permission denied for table claims` is **not acked**; the claim is untouched; after restore the redelivery completes exactly once to the canonical single-success state | Failure modes that are not privilege failures; broker outage semantics |

### 2.3 Local verification at the same tree

- `gofmt -l .` empty · `go vet ./...` clean · `go test ./...` green against
  live PG :5433, localgcp :8085/:4443, Mockoon :3001.
- Four lifecycle tests pass twice consecutively (~45 s per full run); no
  leftover `claimops_e2e_*` roles after either run.
- `ruff check .` clean.

## 3. Security evidence

- `govulncheck`: **0 reachable** vulnerabilities at `2374666`. The one
  reachable finding at `dc4acab` (`GO-2026-6888`, brotli v1.1.0 via Fiber,
  reached through `gcsblob.Store.Put` and `orchestrate.invalidError.Error`)
  was fixed by a patch-level transitive bump to v1.2.7 (`92942e7`), verified
  with the toolchain CI installs (`go1.27.4`).
- `pip-audit` green; `security-delta` green (no advisory introduced by the PR).
- Secret scanning: clean after the history rewrite in §6.
- **Not evidenced here**: OWASP ASVS L2 / threat-model review, authorization
  matrix testing, real-GCP IAM least-privilege review. These are P4 scope and
  were **not** started.

## 4. Known limitations that bear on qualification

Full register: `docs/KNOWN_LIMITATIONS.md`. Rows that block or materially
constrain a release claim:

| KL | State | Why it matters to this record |
|---|---|---|
| KL-01 | Blocked | GCW emulator short-circuits `retry:` steps ⇒ `ESCALATED`, HITL create/resume, and switch-dependent failure matrices are unqualifiable locally |
| KL-02 | Blocked | Execution adoption (create-409 path) unqualifiable against the emulator |
| KL-03 | Blocked | Worker parser image build/redeploy/real-document parse unverified from this repo |
| KL-09, KL-11 | Pending | Live model matrix and Fake-vs-Groq comparison are **not** qualified |
| KL-15 | Corrected | Emulator parity is not production GCP parity; no ClaimOps test exercises Logging/Trace |
| KL-17 | Measured | GCS adapter has no bounded deadline (pre-release required, #59) |
| KL-18 | Pending | Python domain spec still disagrees with the Go edge on `EXPIRED` |
| **KL-19** | **Pending (new)** | **`Processor.Launcher` is nil in production** ⇒ no investigation envelope, launch row, workflow execution, or expiry credential is produced for a real insufficient-evidence claim (#208) |

KL-19 is the single most consequential finding of this pass: the lifecycle
proofs in §2 wire the launcher by hand, so they qualify the launcher
**component**, not the application. Nothing in production launches today.

## 5. Independent review

Two adversarial review rounds were run on the P2.1 diff by a reviewer that did
not author it (read-only, no test execution by the reviewer):

1. Round 1 (`dc4acab`): verdict `PROVEN` for restart-redelivery idempotency,
   `WEAK` for the persistence-failure proof. Findings fixed in `5c0c5fe`,
   `b6d613c`, `2374666`: assert the surfaced error *is* the injected
   `42501 permission denied` (previously any transient error satisfied it),
   inject against a throwaway clone role with an exact copy of the app role's
   grants instead of `REVOKE`ing the shared role, build the clone pool from a
   mutated `pgxpool.Config` and assert `current_user` (because
   `ConnString()` returns the original DSN and silently ignores credential
   changes), restore the exact held privilege set, widen the ack window past
   a full ack deadline, and count every outbox row for the claim.
2. Round 2 (`2374666`): verdict `MERGE-GO`; NEW-1…NEW-5 all `CLOSED`
   (bounded cleanup locks, failing rather than logging a failed role drop,
   column/DOMAIN grant fidelity check, pre-REVOKE privilege assertion,
   20 s ack margin). Three non-blocking residuals recorded: column/DOMAIN
   grant branches are unexercised by the current schema, and grant-option
   parity is not compared.

Not reviewed by a second party: this record, and the pre-existing P0/P1
baseline (its evidence is a previous release's).

## 6. History rewrite (secret-scanning false positive)

GitGuardian flagged incident `40245750` — "Generic Password" at
`lifecycle_e2e_test.go:1347` of commit `f49ebc7`, the synthetic per-run
credential `password := "e2e-" + suffix` used for a throwaway test role. The
value was never a real or reused credential: it was generated from the
process id and a nanosecond timestamp, existed only inside a test that creates
and drops its own role, and granted nothing outside that role.

The literal was removed from the head in `b6d613c` (now generated with
`crypto/rand` and quoted with `format(%I, %L)`), but the commit range still
carried it, so the branch history was rewritten with owner authorization,
preserving the commit boundaries:

| Pre-rewrite | Post-rewrite | Change |
|---|---|---|
| `dc4acab` | `dc4acab` | unchanged |
| `92942e7` | `92942e7` | unchanged |
| `a4e8df1` | `a4e8df1` | unchanged |
| `f49ebc7` | `5c0c5fe` | literal replaced with a non-literal derivation |
| `ccfe581` | `9e6747e` | same file, same substitution |
| `0be1af3` | `b6d613c` | tree byte-identical to pre-rewrite `0be1af3` |

Post-rewrite checks: the final tree is identical to `0be1af3`
(`git diff 0be1af3 b6d613c` empty); no reachable commit in
`origin/main..2374666` contains the literal; the pre-rewrite head is retained
locally as `backup/pre-gg-rewrite` and was **not** pushed; GitGuardian is
green on `2374666`. `2374666` (review hardening) was added afterwards and is
not part of the rewrite.

If the same pattern were ever shown to be a real or reused credential, it
would have to be treated as an exposure and rotated; no evidence of that
exists.

## 7. Consolidated owner decisions outstanding

| # | Decision | Why it cannot be made by an agent |
|---|---|---|
| D1 | Accept (or reject) the scope delivered by #196 | Acceptance criteria sign-off |
| D2 | Ratify or amend the human-review lifecycle contract (#164 / ADR-011) | Product/domain authority: which transitions are automatic vs human-authorized |
| D3 | Enforce branch protection on `main` (required checks, no force-push, review count) | Repository settings, outside code |
| D4 | Authorize a deployment / real-GCP staging run (P6) | Irreversible, credentialed, external |
| D5 | Authorize P4–P7 (security ASVS L2, resilience/load, real-GCP staging, operator validation) | Cost/scope authorization; **registered only, not started** |
| D6 | The release decision itself: merge, tag, and record the `release-gate` run id | By design outside every gate (`RELEASE_CONTRACT.md` §1) |
| D7 | Confirm that PR-check evidence on a branch tip may **not** stand in for `release-gate` M1–M9 on a merged SHA | This record assumes it may not; owner confirmation keeps that assumption explicit |

## 8. What would move this to QUALIFIED

All of the following, on a **merged** release SHA, none of which is satisfied
today:

1. `release-gate.yml` dispatched against the release SHA, verdict
   `EVIDENCE_COMPLETE_NOT_A_RELEASE` or better, with M1–M9 all `pass`
   — including M8 (`qualification.yml` sentinel lines) and M9 (posture record
   fresh **and** `Suite result: **success**`).
2. KL-01, KL-02, KL-03, KL-10, KL-17 and KL-19 cleared or explicitly accepted
   in writing with their impact stated in the release note.
3. KL-09/KL-11 (live model evidence) resolved or explicitly scoped out for the
   release, with H3 attestation recording that scope decision.
4. P4 security evidence (ASVS L2 mapping, authorization matrix) — not started.
5. P6 evidence from a real-GCP staging deployment — not started.
6. The H1–H4 attestations recorded; D1–D6 closed.

Until then the release status in `docs/specs/06_PROGRESS.md` and issue #163
remains **NOT QUALIFIED**, and PR #203 remains a draft.
