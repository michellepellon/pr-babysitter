<!-- ABOUTME: Results of Task 0, the throwaway platform spike that tested the spec's §10.2 assumptions. -->
<!-- ABOUTME: Each answer names the run that produced it and its evidence in testdata/task0/. -->

# Task 0 findings (2026-10-07)

Every run below is in `michellepellon/pr-babysitter-sandbox`, a public repo
created on 2026-10-07, at `https://github.com/michellepellon/pr-babysitter-sandbox/actions/runs/<id>`.
The raw responses are in `testdata/task0/`, scanned for secrets before commit.

**Status:** questions 1–5 are answered. Question 6 is answered offline, and the
runner check waits on question 7. Question 7 fails at the token exchange until
the federation rule is fixed.

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

Still to check on a runner, through the real API: whether Claude Code calls
anything else after a real response, and whether it tries any address other
than the gateway. The spike workflow `spike-q67-wif.yml` does both once
question 7 passes.

## 7. The WIF chain

Run 37656065511 fetched GitHub's token with the expected claims (`sub` as in
question 4, `aud` `https://api.anthropic.com`, `ref` `refs/heads/main`). The
exchange at `POST /v1/oauth/token` returned Anthropic's opaque
`401 Authentication failed`. The likeliest cause is the federation rule still
using the `repo:michellepellon/...` subject that the plan first gave. The
Console's Workload identity History tab records the real reason. No token
appeared in the public log.

Anthropic's docs add three facts the design needs:

- `subject_prefix` is an exact match unless it ends in `*`.
- Each GitHub token can be exchanged only once (`jti` replay protection), so
  every refresh must fetch a new one.
- `workspace:inference` is the narrowest scope that allows Messages and token
  counting.

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
