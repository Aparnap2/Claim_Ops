#!/usr/bin/env python3
"""Prove live-provider tests are absent by default and list only under qual_live."""

from __future__ import annotations

import os
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
API = ROOT / "apps" / "api"
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
    print("Credential sentinel variables are set only for compile/list checks; test bodies are never executed.")
    try:
        default_output = run_list(["go", "test", "./...", "-list", ".*"], env)
        default_names = listed_tests(default_output)
        unexpected = sorted(name for names in LIVE_TESTS.values() for name in names if name in default_names)
        if unexpected:
            print("Live-provider tests compiled into the default build: " + ", ".join(unexpected), file=sys.stderr)
            return 1

        tagged_outputs = {
            "orchestrate": run_list(["go", "test", "-tags", "qual_live", "./internal/investigate/orchestrate/", "-list", ".*"], env),
            "agent": run_list(["go", "test", "-tags", "qual_live", "./cmd/agent", "-list", ".*"], env),
        }
        for package, expected in LIVE_TESTS.items():
            actual = listed_tests(tagged_outputs[package])
            missing = sorted(set(expected) - actual)
            if missing:
                print("Missing qual_live tests in " + package + ": " + ", ".join(missing), file=sys.stderr)
                return 1
        print("PASS: all " + str(sum(map(len, LIVE_TESTS.values()))) +
              " live-provider tests are absent by default and listed under qual_live; none were executed.")
        return 0
    except (OSError, RuntimeError) as exc:
        print(str(exc), file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
