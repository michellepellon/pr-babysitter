#!/usr/bin/env bash
# ABOUTME: Runs the e2e scenarios against the sandbox repo, one at a time, and prints a summary.
# ABOUTME: Usage: e2e/run.sh [scenario number...]; each scenario's header says which caller settings it needs.
set -uo pipefail

if [ "${1:-}" = -h ] || [ "${1:-}" = --help ]; then
  echo "usage: e2e/run.sh [N...]   run scenarios N (default: all) against \${E2E_REPO:-michellepellon/pr-babysitter-sandbox}"
  echo "Each scenario opens its own PR and closes it at the end; full logs go to \$E2E_LOGS (default: a new temp dir)."
  exit 0
fi
dir=$(dirname "$0")
logs=${E2E_LOGS:-$(mktemp -d)}
want=("$@")
failed=0
for script in "$dir"/scenario-*.sh; do
  n=$(basename "$script" | cut -d- -f2)
  if [ ${#want[@]} -gt 0 ] && [[ " ${want[*]} " != *" $n "* ]]; then continue; fi
  name=$(basename "$script" .sh)
  start=$(date +%s)
  if "$script" >"$logs/$name.log" 2>&1; then result=pass; else result=FAIL failed=1; fi
  printf '%-4s %-34s %4ds  %s\n' "$result" "$name" $(($(date +%s) - start)) "$(grep -m1 '^FAIL' "$logs/$name.log")"
done
echo "logs: $logs"
exit "$failed"
