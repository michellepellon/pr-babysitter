#!/usr/bin/env bash
# ABOUTME: e2e scenario 1: a failing test gets a proved fix pushed, its CI waits for approval, the owner is named.
# ABOUTME: Needs the caller's real federation rule ID and dry_run: false; spends one round of model budget.
set -uo pipefail
# shellcheck source=e2e/lib.sh
source "$(dirname "$0")/lib.sh"

owner=$(gh api user --jq .login)
new_pr failing-test calc
round_done 1 || fail "round 1 didn't finish"
check "round 1 pushed the fix" outcome_matches '^pushed [0-9]+ commits'
tip=$(head_sha)
check "the bot wrote the pushed commit" [ "$(gh api "repos/$R/commits/$tip" --jq .author.id)" = "$bot_id" ]
# GITHUB_TOKEN pushes start CI runs that wait for a writer's approval.
check "CI on the bot's commit waits for approval" wait_ci_approval "$tip"
gh workflow run babysit.yml -R "$R" --ref main >/dev/null
check "the status asks @$owner to approve the runs" wait_status "@$owner.*approve the workflow runs" 15
