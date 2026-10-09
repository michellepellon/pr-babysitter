#!/usr/bin/env bash
# ABOUTME: e2e scenario 6: after five rounds the PR is needs-human until someone re-adds the label.
# ABOUTME: Run it with the wrong federation rule ID: every round then ends without a fix, at no model cost.
set -uo pipefail
# shellcheck source=e2e/lib.sh
source "$(dirname "$0")/lib.sh"

new_pr round-cap calc
for n in 1 2 3 4 5; do
  round_done "$n" || fail "round $n didn't finish"
  say "round $n: $(outcome)"
  next_round
done
check "the status stops at five rounds" wait_status '5 rounds have run since the label was added' 15
check "no sixth round ran" [ "$(state | jq .rounds)" = 5 ]
