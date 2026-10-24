# ClaimOps AI — root task runner.
#
# Repo rule: NO docker-compose, here or anywhere else. No target starts
# containers, services, or background processes, and no target requires a
# live database by default: `integration` is STRICT — it fails when Postgres
# or Mockoon are unreachable (set INTEGRATION_ALLOW_DEGRADED=1 to restore the
# old skip-and-pass behavior for quick local runs).
#
# ONE CARVE-OUT (APA-53): `qualify` is the sole target that starts a
# container. It exists precisely because the rules above make qualification
# impossible: `test`/`integration` gate on a LONG-LIVED Postgres whose volume
# accumulates rows across runs, so several PG-gated tests assert over whatever
# a tenant-scoped query returns and their verdicts depend on the volume's
# history. `qualify` does not make any existing target depend on a container;
# it provisions its OWN throwaway Postgres on a different port, migrates it,
# runs the suite, and tears it down on the way out (success or failure). It
# never touches `claimops-postgres` — see infra/postgres/qualify.sh.
#
# Canonical gate chain (one definition, three surfaces — hooks, CI, manual):
#   make pre-commit   fast: format-check + ruff lint + secret/credential guards
#   make check        full: format-check + lint + typecheck + unit tests
#   make pre-push     check + race-test
# Git hooks delegate to these targets (install with `make hooks`); the
# ci.yml `deterministic` job runs the same targets. See docs/ci/VERIFICATION.md.
#
# Every target echoes the command(s) it runs before running them.

.PHONY: setup test lint format format-check typecheck integration local \
	check qualify pre-commit pre-push race-test smoke-liteparse \
	terraform-check hooks

# Extra tool groups needed for Python lint/type/test commands.
UV_RUN := uv run --group dev
# Go module lives here (module claimops-api, go 1.27, stdlib-only metrics).
GO_DIR := apps/api
# Integration gates (overridable): Go tests read TEST_POSTGRES_DSN and gate
# Mockoon on http://localhost:3001; both suites t.Skip when unreachable.
TEST_POSTGRES_DSN ?= postgres://claimops_app:claimops_app@localhost:5433/claimops
MOCKOON_URL ?= http://localhost:3001
# Strict-mode escape hatch for `integration` (see target below).
INTEGRATION_ALLOW_DEGRADED ?= 0

setup:
	@echo "setup: uv sync --group dev"
	uv sync --group dev
	@echo "setup: go mod tidy in $(GO_DIR)"
	cd $(GO_DIR) && go mod tidy

test:
	@echo "test: pytest tests/unit -q"
	$(UV_RUN) pytest tests/unit -q
	@echo "test: go test ./... in $(GO_DIR)"
	cd $(GO_DIR) && go test ./...

lint:
	@echo "lint: ruff check ."
	$(UV_RUN) ruff check .
	@echo "lint: go vet ./... in $(GO_DIR)"
	cd $(GO_DIR) && go vet ./...
	@echo "lint: gofmt -l . in $(GO_DIR) (empty output = clean)"
	cd $(GO_DIR) && files="$$(gofmt -l .)"; \
		if [ -n "$$files" ]; then echo "gofmt: unformatted files:"; echo "$$files"; exit 1; fi; \
		echo "gofmt: clean"

# Writer targets actually rewrite files; format-check is the enforcing gate.
format:
	@echo "format: gofmt -w . in $(GO_DIR) (writes)"
	cd $(GO_DIR) && gofmt -w .
	@echo "format: ruff format . (writes; advisory surface — a few pre-existing files incl. prd.md are not format-clean, see docs/ci/VERIFICATION.md)"
	$(UV_RUN) ruff format .

format-check:
	@echo "format-check: gofmt -l . in $(GO_DIR), enforcing (empty output = clean)"
	cd $(GO_DIR) && files="$$(gofmt -l .)"; \
		if [ -n "$$files" ]; then echo "gofmt: unformatted files:"; echo "$$files"; exit 1; fi; \
		echo "gofmt: clean"
	@echo "format-check: ruff format --check . (advisory only — not gated; pre-existing unformatted files are tracked in docs/ci/VERIFICATION.md)"
	$(UV_RUN) ruff format --check . || true

typecheck:
	@echo "typecheck: mypy packages scripts/ci (per [tool.mypy] strict in pyproject.toml)"
	$(UV_RUN) mypy packages scripts/ci
	@echo "typecheck: go vet ./... in $(GO_DIR)"
	cd $(GO_DIR) && go vet ./...

# Strict by default: a service being down is an ERROR, not a silent skip.
# The old skip-and-pass behavior (green with zero gated tests run) is the
# explicit degraded escape: INTEGRATION_ALLOW_DEGRADED=1. CI (integration.yml)
# runs strict — its services are provisioned as preconditions.
#
# Strict mode proves EXECUTION, not just exit 0:
#   1. the probe fails the target up front when a service is down;
#   2. the suite runs with TEST_POSTGRES_STRICT=1, which arms the sentinel
#      test (internal/repository/postgres/sentinel_test.go) — a real
#      connect+DDL+insert+select round-trip that FAILS (never skips) when
#      the DSN points at a reachable-but-unusable Postgres (wrong creds,
#      wrong database, TCP-only proxy);
#   3. post-run, the APA-83 gates re-run the gated packages with -v and
#      require every Postgres-gated package (and the sentinel by name) plus
#      the named Mockoon-gated tests to appear as --- PASS.
# Degraded mode runs the suite WITHOUT the strict signal (gated tests
# self-skip when their service is down), prints a loud banner naming the
# down services, and performs no execution assertions — it proves nothing
# and is never set in CI.
integration:
	@echo "integration: postgres + mockoon gated go tests (TEST_POSTGRES_DSN=$(TEST_POSTGRES_DSN) MOCKOON_URL=$(MOCKOON_URL))"
	@if [ "$(INTEGRATION_ALLOW_DEGRADED)" = "1" ]; then \
		down="$$(TEST_POSTGRES_DSN='$(TEST_POSTGRES_DSN)' MOCKOON_URL='$(MOCKOON_URL)' python3 scripts/ci/integration_probe.py --down-list)"; \
		if [ -n "$$down" ]; then \
			echo "----------------------------------------------------------------------"; \
			echo "DEGRADED RUN — services unavailable, tests skipped: $$down"; \
			echo "This pass proves NOTHING about service-backed behavior."; \
			echo "----------------------------------------------------------------------"; \
		else \
			echo "integration: degraded mode requested but all services reachable — gated tests will run for real"; \
		fi; \
		echo "integration: go test on gated packages in $(GO_DIR) (degraded: gated tests self-skip when their service is down)"; \
		( cd $(GO_DIR) && TEST_POSTGRES_DSN="$(TEST_POSTGRES_DSN)" go test ./internal/repository/postgres/... ./internal/adapters/... -count=1 ); \
		rc=$$?; \
		echo "integration: done (DEGRADED — no execution assertions; suite exit $$rc)"; \
		exit $$rc; \
	else \
		echo "integration: strict mode — probing services (absent service = FAIL)"; \
		if TEST_POSTGRES_DSN="$(TEST_POSTGRES_DSN)" MOCKOON_URL="$(MOCKOON_URL)" python3 scripts/ci/integration_probe.py; then \
			echo "integration: services up — running gated suites"; \
		else \
			rc=$$?; \
			echo "integration: FAIL — a service is down (probe exit $$rc). Start services (see 'make local') or set INTEGRATION_ALLOW_DEGRADED=1."; \
			exit 1; \
		fi; \
		echo "integration: go test on gated packages in $(GO_DIR) (TEST_POSTGRES_STRICT=1 arms the sentinel)"; \
		( cd $(GO_DIR) && TEST_POSTGRES_DSN="$(TEST_POSTGRES_DSN)" TEST_POSTGRES_STRICT=1 go test ./internal/repository/postgres/... ./internal/adapters/... -count=1 ); \
		rc=$$?; \
		if [ $$rc -ne 0 ]; then \
			echo "integration: FAIL — gated suite exited $$rc (sentinel or a real test failure; see output above)"; \
			exit $$rc; \
		fi; \
		echo "integration: post-run proof that gated tests executed (APA-83 gates)"; \
		TEST_POSTGRES_DSN="$(TEST_POSTGRES_DSN)" TEST_POSTGRES_STRICT=1 python3 scripts/ci/check_pg_gated_packages.py || exit $$?; \
		MOCKOON_URL="$(MOCKOON_URL)" TEST_POSTGRES_DSN="$(TEST_POSTGRES_DSN)" TEST_POSTGRES_STRICT=1 python3 scripts/ci/check_mockoon_gated_tests.py || exit $$?; \
		echo "integration: done (strict — execution proven, not assumed)"; \
	fi

local:
	@echo "local: service map (this target starts nothing — copy/paste only)"
	@echo "  API      :8000 — cd apps/api && PORT=8000 go run ./cmd/api"
	@echo "  Postgres :5433 — TEST_POSTGRES_DSN=postgres://claimops_app:claimops_app@localhost:5433/claimops"
	@echo "  Mockoon  :3001 — serve mocks/mockoon/claims-systems.json; go tests gate on http://localhost:3001/v1/policies/POL-001"
	@echo "  localgcp — localgcp up (Pub/Sub :8085, Storage :4443); then 'eval $$(localgcp env)'"
	@echo "local: start services manually (no docker-compose per repo rule); then run 'make integration'"

localgcp-test:
	@echo "localgcp-test: emulator-gated transport suites (skip, never fail, when emulator down)"
	@printf '%s\n' \
		'import socket' \
		's = socket.socket(); s.settimeout(2)' \
		"print('localgcp-test: Pub/Sub :8085 reachable' if s.connect_ex(('localhost', 8085)) == 0 else 'SKIP: Pub/Sub emulator down — emulator tests will self-skip')" \
		's.close()' \
		| python3 -
	@printf '%s\n' \
		'import urllib.request' \
		'try:' \
		'    r = urllib.request.urlopen("http://localhost:4443", timeout=3)' \
		"    print('localgcp-test: Storage :4443 reachable')" \
		'except Exception as e:' \
		"    print(f'SKIP: Storage emulator down ({e}) — emulator tests will self-skip')" \
		| python3 -
	@echo "localgcp-test: go test transport suites in $(GO_DIR)"
	cd $(GO_DIR) && PUBSUB_EMULATOR_HOST=localhost:8085 STORAGE_EMULATOR_HOST=localhost:4443 TEST_POSTGRES_DSN="$(TEST_POSTGRES_DSN)" go test ./internal/adapters/pubsubadapter/ ./internal/adapters/gcsblob/ ./internal/ingest/ ./internal/outbox/ -count=1
	@echo "localgcp-test: done (exit 0)"

check: format-check lint typecheck test
	@echo "check: format-check + lint + typecheck + unit tests all passed"

# Fast pre-commit gate (also run by the ci.yml deterministic job and by the
# git hook installed via `make hooks`). Deliberately fast: no go build/test.
# Whole-tree structural guards (e.g. check_qual_live_build_isolation.py) stay
# in CI only: they run on a clean checkout and false-positive on untracked
# scratch files in a developer's working tree.
pre-commit:
	@echo "pre-commit: format-check (gofmt enforced, ruff format advisory)"
	$(MAKE) --no-print-directory format-check
	@echo "pre-commit: ruff check ."
	$(UV_RUN) ruff check .
	@echo "pre-commit: secret-pattern guard (shared with the security.yml static job)"
	scripts/ci/check_secret_patterns.sh
	@echo "pre-commit: live-provider credential guard (pull_request workflows)"
	python3 scripts/ci/check_live_provider_workflow_secrets.py

# Full pre-push gate (also run by the git hook installed via `make hooks`).
pre-push:
	@echo "pre-push: full check gate (format-check + lint + typecheck + unit tests)"
	$(MAKE) --no-print-directory check
	@echo "pre-push: race detector on concurrency-sensitive packages"
	$(MAKE) --no-print-directory race-test

# Race detector over the packages that actually share mutable state
# (mirrors the ci.yml deterministic job's scoped -race set; live-provider
# suites stay excluded by build tag and are not compiled here).
race-test:
	@echo "race-test: go test -race -count=1 on concurrency-sensitive packages in $(GO_DIR)"
	cd $(GO_DIR) && go test -race -count=1 \
		./internal/investigate/... \
		./internal/ingest/... \
		./internal/workflow/... \
		./internal/worker/... \
		./internal/ports/... \
		./internal/observability/... \
		./internal/metrics/... \
		./internal/adapters/memblob/...

# APA-48: the LiteParse live shim smoke as a standalone, loud gate. Requires
# the Python env from `make setup` (the shim resolves .venv/bin/python at the
# repo root); CLAIMOPS_REQUIRE_LITEPARSE=1 turns a broken parser environment
# into a hard failure instead of a skip.
smoke-liteparse:
	@echo "smoke-liteparse: live shim smoke (fails if parser env is broken; run 'make setup' first)"
	cd $(GO_DIR) && CLAIMOPS_REQUIRE_LITEPARSE=1 go test ./internal/parser/liteparse/ -run TestLiveShimSmoke -count=1 -v

# Terraform gate (mirrors the ci.yml terraform job).
terraform-check:
	@echo "terraform-check: fmt -check -recursive + init -backend=false + validate in infra/terraform"
	command -v terraform >/dev/null 2>&1 || { echo "terraform-check: terraform not on PATH"; exit 1; }
	cd infra/terraform && terraform fmt -check -recursive
	cd infra/terraform && terraform init -backend=false && terraform validate

# Install git hooks as symlinks to scripts/hooks/*.sh. The hooks delegate to
# `make pre-commit` / `make pre-push` so the gate is defined exactly once.
hooks:
	@echo "hooks: installing git hooks (symlinks to scripts/hooks/*.sh)"
	ln -sf ../../scripts/hooks/pre-commit.sh .git/hooks/pre-commit
	ln -sf ../../scripts/hooks/pre-push.sh .git/hooks/pre-push
	@echo "hooks: installed — .git/hooks/pre-commit, .git/hooks/pre-push"

# APA-53 — qualification gate. Runs the WHOLE PG-gated suite against a
# genuinely fresh, ephemeral database (own container, own port, no volume),
# then tears the container down deterministically. Leaves `claimops-postgres`
# and its volume untouched.
#
# Unlike `integration`, this target FAILS rather than skipping when Postgres
# cannot provide a real, empty, migrated database, and it fails if every
# PG-backed test self-skipped — green-by-skipping is not a qualification.
#
# Overridable: QUAL_PG_PORT (default 55432), QUAL_PG_IMAGE, QUAL_GO_DIR,
# GO_TEST_FLAGS, QUAL_PG_KEEP=1 (leave the container up for triage).
qualify:
	@echo "qualify: ephemeral Postgres (docker run, no compose) + migrations + full PG-gated suite"
	@echo "qualify: the shared claimops-postgres volume is never read, written, or stopped"
	infra/postgres/qualify.sh
	@echo "qualify: done — suite green against a genuinely fresh database"
