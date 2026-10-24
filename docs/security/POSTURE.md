# Security posture

How ClaimOps proves its security posture, and where each proof runs.
Findings are remediated by reviewed commits; no gate auto-fixes or
upgrades dependencies.

## Gates

| Gate | What it proves | Where it runs |
|------|----------------|---------------|
| Secret-pattern guard | No committed-secret shapes in the tree | `make pre-commit` (hooks + CI), `security.yml` `static`, `security-posture.yml` |
| Live-provider build-tag isolation | Live-provider tests cannot run from the default build | `ci.yml` deterministic, `security-posture.yml` |
| Provider-secret-free PR workflows | PR-triggered workflows never reference provider API keys | `ci.yml` deterministic, `security-posture.yml` |
| go mod verify | Module cache matches `go.sum` (integrity) | `security.yml` `static`, `security-posture.yml` |
| uv pip check | Python env dependency-consistent | `security.yml` `static`, `security-posture.yml` |
| govulncheck | No reachable Go CVEs | `security.yml` `govulncheck`, `security-posture.yml` |
| pip-audit | No known Python CVEs in the locked set | `security.yml` `pip-audit`, `security-posture.yml` |
| pip-audit delta | PR introduces no NEW Python CVEs | `security.yml` `security-delta` (PRs only) |
| GitGuardian | Secret detection incl. history | GitHub repo check (external, not duplicated here) |

## Triggers

- **Every PR + push to main:** `security.yml` (full suite + PR delta).
- **Weekly (Mon 06:20 UTC) + on demand:** `security-posture.yml` re-runs the
  suite on main and records a dated summary, so drift no PR touches is
  still caught.
- **Local:** `make pre-commit` runs the secret guard; the CVE scanners are
  CI-only (network-dependent advisory databases).

## Reading results

- A scanner job failing IS the report: findings are listed in the job log.
  Remediation is a separate, reviewed commit (e.g. #184: pypdf>=6.21.2).
- `security-delta` exit 2 = indeterminate (pip-audit could not produce a
  usable report): re-run; do not merge blindly.
- The delta gate fails only on head-only advisories; pre-existing findings
  belong to the `pip-audit` job.
- gitleaks is deliberately not run: GitGuardian covers secret history, and
  offline gitleaks without rules/network adds noise, not signal.

## Known state

- 2026-10-24: govulncheck clean (0 reachable; 2 imported-package and
  2 module-level advisories uncalled by this code). pip-audit clean over
  the locked set (pypdf 6.21.2 per #184). Secret-pattern guard clean.
  Scheduled posture runs keep this section's claims current; a stale
  section is a finding, not a fact.
