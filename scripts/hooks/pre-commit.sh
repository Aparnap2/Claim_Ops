#!/bin/sh
# Git pre-commit hook — delegates to `make pre-commit` so the gate is defined
# exactly once (Makefile) and shared by hooks, CI, and manual runs.
# Install with `make hooks`. Bypassing with --no-verify is forbidden by repo
# policy (AGENTS.md): fix the gate instead.
set -e
root="$(git rev-parse --show-toplevel)"
cd "$root"
exec make pre-commit
