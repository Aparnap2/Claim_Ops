#!/usr/bin/env python3
"""Prove live-provider tests are excluded by build tags and unreachable by default tests."""

from __future__ import annotations

import json
import os
import re
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
API = ROOT / "apps" / "api"
DEFAULT_ONLY_TESTS = {
    "agent": (
        "TestQualificationGate_S4_CrossTenantIsolation",
        "TestQualificationGate_S6_MissingRequiredDocumentHITL",
        "TestQualificationS8_TransportRetryBounds",
    ),
}

LIVE_TESTS = {
    "orchestrate": (
        "TestQualification_S1_NormalGroundedCase",
        "TestQualification_S2_AmbiguousEvidence",
        "TestQualification_S3_FabricatedEvidence",
        "TestQualification_S5_UnauthorizedEvidence",
        "TestQualification_S7_MalformedOutput",
        "TestQualification_S10_DeadlineAndCancellation",
        "TestAPA55_LiveQualification",
        "TestAPA56Causal_LiveMeasurement",
        "TestAPA58_ScenarioFixtures_Matrix",
        "TestAPA58_Poolside_ServesTheProductionShape",
        "TestAPA59ABDecisiveToolTrajectory",
    ),
    "agent": (
        "TestQualificationLive_S4_CrossTenantIsolation",
        "TestQualificationLive_S6_MissingRequiredDocumentHITL",
        "TestQualificationLive_S8_RetryNoDuplicateMutation",
        "TestQualificationLive_S9_BudgetExhaustion",
        "TestQualificationLive_S10_DeadlinePropagates",
        "TestQualificationLive_S11_IdempotentExecution",
    ),
}

# A test-name inventory is useful for the positive/negative -list proof, but
# must not be the only exclusion control. Follow calls from every default-build
# Test* function through same-package helpers and reject live-provider gates or
# environment-backed client constructors reachable along that path.
NON_CODE_RE = re.compile(
    r"//[^\n]*|/\*.*?\*/|\"(?:\\.|[^\"\\])*\"|`[^`]*`|'(?:\\.|[^'\\])*'",
    re.DOTALL,
)
FUNCTION_RE = re.compile(r"\bfunc\s+(?:\([^)]*\)\s*)?([A-Za-z_]\w*)\s*\(")
BARE_CALL_RE = re.compile(r"(?<![.\w])([A-Za-z_]\w*)\s*\(")
LIVE_CALL_RE = re.compile(
    r"\b(?:requireLive[A-Za-z0-9_]*|New[A-Za-z0-9_]*ClientFromEnv)\s*\("
)


def mask_go_non_code(source: str) -> str:
    """Blank comments and literals while preserving offsets and newlines."""
    def blank(match: re.Match[str]) -> str:
        return "".join("\n" if char == "\n" else " " for char in match.group(0))

    return NON_CODE_RE.sub(blank, source)


def extract_go_functions(source: str) -> list[tuple[str, str]]:
    """Extract function names and masked bodies for a lightweight call-graph check."""
    masked = mask_go_non_code(source)
    functions: list[tuple[str, str]] = []
    for match in FUNCTION_RE.finditer(masked):
        name = match.group(1)
        open_paren = masked.find("(", match.start(), match.end())
        depth = 0
        close_paren = -1
        for index in range(open_paren, len(masked)):
            if masked[index] == "(":
                depth += 1
            elif masked[index] == ")":
                depth -= 1
                if depth == 0:
                    close_paren = index
                    break
        if close_paren < 0:
            continue
        body_open = masked.find("{", close_paren + 1)
        if body_open < 0:
            continue
        body_depth = 0
        body_end = -1
        for index in range(body_open, len(masked)):
            if masked[index] == "{":
                body_depth += 1
            elif masked[index] == "}":
                body_depth -= 1
                if body_depth == 0:
                    body_end = index
                    break
        if body_end >= 0:
            functions.append((name, masked[body_open + 1 : body_end]))
    return functions


def find_reachable_live_calls(
    sources: dict[str, str], default_test_files: set[str]
) -> list[str]:
    """Return live-provider calls reachable from default-build test functions."""
    grouped: dict[tuple[str, str], dict[str, list[tuple[str, str]]]] = {}
    file_groups: dict[str, tuple[str, str]] = {}
    for path, source in sources.items():
        masked = mask_go_non_code(source)
        package_match = re.search(r"\bpackage\s+([A-Za-z_]\w*)", masked)
        if not package_match:
            continue
        group = (str(Path(path).parent), package_match.group(1))
        file_groups[path] = group
        functions = grouped.setdefault(group, {})
        for name, body in extract_go_functions(source):
            functions.setdefault(name, []).append((path, body))

    findings: list[str] = []
    for test_path in sorted(default_test_files):
        group = file_groups.get(test_path)
        if group is None:
            continue
        functions = grouped[group]
        roots = [
            (name, body)
            for name, definitions in functions.items()
            for path, body in definitions
            if path == test_path and name.startswith("Test") and name != "TestMain"
        ]
        for root_name, _ in roots:
            pending: list[tuple[str, tuple[str, ...]]] = [(root_name, (root_name,))]
            visited: set[str] = set()
            while pending:
                function_name, chain = pending.pop()
                if function_name in visited:
                    continue
                visited.add(function_name)
                definitions = functions.get(function_name, [])
                for path, body in definitions:
                    for call in LIVE_CALL_RE.finditer(body):
                        marker = call.group(0).strip().removesuffix("(")
                        findings.append(
                            f"{path}:{root_name} reaches {marker} via {' -> '.join(chain)}"
                        )
                    for call in BARE_CALL_RE.finditer(body):
                        target = call.group(1)
                        if target in functions and target not in visited:
                            pending.append((target, chain + (target,)))
    return sorted(set(findings))


def default_go_test_sources() -> tuple[dict[str, str], set[str]]:
    """Ask Go which files actually participate in the default test build."""
    result = subprocess.run(
        ["go", "list", "-test", "-json", "./..."],
        cwd=API,
        check=False,
        text=True,
        capture_output=True,
    )
    if result.returncode:
        raise RuntimeError("go list -test failed: " + result.stderr.strip())

    decoder = json.JSONDecoder()
    packages: list[dict[str, object]] = []
    offset = 0
    while offset < len(result.stdout):
        while offset < len(result.stdout) and result.stdout[offset].isspace():
            offset += 1
        if offset >= len(result.stdout):
            break
        package, offset = decoder.raw_decode(result.stdout, offset)
        packages.append(package)

    source_paths: set[str] = set()
    test_paths: set[str] = set()
    for package in packages:
        directory = Path(str(package["Dir"]))
        for key in ("GoFiles", "CgoFiles", "TestGoFiles", "XTestGoFiles"):
            for filename in package.get(key, []):
                source_paths.add(str(directory / str(filename)))
        for key in ("TestGoFiles", "XTestGoFiles"):
            for filename in package.get(key, []):
                test_paths.add(str(directory / str(filename)))

    sources = {
        path: Path(path).read_text(encoding="utf-8")
        for path in sorted(source_paths)
        if Path(path).is_file()
    }
    return sources, test_paths


def run_list(args: list[str], env: dict[str, str]) -> str:
    print("+ " + " ".join(args), flush=True)
    result = subprocess.run(args, cwd=API, env=env, check=False, text=True, capture_output=True)
    if result.stdout:
        print(result.stdout, end="", flush=True)
    if result.stderr:
        print(result.stderr, file=sys.stderr, end="", flush=True)
    if result.returncode:
        raise RuntimeError("go test -list command failed with exit " + str(result.returncode))
    return result.stdout


def listed_tests(output: str) -> set[str]:
    return {line.strip() for line in output.splitlines() if line.startswith("Test")}


def main() -> int:
    env = os.environ.copy()
    env["GROQ" + "_API_KEY"] = "sentinel"
    env["POOLSIDE" + "_API_KEY"] = "sentinel"
    print(
        "Credential sentinel variables are set only for compile/list checks; "
        "test bodies are never executed."
    )
    try:
        sources, default_test_files = default_go_test_sources()
        reachable = find_reachable_live_calls(sources, default_test_files)
        if reachable:
            print(
                "Live-provider entry point reachable from the default test build:\n- "
                + "\n- ".join(reachable),
                file=sys.stderr,
            )
            return 1

        default_output = run_list(["go", "test", "./...", "-list", ".*"], env)
        default_names = listed_tests(default_output)
        missing_default = [
            name
            for names in DEFAULT_ONLY_TESTS.values()
            for name in names
            if name not in default_names
        ]
        if missing_default:
            print(
                "Deterministic gate/transport tests were lost from the default build: "
                + ", ".join(missing_default),
                file=sys.stderr,
            )
            return 1
        unexpected = [
            name
            for names in LIVE_TESTS.values()
            for name in names
            if name in default_names
        ]
        if unexpected:
            print(
                "Live-provider tests compiled into the default build: " + ", ".join(unexpected),
                file=sys.stderr,
            )
            return 1

        tagged_outputs = {
            "orchestrate": run_list(
                [
                    "go", "test", "-tags", "qual_live",
                    "./internal/investigate/orchestrate/", "-list", ".*",
                ],
                env,
            ),
            "agent": run_list(
                ["go", "test", "-tags", "qual_live", "./cmd/agent", "-list", ".*"], env
            ),
        }
        for package, expected in LIVE_TESTS.items():
            actual = listed_tests(tagged_outputs[package])
            missing = sorted(set(expected) - actual)
            if missing:
                print(
                    "Missing qual_live tests in " + package + ": " + ", ".join(missing),
                    file=sys.stderr,
                )
                return 1
        live_count = sum(map(len, LIVE_TESTS.values()))
        print(
            f"PASS: all {live_count} live-provider tests are absent by default and listed "
            "under qual_live; no live test bodies were executed."
        )
        return 0
    except (OSError, RuntimeError) as exc:
        print(str(exc), file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
