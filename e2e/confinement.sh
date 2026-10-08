#!/usr/bin/env bash
# ABOUTME: Checks agent's sandbox on a runner after `pr-babysitter-sandbox setup` and `close`, with the gateway up.
# ABOUTME: Run as the runner user: e2e/confinement.sh <gateway ip:port>. check.yml runs it; it calls no model.
set -uo pipefail

gateway=$1
sandbox=/usr/local/bin/pr-babysitter-sandbox
failed=0
# A nip.io name that resolves to 10.11.12.13 only if the query reached nip.io's servers.
nonce=$(head -c 6 /dev/urandom | od -An -tx1 | tr -d ' \n')
leak_name() { echo "p${nonce}$1-10-11-12-13.nip.io"; }
# The runner's environment holds the OIDC request token; agent must never see it.
export ACTIONS_ID_TOKEN_REQUEST_TOKEN=must-not-reach-agent

# as_agent runs a command as prove does (see agentPrefix in prove.go), from agent's clone.
as_agent() { (cd /home/agent/repo && sudo env PATH="$PATH" "$sandbox" run timeout 15 "$@"); }

ok() { echo "ok    $1"; }
bad() {
  echo "FAIL  $1"
  [ -n "${2:-}" ] && printf '%s\n' "$2" | head -20 | sed 's/^/      /'
  failed=1
}

# blocked <label> <command...>: the command must fail and print no leaked answer.
blocked() {
  local label=$1 out
  shift
  if out=$(as_agent "$@" 2>&1) || [[ $out == *10.11.12.13* ]]; then bad "$label is blocked" "$out"; else ok "$label is blocked"; fi
}

# allowed <label> <want> <command...>: the command must succeed and print exactly want.
allowed() {
  local label=$1 want=$2 out
  shift 2
  if out=$(as_agent "$@" 2>&1) && [ "$out" = "$want" ]; then ok "$label"; else bad "$label (want $want)" "$out"; fi
}

# A path is untested if the runner image dropped its tool; fail so someone looks.
for tool in getent resolvectl busctl varlinkctl dig curl snap ping python3 ss; do
  command -v "$tool" >/dev/null || bad "runner has $tool, so its path is tested"
done

blocked "getent hosts" getent hosts "$(leak_name a)"
blocked "resolvectl" resolvectl query "$(leak_name b)"
blocked "D-Bus resolve1" busctl call org.freedesktop.resolve1 /org/freedesktop/resolve1 \
  org.freedesktop.resolve1.Manager ResolveHostname isit 0 "$(leak_name c)" 0 0
blocked "varlink resolve" varlinkctl call /run/systemd/resolve/io.systemd.Resolve \
  io.systemd.Resolve.ResolveHostname "{\"name\":\"$(leak_name d)\"}"
blocked "DNS to 127.0.0.53" dig @127.0.0.53 +time=3 +tries=1 "$(leak_name e)"
blocked "DNS to 1.1.1.1" dig @1.1.1.1 +time=3 +tries=1 "$(leak_name f)"
blocked "HTTPS to 1.1.1.1" curl -sS -m 5 https://1.1.1.1/
blocked "snap store search" snap find hello
blocked "ICMP" ping -c1 -W3 1.1.1.1
blocked "the host's other ports" curl -sS -m 5 "http://${gateway%:*}:22/"

# Every unix socket listening on the host: root daemons behind them act for whoever connects.
sockets=$(sudo ss -xlnH | awk '{print $5}' | grep -v '^\*$' | sort -u)
[ -n "$sockets" ] || bad "ss lists the host's listening sockets"
reachable=0
while read -r addr; do
  out=$(as_agent python3 -I -c '
import socket, sys
a = sys.argv[1]
s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
s.settimeout(3)
try:
    s.connect("\0" + a[1:] if a.startswith("@") else a)
    print("connected")
except OSError as e:
    print(type(e).__name__)' "$addr" 2>&1)
  if [ "$out" = connected ]; then
    bad "agent can't connect to $addr"
    reachable=1
  fi
done <<<"$sockets"
[ "$reachable" = 0 ] && ok "agent connects to none of the host's $(wc -l <<<"$sockets") listening unix sockets"

allowed "agent reaches the gateway" 403 curl -sS -m 5 -o /dev/null -w '%{http_code}' "http://$gateway/"
allowed "agent's own loopback works" 200 sh -c \
  'python3 -m http.server --bind 127.0.0.1 9999 >/dev/null 2>&1 & sleep 1; curl -sS -m 5 -o /dev/null -w "%{http_code}" http://127.0.0.1:9999/; kill $!'
allowed "agent sees only HOME and PATH" "$(printf 'HOME=/home/agent\nPATH=%s' "$PATH")" env
# shellcheck disable=SC2016 # expands as agent
allowed "agent runs as itself, with no other groups" "$(id -u agent) $(id -g agent)" sh -c 'echo "$(id -u) $(id -G)"'
allowed "commands start in the caller's directory" /home/agent/repo pwd
allowed "Claude Code starts" ok sh -c '/home/agent/cc/node_modules/.bin/claude --version >/dev/null && echo ok'

# prove's timeout kills the proof's process group as agent, through the same wrapper.
set -m
sudo env PATH="$PATH" "$sandbox" run sh -c 'sleep 300 & exec sleep 300' &
group=$!
set +m
sleep 2
sudo env PATH="$PATH" "$sandbox" run kill -KILL -- "-$group"
sleep 2
if pgrep -g "$group" >/dev/null; then
  bad "killing the process group as agent stops a proof" "$(ps -o pid,pgid,user,args -g "$group")"
  sudo pkill -KILL -u agent
else
  ok "killing the process group as agent stops a proof"
fi

# collect bundles agent's commits through the sandbox.
round=$(as_agent git rev-parse HEAD)
as_agent git commit -q --allow-empty -m "confinement check" >/dev/null
out_dir=$(mktemp -d)
if sudo "$sandbox" collect "$round" "$out_dir" && git bundle list-heads "$out_dir/round.bundle" >/dev/null 2>&1; then
  ok "collect bundles agent's commits"
else
  bad "collect bundles agent's commits" "$(ls -l "$out_dir")"
fi

[ "$failed" = 0 ] && echo "all confinement checks passed" || echo "some confinement checks failed"
exit "$failed"
