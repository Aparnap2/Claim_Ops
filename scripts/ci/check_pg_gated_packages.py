#!/usr/bin/env python3
"""Fail closed unless every PostgreSQL-gated package actually executed tests.

Why this exists: the integration layer's previous anti-fake-green guard counted
`--- PASS` lines for `internal/repository/postgres` only. Eight other packages
gate on TEST_POSTGRES_DSN and self-skip when it is absent, so a run in which
Postgres never came up could still satisfy a single aggregate count drawn from
unrelated, non-Postgres tests in a busy package.

Process exit 0 proves nothing on its own — a package whose entire suite skipped
still exits 0. This check reads the verbose per-test output and requires each
required package to have run at least `MIN_PASSES` tests, so a green result
means DB-backed tests executed rather than merely that the command finished.
"""

from __future__ import annotations

import os
import re
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
API = ROOT / "apps" / "api"

# Every package that gates on TEST_POSTGRES_DSN. Measured on main@93219b1: with
# Postgres absent these packages self-skip (60 skips total across the set);
# with Postgres present the Postgres-backed tests pass. Enumerated explicitly so
# a newly gated package cannot silently join the set without a decision here.
REQUIRED_PACKAGES: tuple[str, ...] = (
    "./cmd/agent",
    "./internal/app",
    "./internal/handlers",
    "./internal/ingest",
    "./internal/investigate",
    "./internal/investigate/orchestrate",
    "./internal/investigate/tools",
    "./internal/repository/postgres",
    "./internal/worker",
    "./internal/workflow",
)

# One executed test is the floor: it distinguishes "ran" from "all skipped".
# The Postgres repository package is the canary — its entire suite is
# Postgres-backed, so it reports 0 passes when the service is absent.
MIN_PASSES = 1

PASS_RE = re.compile(r"^--- PASS: (\S+)", re.MULTILINE)
SKIP_RE = re.compile(r"^--- SKIP: (\S+)", re.MULTILINE)


def count_results(output: str) -> tuple[list[str], list[str]]:
    """Return (passed, skipped) test names from `go test -v` output."""
    return PASS_RE.findall(output), SKIP_RE.findall(output)


def run_package(package: str, env: dict[str, str]) -> tuple[list[str], list[str]]:
    result = subprocess.run(
        ["go", "test", "-v", "-count=1", package],
        cwd=API,
        env=env,
        check=False,
        text=True,
        capture_output=True,
    )
    # Intentionally examine stdout+stderr regardless of exit code: a build or
    # compile error must also be reported, not silently read as "zero passes
    # because nothing ran" with no explanation.
    return count_results(result.stdout + result.stderr)


def underrun_packages(pass_counts: dict[str, int]) -> list[str]:
    """Return required packages that ran fewer than MIN_PASSES tests.

    Split out from `main` so the rule itself is unit-testable: a package whose
    entire suite skipped is the exact case a green `go test` exit code hides.
    """
    return [
        package
        for package in REQUIRED_PACKAGES
        if pass_counts.get(package, 0) < MIN_PASSES
    ]


def main() -> int:
    dsn = os.environ.get("TEST_POSTGRES_DSN", "").strip()
    if not dsn:
        print(
            "TEST_POSTGRES_DSN is unset — cannot prove Postgres-backed tests ran.",
            file=sys.stderr,
        )
        return 1

    env = os.environ.copy()
    # A test must not be able to reach a real provider through this gate.
    env.pop("GROQ_API_KEY", None)
    env.pop("POOLSIDE_API_KEY", None)

    print(f"Postgres-gated package coverage (minimum {MIN_PASSES} passing test per package)")
    pass_counts: dict[str, int] = {}
    for package in REQUIRED_PACKAGES:
        passed, skipped = run_package(package, env)
        pass_counts[package] = len(passed)
        status = "ok" if len(passed) >= MIN_PASSES else "UNDER-RUN"
        print(f"  {status:>9}  {package}  pass={len(passed)} skip={len(skipped)}")

    underran = underrun_packages(pass_counts)

    if underran:
        print(
            "the following Postgres-gated packages executed no tests (they skipped, or "
            "the service is unreachable): " + ", ".join(underran),
            file=sys.stderr,
        )
        return 1

    print(f"PASS: all {len(REQUIRED_PACKAGES)} Postgres-gated packages executed tests.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
