<!-- ABOUTME: Results of Task 0, the throwaway platform spike that tested the spec's §10.2 assumptions. -->
<!-- ABOUTME: Each answer names the run that produced it and its evidence in testdata/task0/. -->

# Task 0 findings (2026-10-07)

Every run below is in `michellepellon/pr-babysitter-sandbox`, a public repo
created on 2026-10-07, at `https://github.com/michellepellon/pr-babysitter-sandbox/actions/runs/<id>`.
The raw responses are in `testdata/task0/`, scanned for secrets before commit.

**Status:** all seven questions are answered. The last section adds a check
the spec had assumed: what the `agent` user can reach on a hosted runner.

## 1. Can `GITHUB_TOKEN` read the branch rules?

Yes, but only proven on a public repo. `GET /repos/{o}/{r}/rules/branches/main`
returned the rules to a job with `permissions: {}` (which still grants
`metadata: read`), and to a request with no token at all. Test a private repo
before piloting on one.

The `pull_request` rule carries `require_extra_approval_for_unattributed_changes: true`,
which GitHub doesn't document; Task 9 finds out what it does. Required checks
match by name unless bound to an app, so §7 now binds each to GitHub Actions
(app ID 15368, from `performed_via_github_app` on the bot's comment).

Run 37653013880. Evidence: `q1-*`, `setup-ruleset-*`, `token-permissions-per-job.txt`.

## 2. The smallest permission for PR comments

`pull-requests: write` creates and edits a PR comment. `issues: write` alone
gets a 403 on create.

Run 37653161245. Evidence: `q2-*`.

## 3. CI that waits for approval

After a `GITHUB_TOKEN` push to the PR branch, the `ci` run's first attempt
concluded `action_required` with no jobs, and GraphQL's `statusCheckRollup` for
the head was `null`. Only the head's check suites show this state, so plan must
read them (spec rule 5).

Michelle's token approved the run with `POST /repos/{o}/{r}/actions/runs/{id}/approve`
(201). It reran as attempt 2 under the same run ID, with `triggering_actor` set
to the approver, and passed.

Separately, one push to `main` started two `ci` runs for the same SHA, a second
apart. State rules must judge every run of a check, not the first one found.

Runs 37653325401 (the push) and 37653357605 (the waiting CI run). Evidence:
`q3-*`, `setup-main-push-runs.json`.

## 4. The environment and OIDC subject of a reusable workflow

A sandbox job that calls `pr-babysitter`'s reusable workflow runs in the
sandbox's own `babysit` environment. Its token's `sub` is
`repo:michellepellon@122621769/pr-babysitter-sandbox@1409071530:environment:babysit`:
GitHub's immutable-ID format, the default for repos created after 2026-07-15.
Federation rules must use that form. The environment's branch policy rejected a
dispatch from another branch before any runner started.

Runs 37653409062 (`main`) and 37653758686 (another branch). Evidence: `q4-*`.

## 5. github-actions[bot]'s identity

REST reports login `github-actions[bot]`, ID 41898282. GraphQL reports login
`github-actions`, `__typename: Bot`, and `databaseId: 41898282`. Match the
status comment's author on the ID, never the login.

Evidence: `q5-*`, `q2-pull-requests-write-POST-response.json`.

## 6. What `claude -p --bare` sends

Tested offline with Claude Code 2.1.290 in a fresh `HOME`, against a local stub
that records each request and answers 400. Evidence: `q6-offline-bare.json`.

- **Endpoints:** only `POST /v1/messages?beta=true`. The gateway must match on
  the URL path, since the query string is there.
- **Requests:** custom tools only (Bash, Edit, Read) and no server tools. The
  system prompt is three lines and the tool descriptions are one line each, so
  our prompt carries all the coding guidance. There is no Write tool; new files
  go through Bash.
- **Repo files:** the working directory held a planted `CLAUDE.md`, `AGENTS.md`,
  skill, command, subagent, SessionStart hook, and `.mcp.json` server, plus
  `enableAllProjectMcpServers: true`. None of them reached a request or ran.
  But `--bare` still applied the repo's `.claude/settings.json`: its `model`
  took effect. Adding `--setting-sources user --strict-mcp-config` stopped that,
  and the spec now uses both flags.
- **Stdin:** `claude -p` waits for stdin to close when it isn't a terminal. Run
  it with `< /dev/null`.

On a runner, Claude Code 2.1.292 ran as `agent` behind the logging proxy, with
the spec's flags and a two-turn task (run 37673094894, $0.011):

- Both requests were `POST /v1/messages?beta=true`, both answered 200, and the
  task finished. No token counting, no `HEAD /api/hello`, no other path.
- The default model was `claude-opus-5-5`.
- The firewall logged no attempt by `agent` to reach anything but the proxy.
  A deliberate blocked request afterward logged its DNS packets, so the log
  rule works and the silence means something.
- Our Anthropic org is HIPAA-configured and rejects Claude Code's
  `context_management` field with a 400 (run 37672087512). Claude Code doesn't
  retry that error, so the run failed. `CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1`
  removes the field, leaving these betas: `claude-code-20250219`,
  `interleaved-thinking-2025-05-14`, `mid-conversation-system-2026-04-07`,
  `effort-2025-11-24`. The body's fields were then `max_tokens`, `messages`,
  `metadata`, `model`, `output_config`, `stream`, `system`, `thinking`, and
  `tools`.

Evidence: `q67-runner-37672087512.txt`, `q67-runner-37673094894.txt`.

## 7. The WIF chain

It works. In run 37673094894 the job fetched GitHub's token, exchanged it at
`POST /v1/oauth/token` for a Bearer `sk-ant-oat01-` token, and called
`/v1/messages` with it. Exchanging the same GitHub token a second time got a
401, so Anthropic's `jti` replay check is on, and every refresh must fetch a
new GitHub token. No token appeared in any public log.

What it took to get there:

- The first three runs got Anthropic's opaque `401 Authentication failed`. The
  Console's Workload identity History tab gave the real reasons. The rule first
  had the old `repo:michellepellon/...` subject. After that was fixed,
  `match_claim_absent`: a claim named `refs` instead of `ref`. Claim names must
  match GitHub's exactly.
- `subject_prefix` is an exact match unless it ends in `*`.
- The Console offers only the `workspace:developer` scope. Narrowing the rule
  to `workspace:inference` takes the Admin API
  (`POST /v1/organizations/federation_rules/{id}`) and an `org:admin` login.
- Anthropic issued 300-second tokens. The gateway must refresh from
  `expires_in`, not from an assumed lifetime.

## What the `agent` user can reach on a hosted runner

The spec says setup runs "with no credentials in reach" without saying how.
On a GitHub-hosted runner (runner 2.337.0):

- `/home/runner` is mode 750, so `agent` can't enter the runner's install
  directory (`/home/runner/actions-runner`), the workspace, or `_temp`. That is
  why the spec clones the PR into `agent`'s home.
- `agent` can't write the Docker socket.
- A search of the whole disk for credential-shaped files that `agent` can read
  (`.credentials*`, `.runner`, `*.key`, `.netrc`, `.git-credentials`,
  `hosts.yml`, `.docker/config.json`) found 15, all test fixtures shipped
  inside open-source packages under `/usr/share/miniconda/pkgs` and
  `/usr/lib/google-cloud-sdk`.
- `agent` can't read the proxy's token file (mode 600, owned by the runner
  user).

This search matched on file names, so it can miss a secret stored under an
unusual name.

## What the spike left behind

- Sandbox `main`: `ci.yml`, the spike workflows, the Go module, and
  `spike/q6proxy`.
- Sandbox ruleset 24664123 on `main`: requires the `test` check and one approval
  of the last push; admins can bypass it.
- Sandbox environment `babysit` (ID 23700782510), limited to `main`.
- Sandbox PR #1 from `spike/test-pr`, and the branch `spike/q4-nonmain`.
- The tool repo's branch `spike/task0` (5f3a274), holding the workflow
  question 4 called.

Task 9 needs the ruleset and environment. Delete the rest once its end-to-end
suite replaces them.
