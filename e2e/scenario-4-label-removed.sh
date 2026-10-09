#!/usr/bin/env bash
# ABOUTME: e2e scenario 4: the babysit label comes off while work runs, so apply pushes nothing.
# ABOUTME: Runs with either federation rule ID; with the wrong one it spends no model budget.
set -uo pipefail
# shellcheck source=e2e/lib.sh
source "$(dirname "$0")/lib.sh"

new_pr label-removed calc
start=$(head_sha)
round_running 1 || fail "round 1 didn't start"
gh pr edit "$pr" -R "$R" --remove-label babysit >/dev/null
say "removed the label mid-round"
round_done 1 || fail "round 1 didn't finish"
check "apply called the round stale" outcome_matches '^stale: the PR is closed or no longer labeled'
check "nothing was pushed" [ "$(head_sha)" = "$start" ]
