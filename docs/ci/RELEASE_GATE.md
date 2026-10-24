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
