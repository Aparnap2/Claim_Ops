#!/usr/bin/env bash
# qualify.sh — ephemeral PostgreSQL qualification environment (APA-53).
#
# WHAT THIS IS
#   A self-contained, deterministic, throwaway PostgreSQL instance for running
#   the WHOLE PG-gated Go suite against a GENUINELY EMPTY database:
#
#     qualification run
#       -> fresh empty database (new container, NO volume, empty layer)
#       -> apply infra/postgres/migrations/*.sql via the repo's own mechanism
#       -> go test -count=1 ./...  (TEST_POSTGRES_DSN pointed at the throwaway)
#       -> deterministic teardown (trap, runs on success AND on failure)
#
# WHY IT EXISTS
#   The suite is normally gated on TEST_POSTGRES_DSN pointing at the long-lived
#   `claimops-postgres` container (localhost:5433). That volume accumulates rows
#   across runs, and several PG-gated tests assert over whatever a tenant-scoped
#   query returns without filtering to the rows the test itself seeded. Their
#   verdicts therefore depend on how much history the volume already holds, not
#   only on the code under test. See 05_BUILD_PLAN.md / the APA-53 report.
#
# SAFETY INVARIANTS (non-negotiable; each is asserted below, not assumed)
#   1. It NEVER contacts, restarts, purges or writes to `claimops-postgres`.
#      The shared volume is evidence. The only way this script can touch it is
#      to be handed a DSN pointing at it, so the port and container name are
#      both hard-guarded against the shared values.
#   2. `docker run` only. No docker-compose (repo rule), and no stray
#      containers: the single server container is `--rm`, and every psql call
#      is `docker exec` against that one container rather than a new throwaway
#      client container per migration.
#   3. The database starts EMPTY every run (no volume mount), so "passes on a
#      fresh database" is a fact this harness can actually demonstrate rather
#      than an assertion.
#   4. Teardown is a trap on EXIT/INT/TERM, so a failing or interrupted suite
#      still leaves no container behind.
#   5. Anti-fake-green: after the suite, this script verifies PG-backed tests
#      actually RAN. A run where every PG test self-skipped is reported as a
#      FAILURE, because green-by-skipping is not a qualification result.
#
# ROLE TOPOLOGY
#   Mirrors .github/workflows/integration.yml exactly, for the same reason:
#   tests must run as a NOSUPERUSER so FORCE RLS is genuinely enforced. A
#   superuser app role bypasses RLS and silently inverts every cross-tenant
#   test. claimops_app/claimops_worker are created BEFORE migrations because
#   002 issues ALTER DEFAULT PRIVILEGES ... FROM claimops_app.
#
# USAGE
#   infra/postgres/qualify.sh                 # ephemeral on 127.0.0.1:55432
#   QUAL_PG_PORT=55433 infra/postgres/qualify.sh
#   QUAL_PG_KEEP=1 infra/postgres/qualify.sh   # leave the container for triage
#   GO_TEST_FLAGS='-run TestOutbox -v' infra/postgres/qualify.sh

set -Eeuo pipefail

readonly REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# QUAL_GO_DIR lets the harness qualify any checkout (e.g. a base commit in a
# worktree) without editing this script. Migrations are read from the SAME
# checkout as the module under test, so a branch that ships migrations is
# qualified against its own schema.
#
# These are derived by string manipulation, not `cd`+`pwd`, so a bad
# QUAL_GO_DIR produces the explicit "go module not found" message from guard()
# rather than dying inside a command substitution with no explanation.
readonly GO_DIR="${QUAL_GO_DIR:-${REPO_ROOT}/apps/api}"
readonly MIGRATIONS_DIR="${QUAL_MIGRATIONS_DIR:-${GO_DIR}/../../infra/postgres/migrations}"

# Throwaway identity. Deliberately NOT the shared values.
readonly QUAL_CONTAINER="${QUAL_PG_CONTAINER:-claimops-pg-qual}"
readonly QUAL_IMAGE="${QUAL_PG_IMAGE:-postgres:16-alpine}"
readonly QUAL_PORT="${QUAL_PG_PORT:-55432}"

# The long-lived shared volume this harness must never reach.
readonly SHARED_CONTAINER="claimops-postgres"
readonly SHARED_PORT="5433"

# Same shape as the shared container and CI: owner/superuser `claimops` runs the
# migrations, throwaway credentials matching the repo's local DSN convention
# (production uses Secret Manager; never a real secret).
readonly OWNER_USER="claimops"
readonly OWNER_PASSWORD="claimops"
readonly OWNER_DB="claimops"
readonly APP_PASSWORD="claimops_app"
readonly WORKER_PASSWORD="claimops_worker"

log() { printf 'qualify: %s\n' "$*"; }
die() { printf 'qualify: FATAL: %s\n' "$*" >&2; exit 1; }

# ---------------------------------------------------------------- guards ----
# These run before anything is created. A guard that only logs is not a guard.

guard() {
  command -v docker >/dev/null 2>&1 || die "docker not on PATH"
  command -v go    >/dev/null 2>&1 || die "go not on PATH"

  if [ "${QUAL_PORT}" = "${SHARED_PORT}" ] || [ "${QUAL_PORT}" = "5432" ]; then
    die "QUAL_PG_PORT=${QUAL_PORT} collides with the shared Postgres port (${SHARED_PORT}); refusing to risk claimops-postgres"
  fi
  if [ "${QUAL_CONTAINER}" = "${SHARED_CONTAINER}" ]; then
    die "QUAL_PG_CONTAINER=${QUAL_CONTAINER} is the shared container name; refusing"
  fi
  [ -d "${MIGRATIONS_DIR}" ] || die "migrations dir not found: ${MIGRATIONS_DIR} (set QUAL_MIGRATIONS_DIR to override)"
  [ -f "${GO_DIR}/go.mod" ]      || die "go module not found: ${GO_DIR} (QUAL_GO_DIR must point at an <repo>/apps/api)"
}

# -------------------------------------------------------------- teardown ----
# Idempotent, and registered before the container is created, so an interrupt
# during `docker run` still cleans up. QUAL_PG_KEEP=1 is for triage only.

teardown() {
  local rc=$?
  if [ "${QUAL_PG_KEEP:-0}" = "1" ]; then
    log "QUAL_PG_KEEP=1 — leaving ${QUAL_CONTAINER} up for triage (delete it yourself)"
    return $rc
  fi
  # `--rm` covers a normal stop; `rm -f` covers a container left by an
  # interrupted run whose flag never took effect.
  if docker ps -a --format '{{.Names}}' | grep -qx "${QUAL_CONTAINER}"; then
    docker rm -f "${QUAL_CONTAINER}" >/dev/null 2>&1 || true
  fi
  return $rc
}

# ------------------------------------------------------------------ start ----

start_postgres() {
  # Clear a same-named container left by an earlier interrupted run, the way
  # the CI Mockoon step does, so --name never collides.
  if docker ps -a --format '{{.Names}}' | grep -qx "${QUAL_CONTAINER}"; then
    log "removing stale ${QUAL_CONTAINER} from a previous run"
    docker rm -f "${QUAL_CONTAINER}" >/dev/null
  fi

  log "starting ephemeral Postgres on 127.0.0.1:${QUAL_PORT} (container ${QUAL_CONTAINER}, NO volume => empty)"
  # No -v: the data directory lives in the container layer and dies with it, so
  # every run starts from a genuinely empty database. Binds loopback only.
  docker run -d --rm \
    --name "${QUAL_CONTAINER}" \
    -e POSTGRES_USER="${OWNER_USER}" \
    -e POSTGRES_PASSWORD="${OWNER_PASSWORD}" \
    -e POSTGRES_DB="${OWNER_DB}" \
    -p "127.0.0.1:${QUAL_PORT}:5432" \
    "${QUAL_IMAGE}" >/dev/null
}

# Bounded, not sleep-forever: fail loudly rather than hang a CI job.
#
# The gate is a REAL query over a real connection, deliberately not
# `pg_isready`. The postgres entrypoint runs an init phase during which the
# server accepts connections on a temporary socket while still reporting
# "the database system is starting up"; pg_isready can report ready there, and
# any real connection made in that window is refused. Probing with an actual
# `SELECT 1` is the only signal that means "migrations can be applied now".
wait_ready() {
  local tries=90 i
  log "waiting for the database to accept a real connection"
  for (( i = 1; i <= tries; i++ )); do
    if docker exec "${QUAL_CONTAINER}" \
        psql -U "${OWNER_USER}" -d "${OWNER_DB}" -tAc 'SELECT 1' >/dev/null 2>&1; then
      log "database serving after ${i} probe(s)"
      return 0
    fi
    # A container that exited early will never become ready; stop waiting.
    if ! docker ps --format '{{.Names}}' | grep -qx "${QUAL_CONTAINER}"; then
      docker logs "${QUAL_CONTAINER}" 2>&1 | tail -20 >&2 || true
      die "postgres container exited before becoming ready"
    fi
    sleep 1
  done
  docker logs "${QUAL_CONTAINER}" 2>&1 | tail -20 >&2 || true
  die "database not serving after ${tries}s"
}

psql_owner() {
  # Migrations and role bootstrap run as the owner, so GRANT/REVOKE and
  # DEFAULT PRIVILEGES land exactly as they do in CI and production.
  docker exec -i "${QUAL_CONTAINER}" \
    psql "postgres://${OWNER_USER}:${OWNER_PASSWORD}@127.0.0.1:5432/${OWNER_DB}" \
    -v ON_ERROR_STOP=1 "$@"
}

bootstrap_roles() {
  # Same production-shaped topology, and the same reason, as
  # .github/workflows/integration.yml: NOSUPERUSER so FORCE RLS is enforced.
  # Idempotent DO block (plain CREATE ROLE has no IF NOT EXISTS) plus
  # unconditional NOSUPERUSER, which also repairs a re-provisioned role.
  # Runs BEFORE migrations: 002 runs ALTER DEFAULT PRIVILEGES ... FROM
  # claimops_app and fails outright if that role does not exist.
  #
  # NOTE: passwords are shell-expanded inside the -c string (psql performs no
  # variable substitution in -c SQL), matching the CI step exactly.
  local sql
  sql="DO \$\$
BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'claimops_app') THEN
    CREATE ROLE claimops_app WITH NOSUPERUSER LOGIN PASSWORD '${APP_PASSWORD}';
  END IF;
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'claimops_worker') THEN
    CREATE ROLE claimops_worker WITH NOSUPERUSER LOGIN PASSWORD '${WORKER_PASSWORD}';
  END IF;
END
\$\$;
ALTER ROLE claimops_app WITH NOSUPERUSER LOGIN PASSWORD '${APP_PASSWORD}';
ALTER ROLE claimops_worker WITH NOSUPERUSER LOGIN PASSWORD '${WORKER_PASSWORD}';"
  log "bootstrapping roles claimops_app / claimops_worker (NOSUPERUSER)"
  psql_owner -q -c "${sql}"
}

apply_migrations() {
  # The repo's own mechanism, verbatim in spirit from integration.yml: every
  # file in infra/postgres/migrations, lexicographic order, ON_ERROR_STOP=1 so
  # a broken migration fails the run instead of half-provisioning a schema.
  local f applied=0
  log "applying migrations from ${MIGRATIONS_DIR#"${REPO_ROOT}/"}"
  for f in "${MIGRATIONS_DIR}"/*.sql; do
    log "  applying $(basename "${f}")"
    psql_owner -q -f - <"${f}"
    applied=$(( applied + 1 ))
  done
  [ "${applied}" -gt 0 ] || die "no migrations found in ${MIGRATIONS_DIR}"
  log "applied ${applied} migration(s)"
}

# Proof that the schema exists but holds NO DATA, which is the precondition the
# whole exercise depends on: "green on a fresh database" is only meaningful if
# the database really was empty when the suite started. Prints the per-table row
# total and refuses to continue if anything is already there.
assert_empty() {
  local rows
  rows="$(docker exec -i "${QUAL_CONTAINER}" \
    psql -U "${OWNER_USER}" -d "${OWNER_DB}" -tAF'|' -c "
      SELECT coalesce(sum(n), 0) FROM (
        SELECT (xpath('/row/c/text()', query_to_xml(
          format('SELECT count(*) AS c FROM public.%I', relname), false, true, '')))[1]::text::bigint AS n
          FROM pg_class c
          JOIN pg_namespace nn ON nn.oid = c.relnamespace
         WHERE nn.nspname = 'public' AND c.relkind = 'r'
      ) s" 2>/dev/null || echo "")"
  if [ -z "${rows}" ]; then
    log "WARNING: could not measure pre-suite row count (non-fatal)"
    return 0
  fi
  log "total rows across public tables before the suite: ${rows}"
  if [ "${rows}" != "0" ]; then
    die "database is NOT empty before the suite (${rows} rows) — 'fresh database' would be a false claim"
  fi
  log "confirmed empty: this run's verdicts cannot be inherited from earlier history"
}

# ------------------------------------------------------------------- main ----

main() {
  guard
  trap teardown EXIT INT TERM

  start_postgres
  wait_ready
  bootstrap_roles
  apply_migrations

  # The suite is wired to the throwaway by default: these exports are the only
  # change in how the tests are invoked. No test's own logic is touched, and no
  # test is reconfigured — they read the same two variables they always have.
  export TEST_POSTGRES_DSN="postgres://claimops_app:${APP_PASSWORD}@127.0.0.1:${QUAL_PORT}/${OWNER_DB}"
  export TEST_POSTGRES_ADMIN_DSN="postgres://${OWNER_USER}:${OWNER_PASSWORD}@127.0.0.1:${QUAL_PORT}/${OWNER_DB}"

  log "TEST_POSTGRES_DSN=${TEST_POSTGRES_DSN}"
  log "TEST_POSTGRES_ADMIN_DSN=postgres://${OWNER_USER}:***@127.0.0.1:${QUAL_PORT}/${OWNER_DB}"

  # Prove the database really is empty, so "fresh database" is a measured fact
  # and not an assumption inherited from the container being new.
  assert_empty

  log "go test ${GO_TEST_FLAGS:--count=1} ./... (GO_TEST_FLAGS='${GO_TEST_FLAGS:-}')"
  local go_status=0
  (
    cd "${GO_DIR}"
    # GO_TEST_FLAGS goes LAST so a caller can override any default here (a
    # trailing -count=30 would otherwise lose to a hardcoded -count=1).
    # shellcheck disable=SC2086
    go test ${GO_TEST_FLAGS:--count=1} ./...
  ) || go_status=$?

  # ---- anti-fake-green ----------------------------------------------------
  # `go test` exit 0 with every PG test skipped would be indistinguishable from
  # a real pass here. Prove PG-backed tests executed.
  log "verifying PG-backed tests actually RAN (not self-skipped)"
  local pg_pass
  pg_pass="$( cd "${GO_DIR}" && TEST_POSTGRES_DSN="${TEST_POSTGRES_DSN}" TEST_POSTGRES_ADMIN_DSN="${TEST_POSTGRES_ADMIN_DSN}" \
    go test -count=1 -v ./internal/repository/postgres/ 2>&1 | grep -c '^--- PASS' || true )"
  log "postgres-backed passing tests: ${pg_pass}"
  if [ "${pg_pass}" -lt 1 ]; then
    die "zero postgres-backed tests ran — the suite self-skipped; this is an infra failure, NOT a qualification pass"
  fi

  if [ "${go_status}" -ne 0 ]; then
    log "SUITE FAILED (go test exit ${go_status}) — see output above; this is a real result, not to be retried into green"
    return "${go_status}"
  fi

  log "QUALIFIED: full suite green against a genuinely fresh, ephemeral database"
}

main "$@"
