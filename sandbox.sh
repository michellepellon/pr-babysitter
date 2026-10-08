#!/usr/bin/env bash
# ABOUTME: Root setup for babysit.yml's work job: the agent and gateway users, agent's clone, Claude Code, the firewall.
# ABOUTME: Usage, as root: sandbox.sh setup <checkout> <items> | close <port> | collect <round head> <out dir>.
set -euo pipefail

# Task 0 ran this version on a runner, and the gateway's allowlist rests on what it sent.
claude_code_version=2.1.292
# apply compares "name <email>" with BABYSIT_BOT in babysit.yml exactly.
bot_name='github-actions[bot]'
bot_email='41898282+github-actions[bot]@users.noreply.github.com'

# as_agent runs a command as agent with only HOME and PATH set, so nothing
# agent runs (npm's install scripts included) sees the job's OIDC variables.
as_agent() { (cd /home/agent && sudo -u agent env -i HOME=/home/agent PATH="$PATH" "$@"); }

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
  # agent may reach only the gateway; everything else, DNS and ICMP included, is refused.
  iptables -I OUTPUT 1 -m owner --uid-owner agent -d 127.0.0.1 -p tcp --dport "$2" -j ACCEPT
  iptables -I OUTPUT 2 -m owner --uid-owner agent -j REJECT
  ip6tables -I OUTPUT 1 -m owner --uid-owner agent -j REJECT
  pkill -KILL -u agent || true # nothing left from setup keeps running
  ;;
collect)
  # Read agent's outputs only as streams from commands run as agent, so a planted
  # symlink reaches nothing agent can't read. apply checks everything again.
  pkill -KILL -u agent || true
  if [ -n "$(as_agent git -C /home/agent/repo rev-list "$2..HEAD")" ]; then
    # One byte over 10 MB, so apply reports the size instead of a cut bundle.
    as_agent git -C /home/agent/repo bundle create - HEAD "^$2" | head -c $(((10 << 20) + 1)) >"$3/round.bundle" || true
  fi
  {
    as_agent cat /home/agent/out/summary.md || true
    if as_agent test -f /home/agent/out/needs-human.md; then
      printf '\n\nNeeds a person:\n'
      as_agent cat /home/agent/out/needs-human.md || true
    fi
  } | head -c 20480 >"$3/summary.md" || true
  ;;
*)
  echo "usage: sandbox.sh setup <checkout> <items> | close <port> | collect <round head> <out dir>" >&2
  exit 2
  ;;
esac
