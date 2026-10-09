# ABOUTME: Shared helpers for the e2e scenarios: make a sandbox PR, read its status comment, wait on rounds.
# ABOUTME: Sourced by e2e/scenario-*.sh; needs gh logged in as a writer on the sandbox repo.
# shellcheck shell=bash

R=${E2E_REPO:-michellepellon/pr-babysitter-sandbox}
bot_id=41898282 # github-actions[bot]
work=$(mktemp -d)
pr=""

say() { echo "  $*"; }
fail() {
  echo "FAIL: $*"
  exit 1
}
check() { # check <description> <command...>
  local what=$1
  shift
  if "$@"; then say "ok: $what"; else fail "$what"; fi
}

cleanup() {
  [ -n "$pr" ] && gh pr close "$pr" -R "$R" --delete-branch >/dev/null 2>&1
  rm -rf "$work"
}
trap cleanup EXIT

# new_pr <slug> <dir>: opens a babysit-labeled PR that adds a Go package in dir whose
# test fails, which gives plan an item and starts round 1 through CI's workflow_run.
# Sets pr and branch.
new_pr() {
  local slug=$1 dir=$2
  branch="e2e/$slug-$(date +%s)"
  gh repo clone "$R" "$work/repo" -- -q
  git -C "$work/repo" checkout -q -b "$branch"
  mkdir -p "$work/repo/$dir"
  sub_go "$(basename "$dir")" >"$work/repo/$dir/code.go"
  sub_test_go "$(basename "$dir")" >"$work/repo/$dir/code_test.go"
  git -C "$work/repo" add "$dir"
  git -C "$work/repo" commit -q -m "feat: add $dir ($slug scenario)"
  git -C "$work/repo" push -q -u origin "$branch" 2>/dev/null
  pr=$(gh pr create -R "$R" --head "$branch" --base main --label babysit \
    --title "e2e $slug" --body "pr-babysitter e2e scenario: $slug." | sed 's|.*/||')
  say "opened PR #$pr on $branch"
}

# sub_go and sub_test_go <package> print a package whose test fails until Sub subtracts.
sub_go() { printf 'package %s\n\n// Sub returns a minus b.\nfunc Sub(a, b int) int { return a + b }\n' "$1"; }
sub_test_go() {
  printf 'package %s\n\nimport "testing"\n\nfunc TestSub(t *testing.T) {\n\tif got := Sub(5, 3); got != 2 {\n\t\tt.Fatalf("Sub(5, 3) = %%d, want 2", got)\n\t}\n}\n' "$1"
}

# state prints the PR's state line as JSON (empty before plan first writes it).
state() {
  gh api "repos/$R/issues/$pr/comments" --paginate --jq ".[] | select(.user.id == $bot_id) | .body |
    split(\"\n\")[0] | select(startswith(\"<!-- babysit-state \")) | ltrimstr(\"<!-- babysit-state \") | rtrimstr(\" -->\")"
}

# status prints the status comment's text after the state line.
status() {
  gh api "repos/$R/issues/$pr/comments" --paginate --jq ".[] | select(.user.id == $bot_id) | .body |
    select(startswith(\"<!-- babysit-state \")) | split(\"\n\")[1:] | join(\" \")"
}

# wait_state <jq condition> <minutes>: polls the state line until the condition holds.
wait_state() {
  local deadline=$(($(date +%s) + $2 * 60)) st
  while [ "$(date +%s)" -lt "$deadline" ]; do
    st=$(state)
    if [ -n "$st" ] && jq -e "$1" >/dev/null <<<"$st"; then return 0; fi
    sleep 10
  done
  say "timed out waiting for $1; state is ${st:-missing}"
  return 1
}

# round_running <n> and round_done <n> wait for round n to start or to end.
round_running() { wait_state ".rounds == $1 and .outcome == \"running\"" 30; }
round_done() { wait_state ".rounds == $1 and .outcome != \"running\" and .outcome != \"\"" 45; }

# next_round: a writer's new comment lifts rule 8's hold after a round without a
# push, and a dispatch on main starts plan.
next_round() {
  gh pr comment "$pr" -R "$R" --body "e2e: please try again." >/dev/null
  gh workflow run babysit.yml -R "$R" --ref main >/dev/null
}

head_sha() { gh pr view "$pr" -R "$R" --json headRefOid --jq .headRefOid; }
outcome() { state | jq -r .outcome; }

# wait_status <extended regex> <minutes>: polls the status text until it matches.
wait_status() {
  local deadline=$(($(date +%s) + $2 * 60))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    if status | grep -Eq "$1"; then return 0; fi
    sleep 10
  done
  say "timed out waiting for status /$1/; it reads: $(status)"
  return 1
}

outcome_matches() { [[ $(outcome) =~ $1 ]] || { say "outcome: $(outcome)"; false; }; }

# round_artifacts <dir>: downloads the newest babysit run's round artifact (transcript,
# gateway log, proofs, summary). Scenarios run one at a time, so it is this PR's round.
round_artifacts() {
  local id
  for id in $(gh run list -R "$R" -w babysit -L 10 --json databaseId --jq '.[].databaseId'); do
    if gh run download "$id" -R "$R" -n round -D "$1" >/dev/null 2>&1; then
      say "round artifacts from run $id"
      return 0
    fi
  done
  return 1
}

# wait_ci_approval <sha>: waits up to 10 minutes for the head's Actions check suite
# (app 15368) to show action_required, which is how CI waiting for approval looks.
wait_ci_approval() {
  local deadline=$(($(date +%s) + 600))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    gh api "repos/$R/commits/$1/check-suites" --jq '.check_suites[] | select(.app.id == 15368) | .conclusion' |
      grep -qx action_required && return 0
    sleep 15
  done
  return 1
}
