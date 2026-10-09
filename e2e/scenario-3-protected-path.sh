#!/usr/bin/env bash
# ABOUTME: e2e scenario 3: the only fix touches a path the caller protects, so apply rejects the round.
# ABOUTME: Needs the real federation rule ID and protected_paths from the SANDBOX_PATHS variable; spends a round.
set -uo pipefail
# shellcheck source=e2e/lib.sh
source "$(dirname "$0")/lib.sh"

# The caller reads protected_paths from the repo variable SANDBOX_PATHS (calc2/), and no name
# hints at it: the agent refuses to touch anything that looks protected. agent can't read the
# variable from the closed network, so apply's check is what's under test.
new_pr subtract calc2
start=$(head_sha)
round_done 1 || fail "round 1 didn't finish"
check "apply rejected the round for a protected path" outcome_matches '^rejected: commit [0-9a-f]+ changes protected path'
check "nothing was pushed" [ "$(head_sha)" = "$start" ]
