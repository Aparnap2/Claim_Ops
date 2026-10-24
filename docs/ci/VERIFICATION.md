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
| `make integration` | full suite with live services; **strict by default** — a down service fails the job (probe exit 3), it does not skip-and-pass | `integration.yml` (services up), local with PG :5433 + Mockoon :3001 |
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
3. **`make integration` is strict by default.** The probe
   (`scripts/ci/integration_probe.py`) exits 3 when a service is down and
   the Makefile fails the target — a green integration run always proves
   the services layer delivered. `INTEGRATION_ALLOW_DEGRADED=1` restores
   the old skip-and-pass behaviour for explicitly degraded local runs;
   CI never sets it.

## Local quick reference

```sh
make hooks          # install git hooks (once per clone)
make pre-commit     # what the pre-commit hook runs
make check          # what the pre-push hook runs (check + race-test)
make integration    # strict; needs PG :5433 + Mockoon :3001 up
make terraform-check
```
