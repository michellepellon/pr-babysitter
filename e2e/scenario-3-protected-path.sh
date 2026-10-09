#!/usr/bin/env bash
# ABOUTME: e2e scenario 3: the only fix touches a path the caller protects, so apply rejects the round.
# ABOUTME: Needs the caller's real federation rule ID and protected_paths: protected/; spends one round.
set -uo pipefail
# shellcheck source=e2e/lib.sh
source "$(dirname "$0")/lib.sh"

# prompt.md tells the agent to keep out of .github/, so this uses the caller's own
# protected_paths, which the agent can't know; apply's check is what's under test.
new_pr protected-path protected/calc
start=$(head_sha)
round_done 1 || fail "round 1 didn't finish"
check "apply rejected the round for a protected path" outcome_matches '^rejected: commit [0-9a-f]+ changes protected path'
check "nothing was pushed" [ "$(head_sha)" = "$start" ]
