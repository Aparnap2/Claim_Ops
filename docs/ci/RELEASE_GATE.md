# Release gate

What must be green before a change merges, and what must be qualified
before a release claim is made.

## Merge requirements (branch protection, strict)

| Check | Workflow | Proves |
|-------|----------|--------|
| `deterministic` | `ci.yml` | format, lint, typecheck, unit tests, race tests, live LiteParse shim smoke, secret guards, build-tag isolation |
| `terraform` | `ci.yml` | Terraform configs parse and validate |
| `static` | `security.yml` | module integrity (`go mod verify`), Python env consistency, secret-pattern guard |

These contexts must stay required and strict (`strict: true`); this PR
does not weaken them.

## Advisory but load-bearing

| Check | Workflow | Notes |
|-------|----------|-------|
| `govulncheck` | `security.yml` | fails on reachable Go CVEs; remediation is a reviewed commit |
| `pip-audit` | `security.yml` | fails on any advisory in the locked Python set |
| `security-delta` | `security.yml` (PRs only) | fails only on advisories the PR introduces; exit 2 = indeterminate, re-run |
| `integration` (services) | `integration.yml` (pushes to main) | PG + Mockoon-backed tests with anti-fake-green assertions |
| `security-posture` | `security-posture.yml` (weekly + dispatch) | re-runs the suite on main; see `docs/security/POSTURE.md` |

## Machine witness (release-gate)

`release-gate.yml` (dispatch-only) re-reads the evidence above for a target
SHA via the GitHub API and emits one machine-readable decision
(`docs/ci/RELEASE_CONTRACT.md` defines the criteria map M1–M9, the human
attestation H1–H4, and the fail-closed rules). It never re-executes tests
and never mints a release.

- **Distinctness:** green PR checks prove merge-readiness only. They are a
  different claim from release qualification, which requires a
  `qualification.yml` run (or `qualify.sh` local equivalent) against a
  fresh empty database — PR checks green ≠ qualified.
- **Necessity, not sufficiency:** a green gate run is *necessary* for a
  release claim and never *sufficient*. `release_decision` is null in
  every gate artifact; the decisive release action is a human merge/tag
  that records the gate run id.

## Release claims

- A release-relevant qualification claim must be backed by
  `qualification.yml` (dispatch-only) or the local equivalent
  (`infra/postgres/qualify.sh`): fresh empty database, anti-fake-green
  gates passed, and — when an LLM matrix is declared — every scenario
  measured (a 429 or skip is never a measurement).
- Before claiming qualification, read `docs/KNOWN_LIMITATIONS.md`: a
  limitation row without evidence, or a claim contradicted by one, blocks
  the release note.
- Findings from any scanner are remediated by reviewed commits; no gate
  auto-fixes or upgrades dependencies.
