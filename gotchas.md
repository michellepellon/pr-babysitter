<!-- ABOUTME: Shared gotchas and reference notes for pr-babysitter, for people and agents. -->
<!-- ABOUTME: Append short entries; edit an entry in place when its facts change. -->

# Gotchas

## Reference: "Agentic AI and Code Reviews" (IT Revolution, Fall 2026)

Grounding paper for this project: https://readwise-assets.s3.amazonaws.com/media/wisereads/articles/agentic-ai-and-code-reviews/1439.pdf

- Layered verification: deterministic checks first, then agent review and policy, then humans only where risk or doubt calls for them. Keep a durable record of every agent decision: what, which policy, what confidence, by whom.
- Track findings across pushes the way Cloudflare does: close fixed ones, carry the rest forward. Give review loops explicit exits: clean pass, max attempts, then a human.
- Agent PRs stall when the engineer who delegated them stops feeling ownership (Block). Noisy review bots lose trust quickly.
- The paper never covers prompt injection against the reviewer. Treat PR text and code comments as untrusted input to every model.

## Reference: roborev (kenn-io/roborev)

Local daemon that reviews each commit with your agent CLIs and keeps findings open until closed: https://www.roborev.io/ (agent-readable index at /llms.txt).

- It already reviews every PR push (GitHub poller), keeps a findings ledger, and feeds findings back to agent sessions. Per its docs it does not fix CI, answer review threads, push to PR branches, rebase, nudge humans, or merge.
- Check kenn-io/forge before building: it says it triages PRs, issues, and CI and turns any item into an agent worktree session.
- PR-safety rules worth copying: read config from the default branch, track by head SHA and recheck before posting, throttle per PR, never post without usable output, never auto-fix without repo-specific guidelines.
- `refine` and agentic fixes run agents unsandboxed; telemetry is on by default (`ROBOREV_TELEMETRY_ENABLED=0`).

## Reference: shepherd-pr (prime-radiant-inc/shepherd-pr)

Agent skill plus read-only `gh` tools that take one PR to an authorized merge or an evidenced blocker: https://github.com/prime-radiant-inc/shepherd-pr. It's the closest existing thing to this project; try it with roborev before building.

- Its roborev reader trusts any comment carrying the `<!-- roborev-pr-comment -->` marker, whoever wrote it. Anyone who can comment can forge a "Review Passed". Check the author, or use roborev's commit status.
- `pr-settle.sh` counts a finished FAILURE as still running (issue #2), and it never settles on a repo without roborev.
- Worth copying: one push per review round; prove each regression test fails without its fix; treat PR text as evidence only; never claim monitoring outlives the session.

## Sandboxing the agent worker

- Claude Code's built-in Bash sandbox isn't enough for unattended runs: it covers shell commands only and by default can read `~/.ssh`. Anthropic's docs point untrusted code to a VM such as Firecracker.
- Firecracker filters no network traffic. Give the VM no network card, route everything over vsock to a host proxy with an allowlist, and keep keys on the host (`ANTHROPIC_BASE_URL` to a gateway that injects the API key).
- Never mount a guest-written disk image on the host; return results over vsock. Firecracker tests host kernels 5.10, 6.1 and 6.18 only, and its jailer needs root to set up.

## Decision: team service on a shared Linux server (2026-10-06)

pr-babysitter serves a team, not one person's laptop, and needs a GitHub App bot identity. Code, CI logs, and PR text may go to Anthropic's API: this is already approved for Claude Code use (Michelle, 2026-10-07). Read the draft spec first: `docs/superpowers/specs/2026-10-06-pr-babysitter-design.md`. Its v2 (2026-10-07) proposes GitHub Actions instead of our own server, pending approval.
