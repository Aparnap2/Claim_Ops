#!/usr/bin/env python3
"""Fail closed unless every Mockoon-gated test actually executed.

Why this exists: `integration.yml` probed Mockoon informationally and never
failed, so Mockoon-dependent tests could self-skip while the job reported
success. That is the same false-green shape APA-83 P10 closed for PostgreSQL.

The difference here is that a package's overall pass count is not evidence: a
package can contain many unrelated passing tests while every Mockoon-gated test
skipped. This gate therefore requires the *named* Mockoon-gated tests to appear
as `--- PASS`, which distinguishes "Mockoon-backed assertions ran" from "the
package ran some other tests".
"""

from __future__ import annotations

import os
import re
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
API = ROOT / "apps" / "api"

# Every test gated on Mockoon *where Mockoon is the only external dependency*,
# derived from repository evidence on main@d1d4937:
#
#   ./internal/adapters/http      TestLiveContracts
#       integration_test.go self-skips when http://localhost:3001 is unreachable.
#   ./internal/investigate/tools  TestLiveToolMatrix
#       mockoon_test.go self-skips when the live origin is unreachable.
#
# DELIBERATELY EXCLUDED — internal/app:
#   The three infra-matrix cases call infraEnvFor(t, _, true), which is
#   Mockoon-gated, but infraEnvFor unconditionally calls infraRequireStorage
#   (localgcp Storage :4443) and calls infraRequireGCW when needGCW. Measured
#   with Mockoon and Postgres both healthy: those three still skip. localgcp is
#   a host CLI with no pinned image and integration.yml deliberately provides no
#   localgcp step; the GCW emulator is blocked by APA-37/KL-01. Requiring them
#   here would make the integration job permanently red for a dependency this
#   workflow intentionally does not supply.
#
# internal/investigate/tools/mockoon_down_live_test.go is also excluded: it
# drives a deliberately closed loopback port and must pass without Mockoon.
#
# This is NOT the PostgreSQL package list: internal/app is in the P10 gate (it
# needs Postgres) but not here, because its remaining blockers are localgcp and
# GCW rather than Mockoon.
REQUIRED_MOCKOON_TESTS: dict[str, tuple[str, ...]] = {
    "./internal/adapters/http": ("TestLiveContracts",),
    "./internal/investigate/tools": ("TestLiveToolMatrix",),
}

# Documented, measured exclusions — asserted in tests so they cannot be dropped
# from this file's rationale without noticing.
EXCLUDED_MOCKOON_TESTS: dict[str, str] = {
    "TestLiveInfraMatrix_DuplicatePubSubDelivery_ConvergesToOneDocumentSet":
        "requires localgcp Storage via infraRequireStorage",
    "TestLiveInfraMatrix_DuplicatePubSubDelivery_StartsWorkflowExactlyOnce":
        "requires localgcp Storage and the GCW emulator",
    "TestLiveInfraMatrix_CorruptGCSBlob_IsTerminalNoRetryNoLaunch":
        "requires localgcp Storage and the GCW emulator",
}

# Subtests append "/name"; match the top-level test exactly or by prefix.
PASS_RE = re.compile(r"^--- PASS: (\S+)", re.MULTILINE)
SKIP_RE = re.compile(r"^--- SKIP: (\S+)", re.MULTILINE)


def count_results(output: str) -> tuple[list[str], list[str]]:
    """Return (passed, skipped) test names from `go test -v` output."""
    return PASS_RE.findall(output), SKIP_RE.findall(output)


def passed_required(passed: list[str], required: tuple[str, ...]) -> set[str]:
    """Return the required tests observed as passing.

    A subtest line looks like `TestLiveContracts/case`; the top-level `go test`
    line is also emitted on its own, so prefix matching is safe and lets a
    package that only reports subtests still be recognised.
    """
    observed: set[str] = set()
    for name in required:
        if any(candidate == name or candidate.startswith(name + "/") for candidate in passed):
            observed.add(name)
    return observed


class PackageResult:
    """Outcome of one `go test -v` run, retaining the process exit code.

    The exit code is independent of the pass counts: a package can report
    passing tests and still exit nonzero because another test failed. Treating
    pass counts alone as health would conceal real failures.
    """

    def __init__(self, returncode: int, passed: list[str], skipped: list[str]) -> None:
        self.returncode = returncode
        self.passed = passed
        self.skipped = skipped

    @property
    def failed_tests(self) -> bool:
        return self.returncode != 0


def run_package(package: str, env: dict[str, str]) -> PackageResult:
    result = subprocess.run(
        ["go", "test", "-v", "-count=1", package],
        cwd=API,
        env=env,
        check=False,
        text=True,
        capture_output=True,
    )
    # Inspect output regardless of exit code: a build error must be reported,
    # not silently read as "no Mockoon tests ran".
    passed, skipped = count_results(result.stdout + result.stderr)
    return PackageResult(result.returncode, passed, skipped)


def evaluate(results: dict[str, PackageResult]) -> list[str]:
    """Return human-readable problems; empty means the gate may pass.

    Failure conditions, all of which block:
    - a required package produced no result at all;
    - `go test` exited nonzero (a real failure, build error, or panic), even
      when other tests in that package passed;
    - a named Mockoon-gated test is absent from the passing set — which covers
      both "skipped" and "never ran".
    """
    problems: list[str] = []
    for package, required in REQUIRED_MOCKOON_TESTS.items():
        result = results.get(package)
        if result is None:
            problems.append(f"{package}: no result (package did not run)")
            continue
        if result.failed_tests:
            problems.append(
                f"{package}: go test exited {result.returncode} "
                f"(pass={len(result.passed)} skip={len(result.skipped)})"
            )
            continue
        observed = passed_required(result.passed, required)
        for name in required:
            if name not in observed:
                problems.append(
                    f"{package}: Mockoon-gated test {name} did not pass "
                    f"(pass={len(result.passed)} skip={len(result.skipped)})"
                )
    return problems


def main() -> int:
    endpoint = os.environ.get("MOCKOON_URL", "").strip()
    if not endpoint:
        print(
            "MOCKOON_URL is unset — cannot prove Mockoon-gated tests ran.",
            file=sys.stderr,
        )
        return 1

    env = os.environ.copy()
    # This gate must never be able to reach a real model provider.
    env.pop("GROQ_API_KEY", None)
    env.pop("POOLSIDE_API_KEY", None)

    print(f"Mockoon-gated test coverage against {endpoint}")
    results: dict[str, PackageResult] = {}
    for package in REQUIRED_MOCKOON_TESTS:
        result = run_package(package, env)
        results[package] = result
        observed = passed_required(result.passed, REQUIRED_MOCKOON_TESTS[package])
        needed = len(REQUIRED_MOCKOON_TESTS[package])
        print(
            f"  {package}  pass={len(result.passed)} skip={len(result.skipped)} "
            f"exit={result.returncode} mockoon={len(observed)}/{needed}"
        )

    problems = evaluate(results)
    if problems:
        for problem in problems:
            print(f"::error::{problem}", file=sys.stderr)
        print(
            f"FAIL: {len(problems)} Mockoon coverage problem(s).",
            file=sys.stderr,
        )
        return 1

    total = sum(len(names) for names in REQUIRED_MOCKOON_TESTS.values())
    print(f"PASS: all {total} Mockoon-gated tests executed against {endpoint}.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
