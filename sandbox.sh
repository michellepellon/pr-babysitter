#!/usr/bin/env bash
# ABOUTME: Root setup for babysit.yml's work job: the agent and gateway users, agent's clone, Claude Code, the firewall.
# ABOUTME: Usage, as root: sandbox.sh setup <checkout> <items> | close <gateway ip:port> | run <command...> | collect <round head> <out dir>.
set -euo pipefail

# Task 0 ran this version on a runner, and the gateway's allowlist rests on what it sent.
claude_code_version=2.1.292
# apply compares "name <email>" with BABYSIT_BOT in babysit.yml exactly.
bot_name='github-actions[bot]'
bot_email='41898282+github-actions[bot]@users.noreply.github.com'

# as_agent runs a command as agent with only HOME and PATH set, so nothing
# agent runs (npm's install scripts included) sees the job's OIDC variables.
# Only setup uses it; after close, agent's commands go through run.
as_agent() { (cd /home/agent && sudo -u agent env -i HOME=/home/agent PATH="$PATH" "$@"); }
agent_ip=10.199.0.2

case "${1:-}" in
setup)
  # Neither user joins sudo or docker: useradd adds no supplementary groups.
  useradd --create-home --shell /bin/bash agent
  useradd --system --no-create-home --shell /usr/sbin/nologin gateway
  chmod 711 /home/agent # prove runs as the runner user and must enter agent's clone
  # The checkout has no token in it (persist-credentials: false). cp -a copies symlinks as links.
  cp -a "$2" /home/agent/repo
  cp -a "$3" /home/agent/items
  mkdir /home/agent/out
  chown -R agent:agent /home/agent/repo /home/agent/items /home/agent/out
  as_agent git config --global user.name "$bot_name"
  as_agent git config --global user.email "$bot_email"
  as_agent npm install --prefix /home/agent/cc --no-audit --no-fund "@anthropic-ai/claude-code@$claude_code_version"
  ;;
close)
  # run puts agent in the babysit network namespace, joined to the host by a veth
  # pair. There agent may reach only the gateway and its own loopback; everything
  # else, DNS and ICMP included, is refused. Agent has no root there to change it.
  gateway_ip=${2%:*} gateway_port=${2##*:}
  ip netns add babysit
  ip link add babysit0 type veth peer name babysit1 netns babysit
  ip addr add "$gateway_ip" peer "$agent_ip" dev babysit0
  ip link set babysit0 up
  ip -n babysit addr add "$agent_ip" peer "$gateway_ip" dev babysit1
  ip -n babysit link set babysit1 up
  ip -n babysit link set lo up
  for t in iptables ip6tables; do ip netns exec babysit "$t" -A OUTPUT -o lo -j ACCEPT; done
  ip netns exec babysit iptables -A OUTPUT -d "$gateway_ip" -p tcp --dport "$gateway_port" -j ACCEPT
  for t in iptables ip6tables; do ip netns exec babysit "$t" -A OUTPUT -j REJECT; done
  # The same limits for any agent process outside the namespace.
  iptables -I OUTPUT 1 -m owner --uid-owner agent -d "$gateway_ip" -p tcp --dport "$gateway_port" -j ACCEPT
  iptables -I OUTPUT 2 -m owner --uid-owner agent -j REJECT
  ip6tables -I OUTPUT 1 -m owner --uid-owner agent -j REJECT
  pkill -KILL -u agent || true # nothing left from setup keeps running
  ;;
run)
  # Runs a command as agent, with only HOME and PATH set, in the babysit network
  # namespace and with private /run and /tmp. Root daemons listen on unix sockets
  # (D-Bus, systemd-resolved, snapd) and would reach the network for agent; the
  # namespace hides the abstract ones and the mounts hide the rest. ip netns exec
  # makes a mount namespace with / as a slave, so the mounts stay inside it. Every
  # step execs, so the command keeps the caller's process group (prove kills it).
  shift
  # shellcheck disable=SC2016 # $0 and $@ expand in the inner sh
  exec ip netns exec babysit sh -c 'mount -t tmpfs -o mode=755 none /run && mount -t tmpfs -o mode=1777 none /tmp &&
    exec setpriv --reuid=agent --regid=agent --init-groups env -i HOME=/home/agent PATH="$0" "$@"' "$PATH" "$@"
  ;;
collect)
  # Read agent's outputs only as streams from commands run as agent, so a planted
  # symlink reaches nothing agent can't read. apply checks everything again.
  pkill -KILL -u agent || true
  if [ -n "$("$0" run git -C /home/agent/repo rev-list "$2..HEAD")" ]; then
    # One byte over 10 MB, so apply reports the size instead of a cut bundle.
    "$0" run git -C /home/agent/repo bundle create - HEAD "^$2" | head -c $(((10 << 20) + 1)) >"$3/round.bundle" || true
  fi
  {
    "$0" run cat /home/agent/out/summary.md || true
    if "$0" run test -f /home/agent/out/needs-human.md; then
      printf '\n\nNeeds a person:\n'
      "$0" run cat /home/agent/out/needs-human.md || true
    fi
  } | head -c 20480 >"$3/summary.md" || true
  ;;
*)
  echo "usage: sandbox.sh setup <checkout> <items> | close <gateway ip:port> | run <command...> | collect <round head> <out dir>" >&2
  exit 2
  ;;
esac
