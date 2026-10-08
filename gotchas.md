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

## Reference: forge (kenn-io/forge)

forge is a local maintainer console from the roborev team, under the Elastic License 2.0. A person starts each agent session from a menu, and nothing runs unattended, so it complements pr-babysitter instead of replacing it. Its sync notes back up two of our rules: an "unknown" mergeable state never counts as an observation, and actions are refused on a stale or unknown head.

## Sandboxing the agent worker

- Claude Code's built-in Bash sandbox isn't enough for unattended runs: it covers shell commands only and by default can read `~/.ssh`. Anthropic's docs point untrusted code to a VM such as Firecracker.
- Firecracker filters no network traffic. Give the VM no network card, route everything over vsock to a host proxy with an allowlist, and keep keys on the host (`ANTHROPIC_BASE_URL` to a gateway that injects the API key).
- Never mount a guest-written disk image on the host; return results over vsock. Firecracker tests host kernels 5.10, 6.1 and 6.18 only, and its jailer needs root to set up.

## Decision: team service on GitHub Actions (2026-10-07)

pr-babysitter serves a team. It runs on GitHub Actions in each pilot repo, with no server, no GitHub App, and no stored secrets (decided 2026-10-07). Code, CI logs, and PR text may go to Anthropic's API; that is already approved for Claude Code use. Read the spec first: `docs/superpowers/specs/2026-10-06-pr-babysitter-design.md`. It lives on Michelle's personal account for now (`michellepellon/pr-babysitter` and `-sandbox`) and moves to the org later. After the move, update the module path, callers' `uses:` lines, and the WIF federation rule.

## Workflow and agent security (verified 2026-10-07)

- Push the bot's fixes with `GITHUB_TOKEN`. CI runs on those pushes then wait for a writer to approve them, so CI secrets never run unreviewed agent code. App and personal tokens skip that approval.
- Never interpolate a `${{ }}` holding PR, branch, comment, or agent data into a `run:` script; pass it through `env:`. Branch names may contain `$(...)`. Pin every action and reusable workflow by commit SHA, and set `cache-mode: none`.
- Get the model credential from Anthropic Workload Identity Federation. Any writer can read a repo's secrets, so don't store keys. Repos created after 2026-07-15 get an immutable-ID subject, `repo:<owner>@<owner_id>/<repo>@<repo_id>:environment:babysit`. A rule's `subject_prefix` matches exactly unless it ends in `*`. Claim names must match GitHub's exactly: a rule with `refs` instead of `ref` failed as `match_claim_absent`. Use scope `workspace:inference`; the Console offers only `workspace:developer`, so narrowing takes the Admin API and an `org:admin` login.
- Each GitHub OIDC token can be exchanged with Anthropic only once (`jti`), so every refresh fetches a new one. Every denial is an opaque `401 Authentication failed`; the Console's Workload identity History tab shows the reason.
- The model gateway must allowlist what it forwards, and fail closed. The agent can call the gateway directly, and `mcp_servers`, `container`, server tools, and images or PDFs given by URL (even inside a `tool_result`) all make Anthropic's servers reach the network, which can carry repo code to an attacker's URL.
- Push with `--force-with-lease=<ref>:<round head>`. A plain push succeeds after someone resets the branch, and restores the commits they dropped.

## Platform facts from Task 0 (2026-10-07)

Evidence and run links: `docs/superpowers/plans/2026-10-07-task0-findings.md`.

- CI waiting for approval is invisible in GraphQL: `statusCheckRollup` is `null`. Read the head's check suites, where it shows as `action_required` with no jobs.
- One push can start two runs of the same check on one SHA. Judge every run.
- Match github-actions[bot] on user ID 41898282. GraphQL calls it `github-actions`, REST `github-actions[bot]`. Bind required checks to the Actions app (ID 15368), or any status with the same name passes them.
- `claude -p --bare` still applies the repo's `.claude/settings.json` (a planted `model` took effect). Add `--setting-sources user --strict-mcp-config`. It calls `POST /v1/messages?beta=true`, so match the gateway's allowlist on the URL path. Run it with `< /dev/null`, or it waits for stdin.
- Our Anthropic org is HIPAA-configured and rejects Claude Code's `context_management` field with a 400 that Claude Code doesn't retry. Set `CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1`.
- Anthropic issued 300-second tokens in Task 0. Refresh from `expires_in`, not from the rule's configured lifetime.
- On GitHub-hosted runners `/home/runner` is mode 750, so a separate `agent` user can't reach the runner's files, the workspace, or the Docker socket. Clone the PR into `agent`'s home.

## Build notes from Tasks 2–6 (2026-10-07)

- The status comment's state line is safe only because `json.Marshal` escapes `<`, `>`, `&` and newlines. An encoder with `SetEscapeHTML(false)` would let a field close the `<!-- -->` comment. GitHub may return comment bodies with CRLF, so trim `\r` from line 1.
- The gateway forwards the body it parsed, re-encoded, not the agent's bytes. Otherwise a body with a repeated key could pass our filter one way and reach Anthropic another way.
- The gateway refuses any `source` that isn't a base64 or text object, so a future tool with a `source` parameter in its `input_schema` would fail rounds closed.
- The exact WIF exchange request (grant type, `assertion`, and the four IDs) is copied from the sandbox repo's `spike-q67-wif.yml` into `gateway.go`. The findings doc says to delete that spike after Task 9; `gateway.go` is now the record.
- Pass `refs/heads/<name>` to `git check-ref-format`, not `--branch`, which expands `@{-1}`. Branch names can't hold spaces but can hold `$(...)` and `${IFS}`.
- A lease that names an explicit SHA ignores the clone's tracking refs, so it holds even when apply's clone is stale. `git fetch <bundle> <sha>` keeps the bundle's attacker-chosen ref name away from git.
- The runner user can't signal `agent`'s processes (EPERM); kill them with `sudo -u agent kill -KILL -- -<pgid>`. Set `WaitDelay` on commands whose output is captured, or a process that leaves the group and keeps the pipe open hangs `Wait`.
- Test repos need `GIT_CONFIG_GLOBAL=/dev/null` and `GIT_CONFIG_NOSYSTEM=1`, so a developer's own git settings, such as commit signing, don't break test commits.

## Build notes from Task 7 (2026-10-07)

- Pass strings to `gh api` with `-f`, not `-F`. `-F` turns a value starting with `@` into a file's contents, and `true` or a number into JSON types.
- With `--paginate`, add `--jq '.[]'` (or `'.check_suites[]'`) and decode the output as a stream of JSON values. That avoids relying on how gh joins pages. Don't use `--jq` to pick a string field: gh prints strings bare, not as JSON.
- gh replaces `{owner}`, `{repo}`, and `{branch}` in an endpoint. Path-escape any GitHub-supplied path segment (plan escapes the base branch) so braces in a branch name stay literal.
- Re-record `testdata/plan/graphql-prs.json` whenever `prQuery` changes. The decode tests caught an alias rename only because the fixture came from the real query. Sandbox PR #1 carries the `babysit` label for this.
