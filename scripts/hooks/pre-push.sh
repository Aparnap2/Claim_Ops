#!/bin/sh
# Git pre-push hook — delegates to `make pre-push` (full check gate + race
# detector) so the gate is defined exactly once (Makefile).
# Install with `make hooks`. Bypassing with --no-verify is forbidden by repo
# policy (AGENTS.md): fix the gate instead.
set -e
root="$(git rev-parse --show-toplevel)"
cd "$root"
exec make pre-push
