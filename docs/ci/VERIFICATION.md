# Verification pipeline

One gate definition, three surfaces. The Makefile is canonical; git hooks
and GitHub Actions run the same targets, so a locally green tree is green
in CI by construction — not by coincidence.

## The chain

| Target | What it runs | Surface(s) |
|--------|--------------|------------|
| `make pre-commit` | `format-check` · `ruff check .` · secret-pattern guard · provider-secret-free PR workflows | pre-commit hook, `ci.yml` deterministic |
| `make check` | `format-check` · `lint` · `typecheck` · `test` | pre-push hook, `ci.yml` deterministic |
| `make race-test` | scoped `-race` over the concurrency-sensitive package trees | pre-push hook, `ci.yml` deterministic |
| `make smoke-liteparse` | `TestLiveShimSmoke` with `CLAIMOPS_REQUIRE_LITEPARSE=1` (fails loudly if the shim cannot run) | `ci.yml` deterministic |
| `make integration` | full suite with live services; **strict by default** — a down service fails up front (probe exit 3), and the strict path proves execution (sentinel + APA-83 gates), not just exit 0 | `integration.yml` (services up), local with PG :5433 + Mockoon :3001 |
| `make terraform-check` | `terraform fmt -check` · `init -backend=false` · `validate` | `ci.yml` terraform |

`make format` is the writer (`gofmt -w` + `ruff format .`); `make
format-check` is the enforcer. gofmt violations are hard failures; see the
carve-out below for `ruff format`.

## Surfaces

- **Local / hooks:** `make hooks` symlinks `.git/hooks/pre-commit` and
  `.git/hooks/pre-push` to `scripts/hooks/*.sh`, which `exec make
  pre-commit` / `make pre-push`. Never bypass with `--no-verify`.
- **CI (`ci.yml`):** the `deterministic` job is a thin wrapper — env setup
  (Go 1.27, Python 3.12, `uv sync --group dev`) then the same make targets
  in chain order. `uv sync` precedes `make check` so `TestLiveShimSmoke`
  inside `go test ./...` executes instead of skipping (APA-48);
  `make smoke-liteparse` re-proves it loudly with `REQUIRE=1`.
- **Security (`security.yml` / `security-posture.yml`):** see
  `docs/security/POSTURE.md`.

## Deliberate carve-outs (do not "fix" by gating)

1. **`ruff format` is advisory in `format-check`.** Seven pre-existing
   files are not formatter-clean (e.g. `docs/specs/prd.md`,
   `scripts/ci/check_mockoon_gated_tests.py`,
   `scripts/ci/check_qual_live_build_isolation.py`, three test files,
   `tests/unit/test_workflow_timeout.py`). Gating on them would fail every
   run for pre-existing debt; reformatting them is out of scope for the
   pipeline PR. `ruff check` (lint) IS a hard gate. New files must be
   formatter-clean — `make format` before committing.
2. **The build-isolation guard is CI-only.**
   `scripts/ci/check_qual_live_build_isolation.py` scans the whole tree
   and correctly flags untracked scratch files (e.g. a live-provider test
   without a build tag) that exist only on developer working trees. It
   runs as its own `ci.yml` step on the clean checkout, never inside
   `make pre-commit`.
3. **`make integration` is strict by default — and strict proves execution.**
   The probe (`scripts/ci/integration_probe.py`) exits 3 when a service is
   down and the Makefile fails the target before any test runs.
   `INTEGRATION_ALLOW_DEGRADED=1` restores the old skip-and-pass behaviour
   for explicitly degraded local runs (loud banner naming the down
   services); CI never sets it.

   Failure signals, by mode:

   | Signal | Strict (default) | Degraded (`INTEGRATION_ALLOW_DEGRADED=1`) |
   |--------|------------------|--------------------------------------------|
   | Service down (probe) | target FAILS before any test runs | banner `DEGRADED RUN — services unavailable, tests skipped: <list>`; target passes |
   | Service reachable but unusable (wrong creds, wrong DB, TCP-only proxy) | sentinel `TestIntegrationSentinelPostgresRoundTrip` FAILS the suite — armed by `TEST_POSTGRES_STRICT=1`, it does a real connect+DDL+insert+select round-trip and never skips | gated tests self-skip; target passes |
   | Services healthy but zero gated tests ran | APA-83 post-run gates fail: `check_pg_gated_packages.py` requires every gated package AND the sentinel by name as `--- PASS`; `check_mockoon_gated_tests.py` requires the named Mockoon tests | no execution assertions (the banner says so) |
   | Everything green | `PASS: all 10 Postgres-gated packages executed tests (PG-gated tests executed: N passed, M skipped)` | `done (DEGRADED — no execution assertions)` |

   The sentinel is inert without `TEST_POSTGRES_STRICT=1`, so `make test` /
   `make check` (no services) and the deterministic CI job stay green.

## Local quick reference

```sh
make hooks          # install git hooks (once per clone)
make pre-commit     # what the pre-commit hook runs
make check          # what the pre-push hook runs (check + race-test)
make integration    # strict; needs PG :5433 + Mockoon :3001 up
make terraform-check
```
