#!/usr/bin/env python3
"""Release gate — machine witness for docs/ci/RELEASE_CONTRACT.md.

Consumes existing CI evidence via the GitHub API (gh CLI) and emits exactly
one machine-readable JSON decision. It never re-executes tests, never
rebuilds, and never re-runs scanners.

Verdicts (RELEASE_CONTRACT.md §5):
  BLOCKED                          exit 2 — a machine criterion failed or is
                                   INDETERMINATE, or the attestation is malformed
  EVIDENCE_COMPLETE_NOT_A_RELEASE  exit 0 — every machine criterion passed and
                                   no attestation was provided
  PASSED_WITH_ATTESTATION          exit 0 — every machine criterion passed and
                                   a well-formed attestation was recorded

No verdict mints a release: `release_decision` is null in every artifact,
and the decisive release action is a human merge/tag that records the gate
run id. Gate green is NECESSARY-NOT-SUFFICIENT for a release.

Fail-closed: a missing run, a non-success conclusion, an unreadable
qualification log, a missing/stale posture record, a malformed attestation,
or any API failure blocks — never an implicit pass.
"""

from __future__ import annotations

import argparse
import json
import os
import re
import subprocess
import sys
from collections.abc import Callable
from dataclasses import dataclass
from datetime import UTC, datetime
from pathlib import Path
from typing import Any, Protocol
from urllib.parse import quote

EXIT_PASS = 0
EXIT_BLOCKED = 2

WORKFLOW_CI = "ci"
WORKFLOW_SECURITY = "security"
WORKFLOW_INTEGRATION = "integration"
WORKFLOW_QUALIFICATION = "qualification"
POSTURE_ISSUE_TITLE = "Security posture runs"
# Exact outcome marker written by .github/workflows/security-posture.yml
# ("Append dated run summary to the tracking issue"): the template line is
# `- Suite result: **${{ needs.suite.result }}**` and needs.*.result has a
# fixed GitHub vocabulary (success | failure | cancelled | skipped | ...).
# M9 parses THIS line from the latest comment body — freshness alone is not
# enough, because the summary job runs `if: always()` and a FAILED suite
# still writes a fresh dated comment.
POSTURE_OUTCOME_PATTERN = re.compile(r"^- Suite result: \*\*([^*\r\n]+)\*\*\s*$", re.MULTILINE)
DEFAULT_POSTURE_MAX_AGE_HOURS = 168.0

# Sentinel lines emitted by infra/postgres/qualify.sh (the harness that
# qualification.yml runs). The gate parses them from the qualification run's
# log; they are the skip-armor: a green exit with zero executed PG-backed
# tests, or an LLM matrix that measured nothing, is never a qualification.
QUALIFIED_MARKER = "QUALIFIED: full suite green against a genuinely fresh, ephemeral database"
PG_SENTINEL_RE = re.compile(r"postgres-backed passing tests: (\d+)")
LLM_SENTINEL_RE = re.compile(r"LLM cases measured: (\d+)/(\d+)")

VERDICT_BLOCKED = "BLOCKED"
VERDICT_EVIDENCE_COMPLETE = "EVIDENCE_COMPLETE_NOT_A_RELEASE"
VERDICT_PASSED = "PASSED_WITH_ATTESTATION"

DISCLAIMER = (
    "Machine witness of machine-checkable criteria plus a recorded human "
    "attestation. NECESSARY-NOT-SUFFICIENT for a release; a human release "
    "action (merge/tag) must record this gate run id."
)

# Runs triggered by these events carry the merged-SHA semantics the gate
# evaluates; a pull_request run for the same SHA is only a fallback.
_PREFERRED_EVENTS = frozenset({"push", "workflow_dispatch", "schedule"})

STATUS_PASS = "pass"
STATUS_FAIL = "fail"
STATUS_NA = "na"
STATUS_INDETERMINATE = "indeterminate"

_BLOCKING_STATUSES = frozenset({STATUS_FAIL, STATUS_INDETERMINATE})


class GateError(RuntimeError):
    """An evidence query failed — the gate must fail closed."""


class GhApi(Protocol):
    """Callable that runs `gh api <args...>` and returns the parsed JSON.

    The parsed value is Any: GitHub endpoints return either JSON objects or
    arrays depending on the endpoint, and every consumer validates the shape
    it relies on (fail-closed on mismatch).
    """

    def __call__(self, args: list[str]) -> Any: ...


RunLogFetcher = Callable[[int], "str | None"]


@dataclass(frozen=True)
class GateConfig:
    repo: str
    target_sha: str
    release_label: str
    posture_max_age_hours: float
    attestation_statement: str
    attested_by: str
    attested_date: str
    gate_run_id: int
    gate_actor: str
    gate_sha: str
    out: str | None


def make_gh_api() -> GhApi:
    """Build a GhApi backed by the gh CLI (auth via GH_TOKEN/GITHUB_TOKEN)."""

    def api(args: list[str]) -> Any:
        proc = subprocess.run(
            ["gh", "api", *args],
            capture_output=True,
            text=True,
            timeout=120,
        )
        if proc.returncode != 0:
            raise GateError(f"gh api {args[0]!r} failed: {proc.stderr.strip()[:400]}")
        try:
            return json.loads(proc.stdout)
        except json.JSONDecodeError as exc:
            raise GateError(f"gh api {args[0]!r} returned invalid JSON: {exc}") from exc

    return api


def make_run_log_fetcher() -> RunLogFetcher:
    """Build a fetcher for a workflow run's log via `gh run view --log`."""

    def fetch(run_id: int) -> str | None:
        proc = subprocess.run(
            ["gh", "run", "view", str(run_id), "--log"],
            capture_output=True,
            text=True,
            timeout=600,
        )
        if proc.returncode != 0:
            return None
        return proc.stdout

    return fetch


def _parse_gh_time(value: str) -> datetime:
    """Parse a GitHub API timestamp (ISO 8601, Z suffix)."""
    return datetime.strptime(value, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=UTC)


def resolve_full_sha(gh: GhApi, repo: str, target: str) -> str:
    """Resolve a short or full SHA to the full commit SHA via the commits API."""
    data = gh([f"repos/{repo}/commits/{target}"])
    sha = data.get("sha")
    if not isinstance(sha, str) or len(sha) != 40:
        raise GateError(f"could not resolve target SHA {target!r} to a full SHA")
    return sha


def list_runs(gh: GhApi, repo: str, full_sha: str) -> list[dict[str, Any]]:
    """List completed-and-otherwise workflow runs whose head_sha is the target."""
    runs: list[dict[str, Any]] = []
    for page in (1, 2, 3):
        data = gh([f"repos/{repo}/actions/runs?head_sha={full_sha}&per_page=100&page={page}"])
        batch = data.get("workflow_runs", [])
        if not isinstance(batch, list):
            raise GateError("unexpected workflow_runs payload from the runs API")
        runs.extend(batch)
        if len(batch) < 100:
            break
    return runs


def select_run(runs: list[dict[str, Any]], workflow_name: str) -> dict[str, Any] | None:
    """Pick the run that speaks for `workflow_name` on the target SHA.

    Among completed runs for the workflow, push/workflow_dispatch/schedule
    runs are preferred over pull_request runs; within the preferred pool the
    most recent run wins (its conclusion is that workflow's current verdict
    for the SHA).
    """
    candidates = [
        run for run in runs if run.get("name") == workflow_name and run.get("status") == "completed"
    ]
    if not candidates:
        return None
    preferred = [run for run in candidates if run.get("event") in _PREFERRED_EVENTS]
    pool = preferred or candidates
    return max(pool, key=lambda run: str(run.get("created_at", "")))


def run_jobs(gh: GhApi, repo: str, run_id: int) -> list[dict[str, Any]]:
    """List the jobs of a workflow run."""
    data = gh([f"repos/{repo}/actions/runs/{run_id}/jobs?per_page=100"])
    jobs = data.get("jobs", [])
    if not isinstance(jobs, list):
        raise GateError(f"unexpected jobs payload for run {run_id}")
    return jobs


def job_conclusion(jobs: list[dict[str, Any]], name: str) -> str | None:
    """Return the conclusion of the named job, or None if it is absent."""
    for job in jobs:
        if job.get("name") == name:
            conclusion = job.get("conclusion")
            return str(conclusion) if conclusion is not None else None
    return None


def _criterion(
    cid: str,
    workflow: str,
    job: str,
    run: dict[str, Any] | None,
    status: str,
    reason: str,
) -> dict[str, Any]:
    if run is None:
        return {
            "criterion": cid,
            "workflow": workflow,
            "job": job,
            "run_id": None,
            "conclusion": None,
            "url": None,
            "head_sha": None,
            "timestamp": None,
            "status": status,
            "reason": reason,
        }
    return {
        "criterion": cid,
        "workflow": workflow,
        "job": job,
        "run_id": run.get("id"),
        "conclusion": run.get("conclusion"),
        "url": run.get("html_url"),
        "head_sha": run.get("head_sha"),
        "timestamp": run.get("created_at"),
        "status": status,
        "reason": reason,
    }


def _run_and_job_criterion(
    gh: GhApi,
    repo: str,
    runs: list[dict[str, Any]],
    cid: str,
    workflow: str,
    job: str,
) -> tuple[dict[str, Any], dict[str, Any] | None]:
    """M1/M2/M3/M4/M5/M7 shape: run exists, run success, named job success."""
    run = select_run(runs, workflow)
    if run is None:
        return (
            _criterion(
                cid,
                workflow,
                job,
                None,
                STATUS_FAIL,
                f"no completed {workflow} run for the target SHA",
            ),
            None,
        )
    run_id = int(run["id"])
    conclusion = str(run.get("conclusion"))
    if conclusion != "success":
        return (
            _criterion(
                cid,
                workflow,
                job,
                run,
                STATUS_FAIL,
                f"{workflow} run {run_id} conclusion is {conclusion!r}",
            ),
            run,
        )
    jobs = run_jobs(gh, repo, run_id)
    job_conc = job_conclusion(jobs, job)
    if job_conc is None:
        return (
            _criterion(
                cid,
                workflow,
                job,
                run,
                STATUS_INDETERMINATE,
                f"job {job!r} not found in {workflow} run {run_id}",
            ),
            run,
        )
    if job_conc != "success":
        return (
            _criterion(
                cid,
                workflow,
                job,
                run,
                STATUS_FAIL,
                f"job {job!r} in {workflow} run {run_id} conclusion is {job_conc!r}",
            ),
            run,
        )
    return (
        _criterion(
            cid,
            workflow,
            job,
            run,
            STATUS_PASS,
            f"run {run_id} and job {job!r} succeeded",
        ),
        run,
    )


def _security_delta_criterion(
    security_run: dict[str, Any] | None, security_jobs: list[dict[str, Any]] | None
) -> dict[str, Any]:
    """M6: security-delta is N/A on push/workflow_dispatch runs (PR-only job);
    on a pull_request run it must exist and succeed (exit 2 = indeterminate)."""
    if security_run is None:
        return _criterion(
            "M6",
            WORKFLOW_SECURITY,
            "security-delta",
            None,
            STATUS_FAIL,
            "no security run for the target SHA (M3 already fails)",
        )
    event = str(security_run.get("event"))
    if event in _PREFERRED_EVENTS:
        return _criterion(
            "M6",
            WORKFLOW_SECURITY,
            "security-delta",
            security_run,
            STATUS_NA,
            f"PR-only job; the selected security run event is {event!r}",
        )
    run_id = int(security_run["id"])
    if security_jobs is None:
        return _criterion(
            "M6",
            WORKFLOW_SECURITY,
            "security-delta",
            security_run,
            STATUS_INDETERMINATE,
            f"jobs of security run {run_id} unavailable",
        )
    job_conc = job_conclusion(security_jobs, "security-delta")
    if job_conc is None:
        return _criterion(
            "M6",
            WORKFLOW_SECURITY,
            "security-delta",
            security_run,
            STATUS_INDETERMINATE,
            f"security-delta job absent from a pull_request security run {run_id}",
        )
    if job_conc != "success":
        return _criterion(
            "M6",
            WORKFLOW_SECURITY,
            "security-delta",
            security_run,
            STATUS_FAIL,
            f"security-delta job in security run {run_id} conclusion is {job_conc!r}",
        )
    return _criterion(
        "M6",
        WORKFLOW_SECURITY,
        "security-delta",
        security_run,
        STATUS_PASS,
        f"security-delta job succeeded in security run {run_id}",
    )


def _qualification_criterion(
    runs: list[dict[str, Any]], fetch_log: RunLogFetcher
) -> tuple[dict[str, Any], dict[str, Any]]:
    """M8: a successful qualification run on the SHA whose log carries the
    sentinel lines (skip-armor). Missing log or missing sentinel lines is
    INDETERMINATE — never an implicit pass."""
    qual: dict[str, Any] = {
        "run_id": None,
        "conclusion": None,
        "scope": None,
        "pg_backed_passing_tests": None,
        "llm_measured": None,
        "llm_expected": None,
        "log_evidence": False,
    }
    run = select_run(runs, WORKFLOW_QUALIFICATION)
    if run is None:
        return (
            _criterion(
                "M8",
                WORKFLOW_QUALIFICATION,
                "qualify",
                None,
                STATUS_FAIL,
                "no qualification run for the target SHA — live qualification "
                "evidence is required (PR checks green ≠ qualified)",
            ),
            qual,
        )
    run_id = int(run["id"])
    qual["run_id"] = run_id
    qual["conclusion"] = run.get("conclusion")
    conclusion = str(run.get("conclusion"))
    if conclusion != "success":
        return (
            _criterion(
                "M8",
                WORKFLOW_QUALIFICATION,
                "qualify",
                run,
                STATUS_FAIL,
                f"qualification run {run_id} conclusion is {conclusion!r}",
            ),
            qual,
        )
    log = fetch_log(run_id)
    if log is None:
        return (
            _criterion(
                "M8",
                WORKFLOW_QUALIFICATION,
                "qualify",
                run,
                STATUS_INDETERMINATE,
                f"qualification run {run_id} log unavailable (expired or fetch "
                "failed) — INDETERMINATE, never an implicit pass",
            ),
            qual,
        )
    qual["log_evidence"] = True
    if QUALIFIED_MARKER not in log:
        return (
            _criterion(
                "M8",
                WORKFLOW_QUALIFICATION,
                "qualify",
                run,
                STATUS_INDETERMINATE,
                f"qualification run {run_id} log lacks the QUALIFIED sentinel line",
            ),
            qual,
        )
    pg_match = PG_SENTINEL_RE.search(log)
    if pg_match is None:
        return (
            _criterion(
                "M8",
                WORKFLOW_QUALIFICATION,
                "qualify",
                run,
                STATUS_INDETERMINATE,
                f"qualification run {run_id} log lacks the PG sentinel count",
            ),
            qual,
        )
    pg_count = int(pg_match.group(1))
    qual["pg_backed_passing_tests"] = pg_count
    if pg_count < 1:
        return (
            _criterion(
                "M8",
                WORKFLOW_QUALIFICATION,
                "qualify",
                run,
                STATUS_INDETERMINATE,
                f"qualification run {run_id} PG sentinel count is zero — the "
                "suite self-skipped; this is not a qualification",
            ),
            qual,
        )
    llm_match = LLM_SENTINEL_RE.search(log)
    if llm_match is None:
        qual["scope"] = "pg-only"
        qual["llm_measured"] = 0
        qual["llm_expected"] = 0
        return (
            _criterion(
                "M8",
                WORKFLOW_QUALIFICATION,
                "qualify",
                run,
                STATUS_PASS,
                f"PG-only qualification (run {run_id}, {pg_count} PG-backed passing tests)",
            ),
            qual,
        )
    measured = int(llm_match.group(1))
    expected = int(llm_match.group(2))
    qual["scope"] = "llm-matrix"
    qual["llm_measured"] = measured
    qual["llm_expected"] = expected
    if measured != expected:
        return (
            _criterion(
                "M8",
                WORKFLOW_QUALIFICATION,
                "qualify",
                run,
                STATUS_FAIL,
                f"qualification run {run_id} LLM matrix incomplete: "
                f"{measured}/{expected} measured (a 429/skip is never a "
                "measurement)",
            ),
            qual,
        )
    return (
        _criterion(
            "M8",
            WORKFLOW_QUALIFICATION,
            "qualify",
            run,
            STATUS_PASS,
            f"LLM-matrix qualification (run {run_id}, {pg_count} PG-backed "
            f"passing tests, {measured}/{expected} scenarios measured)",
        ),
        qual,
    )


def _find_posture_issue(gh: GhApi, repo: str) -> int | None:
    """Find the `Security posture runs` tracking issue (exact title match)."""
    query = quote(f'repo:{repo} "{POSTURE_ISSUE_TITLE}" in:title type:issue')
    try:
        data = gh([f"search/issues?q={query}"])
        for item in data.get("items", []):
            if item.get("title") == POSTURE_ISSUE_TITLE:
                number = item.get("number")
                if isinstance(number, int):
                    return number
    except GateError:
        pass  # search unavailable — fall through to listing
    for page in (1, 2, 3):
        data = gh([f"repos/{repo}/issues?state=all&per_page=100&page={page}"])
        if not isinstance(data, list):
            raise GateError("unexpected issues payload from the issues API")
        for item in data:
            if item.get("title") == POSTURE_ISSUE_TITLE and item.get("pull_request") is None:
                number = item.get("number")
                if isinstance(number, int):
                    return number
        if len(data) < 100:
            break
    return None


def _latest_posture_comment(gh: GhApi, repo: str, issue_number: int) -> tuple[str, str] | None:
    """Return (created_at, body) of the latest comment on the issue."""
    comments: list[dict[str, Any]] = []
    for page in (1, 2, 3):
        data = gh([f"repos/{repo}/issues/{issue_number}/comments?per_page=100&page={page}"])
        if not isinstance(data, list):
            raise GateError(f"unexpected comments payload for issue {issue_number}")
        comments.extend(data)
        if len(data) < 100:
            break
    if not comments:
        return None
    latest = max(comments, key=lambda comment: str(comment.get("created_at", "")))
    created = latest.get("created_at")
    if not isinstance(created, str):
        return None
    body = latest.get("body")
    return created, (body if isinstance(body, str) else "")


def _parse_posture_outcome(body: str) -> tuple[str, str] | None:
    """Parse the recorded suite outcome from a posture comment body.

    Returns (outcome, matched_line) for the exact `- Suite result: **X**`
    marker, or None when the body carries no marker (or conflicting
    duplicates) — the caller must treat None as INDETERMINATE, never as a
    pass. Anything other than the literal value `success` is an explicit
    failure (failure | cancelled | skipped | ...).
    """
    matches = list(POSTURE_OUTCOME_PATTERN.finditer(body))
    if not matches:
        return None
    values = {match.group(1).strip() for match in matches}
    if len(values) != 1:
        return None  # ambiguous/conflicting markers — fail closed
    value = values.pop()
    return value, matches[-1].group(0).rstrip()


def _posture_criterion(
    gh: GhApi, repo: str, max_age_hours: float, now: datetime
) -> tuple[dict[str, Any], dict[str, Any]]:
    """M9: the latest dated comment on the posture tracking issue must
    record an EXPLICITLY successful suite result AND be within the
    freshness bound — both must hold. The summary job runs `if: always()`,
    so a failed suite still writes a fresh comment; freshness alone would
    let the gate pass on a failing posture. Missing issue/comments,
    unparseable timestamp, or a body without a parseable outcome marker is
    INDETERMINATE — never an implicit pass."""
    posture: dict[str, Any] = {
        "issue_number": None,
        "latest_comment_at": None,
        "recorded_outcome": None,
        "outcome_line": None,
        "age_hours": None,
        "max_age_hours": max_age_hours,
        "status": STATUS_INDETERMINATE,
    }
    issue_number = _find_posture_issue(gh, repo)
    if issue_number is None:
        return (
            _criterion(
                "M9",
                "security-posture",
                "summary",
                None,
                STATUS_INDETERMINATE,
                "posture tracking issue not found — no posture record exists",
            ),
            posture,
        )
    posture["issue_number"] = issue_number
    latest = _latest_posture_comment(gh, repo, issue_number)
    if latest is None:
        return (
            _criterion(
                "M9",
                "security-posture",
                "summary",
                None,
                STATUS_INDETERMINATE,
                f"posture tracking issue #{issue_number} has no comments",
            ),
            posture,
        )
    created_at, body = latest
    posture["latest_comment_at"] = created_at
    outcome = _parse_posture_outcome(body)
    if outcome is None:
        return (
            _criterion(
                "M9",
                "security-posture",
                "summary",
                None,
                STATUS_INDETERMINATE,
                "posture record has no parseable suite outcome — expected a "
                f"'- Suite result: **...**' line (issue #{issue_number})",
            ),
            posture,
        )
    recorded, outcome_line = outcome
    posture["recorded_outcome"] = recorded
    posture["outcome_line"] = outcome_line
    if recorded != "success":
        posture["status"] = STATUS_FAIL
        return (
            _criterion(
                "M9",
                "security-posture",
                "summary",
                None,
                STATUS_FAIL,
                f"latest posture suite failed: {outcome_line!r} (issue #{issue_number})",
            ),
            posture,
        )
    try:
        timestamp = _parse_gh_time(created_at)
    except ValueError:
        return (
            _criterion(
                "M9",
                "security-posture",
                "summary",
                None,
                STATUS_INDETERMINATE,
                f"unparseable posture timestamp {created_at!r}",
            ),
            posture,
        )
    age_hours = (now - timestamp).total_seconds() / 3600.0
    posture["age_hours"] = round(age_hours, 2)
    if age_hours > max_age_hours:
        posture["status"] = STATUS_FAIL
        return (
            _criterion(
                "M9",
                "security-posture",
                "summary",
                None,
                STATUS_FAIL,
                f"posture record is stale: {age_hours:.1f}h old exceeds the "
                f"{max_age_hours:.1f}h bound (issue #{issue_number})",
            ),
            posture,
        )
    posture["status"] = STATUS_PASS
    return (
        _criterion(
            "M9",
            "security-posture",
            "summary",
            None,
            STATUS_PASS,
            f"posture record fresh ({age_hours:.1f}h old within the "
            f"{max_age_hours:.1f}h bound) and suite recorded success "
            f"(issue #{issue_number})",
        ),
        posture,
    )


def validate_attestation(statement: str, attested_by: str, attested_date: str) -> dict[str, Any]:
    """Form-validate the attestation inputs (RELEASE_CONTRACT.md §3).

    Missing (all blank) is not an error — it caps the verdict at
    EVIDENCE_COMPLETE_NOT_A_RELEASE. Malformed (some fields blank, or an
    unparseable date) is a hard fail.
    """
    record: dict[str, Any] = {
        "provided": False,
        "statement": statement,
        "attested_by": attested_by,
        "attested_date": attested_date,
        "status": "missing",
    }
    if not (statement.strip() or attested_by.strip() or attested_date.strip()):
        return record
    record["provided"] = True
    if not (statement.strip() and attested_by.strip() and attested_date.strip()):
        record["status"] = "malformed"
        return record
    try:
        datetime.strptime(attested_date.strip(), "%Y-%m-%d")
    except ValueError:
        record["status"] = "malformed"
        return record
    record["status"] = STATUS_PASS
    return record


def decide(
    evidence: list[dict[str, Any]], attestation: dict[str, Any]
) -> tuple[str, list[dict[str, str]]]:
    """Map criteria statuses + attestation form to the verdict."""
    failures = [
        {"criterion": str(criterion["criterion"]), "reason": str(criterion["reason"])}
        for criterion in evidence
        if criterion["status"] in _BLOCKING_STATUSES
    ]
    if attestation["status"] == "malformed":
        failures.append(
            {
                "criterion": "H1-H3",
                "reason": "attestation malformed: all three fields "
                "(attestation_statement, attested_by, attested_date as ISO "
                "YYYY-MM-DD) must be non-blank",
            }
        )
        return VERDICT_BLOCKED, failures
    if failures:
        return VERDICT_BLOCKED, failures
    if attestation["status"] == "missing":
        return VERDICT_EVIDENCE_COMPLETE, failures
    return VERDICT_PASSED, failures


def evaluate(gh: GhApi, fetch_log: RunLogFetcher, config: GateConfig) -> tuple[dict[str, Any], int]:
    """Collect evidence, decide, and build the decision artifact."""
    now = datetime.now(UTC)
    full_sha = resolve_full_sha(gh, config.repo, config.target_sha)
    runs = list_runs(gh, config.repo, full_sha)

    evidence: list[dict[str, Any]] = []

    ci_run_record, _ci_run = _run_and_job_criterion(
        gh, config.repo, runs, "M1", WORKFLOW_CI, "deterministic"
    )
    evidence.append(ci_run_record)
    ci_run = select_run(runs, WORKFLOW_CI)
    if ci_run is not None and ci_run_record["status"] == STATUS_PASS:
        terraform_conc = job_conclusion(run_jobs(gh, config.repo, int(ci_run["id"])), "terraform")
        if terraform_conc is None:
            m2_status, m2_reason = (
                STATUS_INDETERMINATE,
                f"job 'terraform' not found in ci run {int(ci_run['id'])}",
            )
        elif terraform_conc != "success":
            m2_status, m2_reason = (
                STATUS_FAIL,
                f"job 'terraform' in ci run {int(ci_run['id'])} conclusion is {terraform_conc!r}",
            )
        else:
            m2_status, m2_reason = (
                STATUS_PASS,
                f"job 'terraform' in ci run {int(ci_run['id'])} succeeded",
            )
    else:
        m2_status, m2_reason = (
            str(ci_run_record["status"]),
            str(ci_run_record["reason"]),
        )
    evidence.append(_criterion("M2", WORKFLOW_CI, "terraform", ci_run, m2_status, m2_reason))

    security_record, _security_run = _run_and_job_criterion(
        gh, config.repo, runs, "M3", WORKFLOW_SECURITY, "static"
    )
    evidence.append(security_record)
    security_run = select_run(runs, WORKFLOW_SECURITY)
    security_jobs: list[dict[str, Any]] | None = None
    if security_run is not None and security_record["status"] == STATUS_PASS:
        security_jobs = run_jobs(gh, config.repo, int(security_run["id"]))
        for cid, job in (("M4", "govulncheck"), ("M5", "pip-audit")):
            job_conc = job_conclusion(security_jobs, job)
            if job_conc is None:
                status, reason = (
                    STATUS_INDETERMINATE,
                    f"job {job!r} not found in security run {int(security_run['id'])}",
                )
            elif job_conc != "success":
                status, reason = (
                    STATUS_FAIL,
                    f"job {job!r} in security run {int(security_run['id'])} "
                    f"conclusion is {job_conc!r}",
                )
            else:
                status, reason = (
                    STATUS_PASS,
                    f"job {job!r} in security run {int(security_run['id'])} succeeded",
                )
            evidence.append(_criterion(cid, WORKFLOW_SECURITY, job, security_run, status, reason))
    else:
        for cid, job in (("M4", "govulncheck"), ("M5", "pip-audit")):
            evidence.append(
                _criterion(
                    cid,
                    WORKFLOW_SECURITY,
                    job,
                    security_run,
                    str(security_record["status"]),
                    str(security_record["reason"]),
                )
            )
    evidence.append(_security_delta_criterion(security_run, security_jobs))

    integration_record, _integration_run = _run_and_job_criterion(
        gh, config.repo, runs, "M7", WORKFLOW_INTEGRATION, "services"
    )
    evidence.append(integration_record)

    qualification_record, qualification = _qualification_criterion(runs, fetch_log)
    evidence.append(qualification_record)

    posture_record, posture = _posture_criterion(gh, config.repo, config.posture_max_age_hours, now)
    evidence.append(posture_record)

    attestation = validate_attestation(
        config.attestation_statement, config.attested_by, config.attested_date
    )
    verdict, failures = decide(evidence, attestation)

    decision: dict[str, Any] = {
        "schema_version": 1,
        "verdict": verdict,
        "release_decision": None,
        "disclaimer": DISCLAIMER,
        "gate": {
            "run_id": config.gate_run_id,
            "actor": config.gate_actor,
            "gate_workflow_sha": config.gate_sha,
            "generated_at": now.strftime("%Y-%m-%dT%H:%M:%SZ"),
        },
        "target_sha": full_sha,
        "release_label": config.release_label,
        "evidence": evidence,
        "qualification": qualification,
        "posture": posture,
        "attestation": attestation,
        "failures": failures,
    }
    exit_code = EXIT_BLOCKED if verdict == VERDICT_BLOCKED else EXIT_PASS
    return decision, exit_code


def _blocked_on_error(config: GateConfig, exc: Exception) -> dict[str, Any]:
    """Fail-closed artifact for an unexpected evidence-collection error."""
    now = datetime.now(UTC)
    return {
        "schema_version": 1,
        "verdict": VERDICT_BLOCKED,
        "release_decision": None,
        "disclaimer": DISCLAIMER,
        "gate": {
            "run_id": config.gate_run_id,
            "actor": config.gate_actor,
            "gate_workflow_sha": config.gate_sha,
            "generated_at": now.strftime("%Y-%m-%dT%H:%M:%SZ"),
        },
        "target_sha": config.target_sha,
        "release_label": config.release_label,
        "evidence": [],
        "qualification": None,
        "posture": None,
        "attestation": None,
        "failures": [
            {
                "criterion": "GATE",
                "reason": f"INDETERMINATE: evidence collection failed: {exc}",
            }
        ],
    }


def _parse_args(argv: list[str] | None) -> GateConfig:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--repo", default=None, help="owner/name (default $GITHUB_REPOSITORY)")
    parser.add_argument("--target-sha", required=True, help="commit SHA (short or full)")
    parser.add_argument("--release-label", default="")
    parser.add_argument(
        "--posture-max-age-hours",
        type=float,
        default=DEFAULT_POSTURE_MAX_AGE_HOURS,
        help="posture freshness bound in hours (default 168; override for testing)",
    )
    parser.add_argument("--attestation-statement", default="")
    parser.add_argument("--attested-by", default="")
    parser.add_argument("--attested-date", default="")
    parser.add_argument("--gate-run-id", type=int, default=0)
    parser.add_argument("--gate-actor", default="")
    parser.add_argument("--gate-sha", default="")
    parser.add_argument("--out", default=None, help="also write the JSON decision here")
    args = parser.parse_args(argv)

    repo = args.repo or os.environ.get("GITHUB_REPOSITORY")
    if not repo:
        parser.error("--repo or $GITHUB_REPOSITORY is required")
    assert args.target_sha is not None  # argparse required=True
    return GateConfig(
        repo=repo,
        target_sha=args.target_sha,
        release_label=args.release_label,
        posture_max_age_hours=args.posture_max_age_hours,
        attestation_statement=args.attestation_statement,
        attested_by=args.attested_by,
        attested_date=args.attested_date,
        gate_run_id=args.gate_run_id,
        gate_actor=args.gate_actor,
        gate_sha=args.gate_sha,
        out=args.out,
    )


def main(argv: list[str] | None = None) -> int:
    config = _parse_args(argv)
    gh = make_gh_api()
    fetch_log = make_run_log_fetcher()
    try:
        decision, exit_code = evaluate(gh, fetch_log, config)
    except Exception as exc:  # fail-closed on any evidence-collection failure
        decision = _blocked_on_error(config, exc)
        exit_code = EXIT_BLOCKED
    payload = json.dumps(decision, indent=2)
    if config.out is not None:
        Path(config.out).write_text(payload + "\n", encoding="utf-8")
    print(payload)
    print(
        f"release-gate: verdict={decision['verdict']} "
        f"target={decision['target_sha']} failures={len(decision['failures'])}",
        file=sys.stderr,
    )
    return exit_code


if __name__ == "__main__":
    sys.exit(main())
