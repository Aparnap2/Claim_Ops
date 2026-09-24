"""S5/APA-26 structural coverage for workflows/claim-investigation.yaml.

No emulator is available in unit CI, so this pins the load-bearing
workflow shape instead of executing it:
  1. Both HITL waits catch callback errors with try/except, and ONLY a
     TimeoutError reaches the EXPIRE post; any other error re-raises
     (a non-timeout failure must never become business-state EXPIRED).
  2. The EXPIRE posts carry the pre-signed credential opaquely
     (X-Signature + verbatim body from the launch argument) — the
     workflow never constructs auth material, satisfying the HMAC
     boundary without weakening it.
  3. Every http.post step has an explicit timeout and bounded retry.
"""

import pathlib

import yaml

WF = pathlib.Path(__file__).resolve().parent.parent.parent / "workflows" / "claim-investigation.yaml"


def load_steps():
    doc = yaml.safe_load(WF.read_text())
    steps = doc["main"]["steps"]
    by_name = {}
    for s in steps:
        assert isinstance(s, dict) and len(s) == 1
        by_name[next(iter(s))] = next(iter(s.values()))
    return by_name


def test_both_waits_filter_timeout_error_only():
    by_name = load_steps()
    for wait in ("wait_human", "wait_human_escalated"):
        node = by_name[wait]
        assert "try" in node and "except" in node, f"{wait} must try/except"
        assert node["except"]["as"] == "e"
        sub = {next(iter(s)): next(iter(s.values())) for s in node["except"]["steps"]}
        # A switch routes TimeoutError to expire, everything else to raise.
        switches = [v for k, v in sub.items() if "switch" in v.get("switch", {}) or "switch" in v]
        assert switches, f"{wait} needs a TimeoutError switch"
        conds = switches[0]["switch"]
        assert conds[0]["condition"] == '${"TimeoutError" in e.tags}'
        assert conds[0]["next"] in ("expire", "expire_escalated")
        assert conds[1]["condition"] is True
        assert conds[1]["next"] in ("propagate", "propagate_escalated")
        # The non-timeout branch re-raises the original error.
        prop = sub[conds[1]["next"]]
        assert prop.get("raise") == "${e}", f"{wait} must re-raise non-timeout errors"


def test_expire_posts_are_signed_opaque():
    by_name = load_steps()
    for wait, expire in (("wait_human", "expire"), ("wait_human_escalated", "expire_escalated")):
        sub = {next(iter(s)): next(iter(s.values())) for s in by_name[wait]["except"]["steps"]}
        post = sub[expire]
        assert post["call"] == "http.post"
        headers = post["args"]["headers"]
        assert headers["X-Signature"] == "${args.expire_signature}", "signature must come from launch args"
        assert post["args"]["body"] == "${args.expire_body}", "body must be forwarded verbatim"
        # The workflow constructs no auth material itself.
        dumped = yaml.safe_dump(post)
        assert "compute_hmac" not in dumped
        assert '"EXPIRE"' not in dumped and "'EXPIRE'" not in dumped


def test_all_posts_bounded():
    by_name = load_steps()

    def walk(node):
        if isinstance(node, dict):
            if node.get("call") == "http.post":
                yield node
            for v in node.values():
                yield from walk(v)
        elif isinstance(node, list):
            for v in node:
                yield from walk(v)

    posts = list(walk(by_name))
    assert len(posts) >= 4
    for p in posts:
        assert p["args"].get("timeout") == 60
        assert p.get("retry", {}).get("max_retries") == 3
