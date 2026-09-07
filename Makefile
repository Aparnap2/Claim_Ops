# ClaimOps AI — root task runner.
#
# Repo rule: NO docker-compose, here or anywhere else. No target starts
# containers, services, or background processes, and no target requires a
# live database: integration-gated tests skip gracefully when Postgres or
# Mockoon are absent (exit 0 with a printed skip reason).
#
# Every target echoes the command(s) it runs before running them.

.PHONY: setup test lint format typecheck integration local check

# Extra tool groups needed for Python lint/type/test commands.
UV_RUN := uv run --group dev
# Go module lives here (module claimops-api, go 1.25.6, stdlib-only metrics).
GO_DIR := apps/api
# Integration gates (overridable): Go tests read TEST_POSTGRES_DSN and gate
# Mockoon on http://localhost:3001; both suites t.Skip when unreachable.
TEST_POSTGRES_DSN ?= postgres://claimops_app:claimops_app@localhost:5433/claimops
MOCKOON_URL ?= http://localhost:3001

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

format:
	@echo "format: gofmt -l . in $(GO_DIR), enforcing (empty output = clean)"
	cd $(GO_DIR) && files="$$(gofmt -l .)"; \
		if [ -n "$$files" ]; then echo "gofmt: unformatted files:"; echo "$$files"; exit 1; fi; \
		echo "gofmt: clean"
	@echo "format: ruff format --check . (Python, advisory only — not gated)"
	$(UV_RUN) ruff format --check . || true

typecheck:
	@echo "typecheck: mypy packages (per [tool.mypy] strict in pyproject.toml)"
	$(UV_RUN) mypy packages
	@echo "typecheck: go vet ./... in $(GO_DIR)"
	cd $(GO_DIR) && go vet ./...

integration:
	@echo "integration: postgres + mockoon gated go tests (TEST_POSTGRES_DSN=$(TEST_POSTGRES_DSN) MOCKOON_URL=$(MOCKOON_URL))"
	@echo "integration: probing services (informational only — go tests self-skip when absent)"
	@printf '%s\n' \
		'import os, socket, urllib.parse, urllib.request' \
		"dsn = urllib.parse.urlparse(os.environ['TEST_POSTGRES_DSN'])" \
		"host, port = dsn.hostname or 'localhost', dsn.port or 5433" \
		's = socket.socket(); s.settimeout(2)' \
		"print(f'integration: Postgres {host}:{port} reachable' if s.connect_ex((host, port)) == 0 else f'SKIP: Postgres unreachable at {host}:{port} — postgres-gated tests will self-skip')" \
		's.close()' \
		"url = os.environ['MOCKOON_URL'] + '/v1/policies/POL-001'" \
		'try:' \
		'    r = urllib.request.urlopen(url, timeout=3)' \
		"    print(f'integration: Mockoon {url} reachable (status {r.status})' if r.status == 200 else f'SKIP: Mockoon gate returned status {r.status} — mockoon-gated tests will self-skip')" \
		'except Exception as e:' \
		"    print(f'SKIP: Mockoon unreachable at {url} ({e}) — mockoon-gated tests will self-skip')" \
		| TEST_POSTGRES_DSN="$(TEST_POSTGRES_DSN)" MOCKOON_URL="$(MOCKOON_URL)" python3 -
	@echo "integration: go test on gated packages in $(GO_DIR)"
	cd $(GO_DIR) && TEST_POSTGRES_DSN="$(TEST_POSTGRES_DSN)" go test ./internal/repository/postgres/... ./internal/adapters/... -count=1
	@echo "integration: done (exit 0 — absent services skip, they never fail the target)"

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

check: format lint typecheck test
	@echo "check: format + lint + typecheck + unit tests all passed"
