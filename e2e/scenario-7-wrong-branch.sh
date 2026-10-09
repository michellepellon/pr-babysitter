#!/usr/bin/env bash
# ABOUTME: e2e scenario 7: a dispatch from a branch other than main fails before work starts.
# ABOUTME: Runs with either federation rule ID; it never reaches the model.
set -uo pipefail
# shellcheck source=e2e/lib.sh
source "$(dirname "$0")/lib.sh"

new_pr wrong-branch calc
round_done 1 || fail "round 1 didn't finish"
# A branch carrying main's caller, dispatched from.
other="e2e/dispatch-$(date +%s)"
git -C "$work/repo" push -q origin "origin/main:refs/heads/$other" 2>/dev/null
trap 'git -C "$work/repo" push -q origin --delete "$other" 2>/dev/null; cleanup' EXIT
gh pr comment "$pr" -R "$R" --body "e2e: please try again." >/dev/null
run=$(gh workflow run babysit.yml -R "$R" --ref "$other" | sed -n 's|.*/actions/runs/||p')
[ -n "$run" ] || fail "the dispatch printed no run"
gh run watch "$run" -R "$R" >/dev/null 2>&1
jobs=$(gh run view "$run" -R "$R" --json jobs)
check "plan picked the PR" [ "$(jq -r '.jobs[] | select(.name | endswith("plan")) | .conclusion' <<<"$jobs")" = success ]
check "work failed" [ "$(jq -r '.jobs[] | select(.name | endswith("work")) | .conclusion' <<<"$jobs")" = failure ]
check "Claude Code never ran" [ -z "$(jq -r '.jobs[].steps[]? | select(.name == "Run Claude Code as agent" and .conclusion != "skipped") | .name' <<<"$jobs")" ]
round_done 2 || fail "round 2 didn't finish"
check "apply recorded the failed work job" outcome_matches '^failed: work ended failure'
