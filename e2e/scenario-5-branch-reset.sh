#!/usr/bin/env bash
# ABOUTME: e2e scenario 5: a person resets the PR branch while work runs, so apply pushes nothing.
# ABOUTME: Runs with either federation rule ID; with the wrong one it spends no model budget.
set -uo pipefail
# shellcheck source=e2e/lib.sh
source "$(dirname "$0")/lib.sh"

new_pr branch-reset calc
round_running 1 || fail "round 1 didn't start"
git -C "$work/repo" commit -q --amend -m "feat: add calc (reset mid-round)"
git -C "$work/repo" push -q --force origin "$branch" 2>/dev/null
reset=$(git -C "$work/repo" rev-parse HEAD)
say "reset the branch to $reset mid-round"
round_done 1 || fail "round 1 didn't finish"
check "apply called the round stale" outcome_matches '^stale: the PR moved'
check "the branch keeps the person's commit" [ "$(head_sha)" = "$reset" ]
