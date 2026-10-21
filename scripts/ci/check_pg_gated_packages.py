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
    "./internal/adapters/worker",
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


class PackageResult:
    """Outcome of one `go test -v` run.

    `returncode` is retained deliberately: a package can report passing tests and
    still exit nonzero because another test failed or the build broke. Counting
    passes alone would classify a package with a real failure as healthy.
    """

    def __init__(self, returncode: int, passed: list[str], skipped: list[str]) -> None:
        self.returncode = returncode
        self.passed = passed
        self.skipped = skipped

    @property
    def failed_to_run(self) -> bool:
        """True when the package executed no passing test (all skipped)."""
        return len(self.passed) < MIN_PASSES

    @property
    def failed_tests(self) -> bool:
        """True when go test itself reported failure, build error, or timeout."""
        return self.returncode != 0


def count_results(output: str) -> tuple[list[str], list[str]]:
    """Return (passed, skipped) test names from `go test -v` output."""
    return PASS_RE.findall(output), SKIP_RE.findall(output)


def run_package(package: str, env: dict[str, str]) -> PackageResult:
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
    passed, skipped = count_results(result.stdout + result.stderr)
    return PackageResult(result.returncode, passed, skipped)


def evaluate(results: dict[str, PackageResult]) -> list[str]:
    """Return human-readable problems; empty means the gate may pass.

    Two independent failures, both of which must block:
    - a package that ran no tests (everything skipped, or the service is down);
    - a package whose `go test` exited nonzero (a real failure, build error, or
      panic), even when other tests in that package passed.
    """
    problems: list[str] = []
    for package in REQUIRED_PACKAGES:
        result = results.get(package)
        if result is None:
            problems.append(f"{package}: no result (package did not run)")
            continue
        if result.failed_tests:
            problems.append(
                f"{package}: go test exited {result.returncode} "
                f"(pass={len(result.passed)} skip={len(result.skipped)})"
            )
        elif result.failed_to_run:
            problems.append(
                f"{package}: executed no tests "
                f"(pass={len(result.passed)} skip={len(result.skipped)})"
            )
    return problems


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
    results: dict[str, PackageResult] = {}
    for package in REQUIRED_PACKAGES:
        result = run_package(package, env)
        results[package] = result
        if result.failed_tests:
            status = "FAILED"
        elif result.failed_to_run:
            status = "UNDER-RUN"
        else:
            status = "ok"
        print(
            f"  {status:>9}  {package}  "
            f"pass={len(result.passed)} skip={len(result.skipped)} exit={result.returncode}"
        )

    problems = evaluate(results)
    if problems:
        for problem in problems:
            print(f"::error::{problem}", file=sys.stderr)
        print(
            f"FAIL: {len(problems)} Postgres-gated package(s) did not run clean.",
            file=sys.stderr,
        )
        return 1

    print(f"PASS: all {len(REQUIRED_PACKAGES)} Postgres-gated packages executed tests.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
