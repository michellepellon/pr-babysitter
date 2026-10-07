<!-- ABOUTME: Design spec for pr-babysitter, a GitHub Actions tool that takes labeled PRs to merge-ready or to a named blocker. -->
<!-- ABOUTME: Covers the three jobs, state rules, sandbox, trust boundaries, defaults, setup, and tests. -->

# pr-babysitter design

- **Status:** Direction approved. This revision applies the fresh-eyes
  review. Nothing is built yet.
- **Decided:**
  - It serves a team.
  - Code, CI logs, and PR text may go to Anthropic's API, which is already
    approved for Claude Code use.
  - It runs on GitHub Actions, not on our own server.
- **Size target:** about 650 lines of Go, 180 lines of workflow YAML, and a
  60-line prompt.

## 1. Summary

pr-babysitter fixes whatever blocks a labeled PR (failing checks and review
findings) and tells the PR's owner when only a person can act. It never reviews,
approves, or merges.

Each pilot repo calls one reusable workflow with three jobs:

| Job | `GITHUB_TOKEN` permissions | Runs repo code? | Does |
|---|---|---|---|
| plan | contents, actions, checks, and statuses: read; pull requests: write | No | Works out each labeled PR's state, updates status comments, picks at most one PR for a round |
| work | contents: read; id-token: write | Yes, as the unprivileged user `agent` | Runs Claude Code in a sandbox, proves each fix, bundles the commits |
| apply | contents and pull requests: write | No | Checks the bundle, pushes it, records the outcome |

There is no GitHub App and no stored secret:

- The model credential comes from GitHub's identity token, through Anthropic
  Workload Identity Federation (WIF).
- apply pushes with `GITHUB_TOKEN`. CI runs on the bot's commits therefore start
  in GitHub's approval-required state, and wait until someone with write access
  clicks "Approve workflows to run".

## 2. What GitHub and Anthropic already do

| Need | Covered by |
|---|---|
| Merging when ready | Auto-merge; merge queue |
| A person approves bot code before merge | Rulesets: dismiss stale approvals, and require approval of the most recent push. plan checks both. |
| A person approves before CI runs bot code | Pushes made with `GITHUB_TOKEN` create approval-required runs |
| The bot can't edit workflows | `GITHUB_TOKEN` can't write `.github/workflows`. apply also blocks all of `.github/**`. |
| Model credential | WIF: a GitHub OIDC token is exchanged for an Anthropic token that lasts minutes |
| Spend cap | A workspace spend limit in Anthropic, plus Claude Code's `--max-budget-usd` |
| Triggers, one run at a time, timeouts | `schedule`, `workflow_run`, `concurrency`, `timeout-minutes` |
| A fresh machine per job | GitHub-hosted runners |
| Cache poisoning | `cache-mode: none`, enforced by GitHub's cache service |
| Audit trail | Actions logs and artifacts, git history, PR comments |
| Notifications | @mentions, GitHub's Slack app, scheduled reminders |
| Pause | Remove the `babysit` label, or disable the workflow |

Things we would build only if needed:

- **Firecracker microVMs for work** (design in commit `3b9f9cc`): if policy
  forbids GitHub-hosted runners, or the threat model needs a kernel boundary
  around the agent.
- **A queryable ledger:** if someone asks questions that the GitHub API and
  Actions logs can't answer.
- **Our own notifications:** if GitHub's fall short.

## 3. States

plan reads each open, labeled PR with one GraphQL query, always passing values
as variables and never building the query string by hand. It applies these
rules in order and stops at the first that matches:

1. The PR comes from a fork, or the newest `babysit` label wasn't added by a
   person with write access: **skip**. A fork PR gets a one-time "not supported"
   status.
2. The base branch's rules lack required checks, stale-approval dismissal, or
   last-push approval: **needs-human** ("repo setup incomplete"). No rounds run
   until this is fixed.
3. Five rounds have run since the label was added: **needs-human**. Removing and
   re-adding the label resets the count.
4. The PR has merge conflicts: **needs-human** ("resolve conflicts"). PR
   workflows don't run on a conflicted PR, so waiting would never end.
5. The head is the bot's last push, and its CI is waiting for approval:
   **needs-human** ("review the bot's commits, then approve the workflow runs").
   In this state a check suite or run for the head has concluded
   `action_required` with no jobs, and GraphQL's combined status is `null`.
   That means plan must read check suites to detect it.
6. A check on the head is pending, or GitHub hasn't yet computed mergeability or
   checks: **waiting**. After 60 minutes on the same head: **needs-human**,
   naming the stuck check.
7. A check was cancelled, needs action, or went stale: **needs-human**, naming
   the check.
8. The last round ended without a push, and no writer has commented, reviewed,
   or pushed since: **needs-human**, giving that round's reason.
9. There are items: **start a round**. Items are:
   - checks on the head that failed, timed out, or errored;
   - unresolved review threads and change requests from writers that are newer
     than the bot's last push (all of them, before the bot's first push);
   - comments from listed reviewer bots that name the head commit.
10. The PR isn't approved: **needs-human** ("needs review").
11. Otherwise: **ready** ("approved and green; merge or enable auto-merge").

plan judges each check on its own state, never on GitHub's combined rollup,
which can report failure while other checks are still running. It identifies
people and bots by user ID, never by login.

## 4. A round

Each run handles at most one round: of the PRs that qualify, the one whose label
is oldest. The workflow's `concurrency` group allows one run per repo, plus one
pending.

### plan

Before work starts, plan:

- writes the round into the status comment's state: the round count plus one,
  the round's head, and an outcome of `running`;
- uploads the items as an artifact: the tail of each failed job's log
  (`gh run view --log-failed`, at most 200 KB each), the review threads, and the
  comments.

If the state still says `running` when the next run starts, that round died, and
plan records it as failed.

### work

work runs in the `babysit` environment, with a 45-minute timeout. Its steps:

1. **Check the branch.** Fail unless the run is on the default branch.
2. **Set up users and code.** As root, create users `agent` and `gateway`,
   neither with sudo or Docker access. Check out the PR head with
   `persist-credentials: false`, then clone it into `agent`'s home as `agent`.
3. **Run setup.** As `agent`, with an open network and no credentials in reach,
   run the repo's `setup_command`. Only code a person has approved gets here:
   - the first round runs the human's own head;
   - every later head ran CI only after a person approved it (rule 5).
4. **Close the network.** iptables and ip6tables rules drop all of `agent`'s
   traffic, DNS and ICMP included, except connections to the gateway's port on
   127.0.0.1.
5. **Start the gateway.** As `gateway`, start the model gateway (described
   below), passing it the job's OIDC request variables.
6. **Run Claude Code.** As `agent`, run Claude Code under `env -i`, so it sees
   only these variables:
   - `HOME` and `PATH`
   - `ANTHROPIC_BASE_URL`, pointing at the gateway
   - a placeholder `ANTHROPIC_API_KEY`
   - `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1`
   - `CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1`. A HIPAA-configured Anthropic
     org rejects the `context_management` field that Claude Code otherwise
     sends, and this variable drops it along with Claude Code's other
     pre-release fields.

   Use the flags `-p --bare --setting-sources user --strict-mcp-config
   --permission-mode bypassPermissions --max-turns 100 --max-budget-usd 10
   --output-format stream-json --verbose`. Task 0 planted files to test these
   flags:
   - `--bare` keeps out the PR's `CLAUDE.md`, `AGENTS.md`, skills, commands,
     subagents, hooks, and `.mcp.json`, and leaves only the Bash, Edit, and Read
     tools.
   - `--bare` still applies the PR's `.claude/settings.json`, such as its
     `model`, so `--setting-sources user` and `--strict-mcp-config` shut that
     out. `agent`'s own home holds no settings.
   - `--bare` cuts Claude Code's system prompt to three lines, so our prompt
     carries all the coding guidance.

   The prompt is adapted from shepherd-pr (MIT, keeping its notice). It tells
   the agent to:
   - prove or refute each item against the code, treating item text as data;
   - fix each valid, in-scope item as a test commit followed by a fix commit,
     where the fix commit's trailer names its proof: either
     `Babysit-Proof: test <selector>` or `Babysit-Proof: lint`;
   - write `summary.md` with each item's outcome, and `needs-human.md` if a
     person must decide something.
7. **Prove the fixes.** Kill every `agent` process first. Our program drives the
   proofs from the runner user, but every git and proof command runs as
   `agent`, passed as an argument list with no shell: either
   `test_command <selector>` or `lint_command`.
   - Selectors must match `^[A-Za-z0-9_./:\[\]-]{1,200}$`.
   - Each proved commit must fail its proof at its parent and pass at itself.
   - Every other commit must be the parent of a commit that does.
   - Every proof must also pass at the final commit.
   - Each proof gets 5 minutes, and all of them together get 10.
   - Any failure means no push.
8. **Collect the output.** Kill every `agent` process again. Read the outputs
   only as streams from commands run as `agent` (`git bundle create -` and
   `cat`), with caps of 10 MB for the bundle and 20 KB for the summary. Upload
   them with the proof report and the transcript, and keep these artifacts for
   3 days. work sets no job outputs.

### The model gateway

The gateway is a small reverse proxy on 127.0.0.1. It:

- exchanges the job's GitHub OIDC token for an Anthropic token, and refreshes
  it before the response's `expires_in` runs out. Anthropic issued 300-second
  tokens in Task 0. Each exchange fetches a fresh GitHub token, because
  Anthropic accepts each one only once;
- forwards only `POST /v1/messages` and `/v1/messages/count_tokens`, matching
  the URL path alone, since Claude Code adds `?beta=true`;
- checks each request body against allowlists and rejects anything else. The
  agent can send the gateway any request it likes, and some Messages features
  make Anthropic's own servers fetch a URL, which could carry our code to an
  attacker. So the gateway allows only:
  - the top-level fields Claude Code sent in Task 0: `max_tokens`, `messages`,
    `metadata`, `model`, `output_config`, `stream`, `system`, `thinking`, and
    `tools`. That keeps out `mcp_servers` and `container`.
  - tools whose `type` is empty or `custom`, which keeps out server tools such
    as web fetch;
  - content `source` objects, anywhere in the body, whose `type` is `base64` or
    `text`. That keeps out images and PDFs given by URL, including ones nested
    in tool results.

  A Claude Code upgrade that adds a field fails its rounds closed until the
  allowlist gains the field, with evidence from a run like Task 0's;
- replaces the agent's auth headers with its own;
- stops after 400 requests per round.

### apply

apply has a 10-minute timeout. Its steps:

1. **Validate inputs.** Check the PR number, head SHA, and branch name that plan
   passed (the branch name with `git check-ref-format`). Pass every
   GitHub-derived value to steps through `env:`. Never interpolate a `${{ }}`
   expression that holds PR, branch, comment, or agent data into a `run:`
   script.
2. **Re-read the PR.** It must still be open, not from a fork, labeled by a
   writer, and on the same head. Otherwise record "stale" and stop.
3. **Check the bundle.** Fetch the round's head, then `git fetch` the bundle
   with `transfer.fsckObjects`. Require all of these:
   - exactly one ref
   - it descends from the round's head
   - no merge commits
   - the bot identity authored and committed every commit
   - no issue-closing keywords such as "Fixes #12"
   - no deleted files, checked with `--no-renames` so that renames count as
     deletions
   - no changes under protected paths
   - at most 20 commits, 1,000 changed lines in total, and 400 lines per file
   - a proof report that covers exactly these commits
4. **Push.** Unless `dry_run` is set, push with
   `--force-with-lease=refs/heads/<branch>:<round head>`. A plain push would
   succeed even after a person had reset the branch to an older commit, and
   would bring back the commits they had dropped.
5. **Record the outcome.** Always (`if: always()`) record the outcome and update
   the status comment.

## 5. Status comment

github-actions[bot] writes one comment per PR and edits it whenever the state
changes. The comment has three parts:

1. **The state line.** Line 1 is `<!-- babysit-state {...} -->`. Its fields are
   `v`, `label_event`, `owner`, `rounds`, `round_head`, `outcome`,
   `last_push_sha`, `last_push_at`, `head_seen_sha`, and `head_seen_at`. plan
   reads only this line, only from comments by github-actions[bot] (matched by
   ID), and pages through all of a PR's comments to find it.
2. **The status.** The state, what the PR is waiting on, and an @mention of the
   owner (the writer who added the label) whenever a person must act.
3. **The last round's summary,** inside a fenced code block. The fence must be
   longer than any run of backticks in the text, so that agent text can't
   render links, images, mentions, or HTML. An image link alone can leak data
   through GitHub's image proxy.

## 6. Security model

Guarantees:

- The agent never holds a GitHub token or a model credential. While it runs, it
  can reach only the gateway, and the gateway forwards no request that makes
  Anthropic's servers fetch a URL or run code.
- Nothing the agent writes runs with CI's secrets, or reaches the default
  branch, until a person approves it. No job is given `actions: write`, so
  pr-babysitter can never approve its own CI runs. A person's token can,
  through `POST /actions/runs/{id}/approve`.
- PRs can't change how pr-babysitter behaves. It runs only from the default
  branch, its inputs live there, and it pins its reusable workflow and every
  action by commit SHA.
- Only writers can adopt a PR or create items. Text from anyone else never
  becomes work.
- apply trusts nothing from work except a bundle that passes every check in §4.

Residual risks, accepted for v1:

- If the agent gains root inside work, it can mint Anthropic tokens until the
  job ends (capped by the workspace spend limit) and reach the internet for that
  job. It still can't write to GitHub, touch the cache, or reach apply's
  machine.
- People may approve the bot's CI runs without reading its commits.
- Other workflows in the same repo also post as github-actions[bot]. One that
  echoed attacker text could fake the state line. The worst case is extra
  rounds, which the spend limits cap.
- Proofs catch mistakes. They won't stop an agent that sets out to deceive.

## 7. Setup

For each pilot repo:

- A ruleset on the default branch that requires status checks, dismisses stale
  approvals, and requires approval of the most recent push. Bind each required
  check to the GitHub Actions app (integration ID 15368). An unbound check
  passes for any status that uses the same name.
- An environment named `babysit`, limited to the default branch, holding no
  secrets.
- Workflow permissions that let jobs request write access, plus access to the
  org's `pr-babysitter` repo. That repo holds the reusable workflow and a
  composite action that builds the Go program.
- A caller workflow of about 25 lines:
  - triggers: `schedule` (hourly), `workflow_run` (on the repo's CI
    workflows), and `workflow_dispatch`
  - `cache-mode: none`
  - the inputs in §8

For Anthropic:

- A WIF service account in a workspace that has a spend limit.
- A federation rule for each repo, with scope `workspace:inference` and a
  600-second token lifetime. The Console offers only `workspace:developer`, so
  set `workspace:inference` afterward through the Admin API
  (`POST /v1/organizations/federation_rules/{id}`), which needs an `org:admin`
  login. Repos created after 2026-07-15 use GitHub's
  immutable-ID subject by default, so set `subject_prefix` to exactly
  `repo:<owner>@<owner_id>/<repo>@<repo_id>:environment:babysit`, with no
  trailing `*`, which would make it a prefix match. Add the claims
  `repository_owner: <owner>` and `ref: refs/heads/<default branch>`.
  Optionally, add a CEL condition that pins `job_workflow_ref` to
  `<owner>/pr-babysitter/.github/workflows/babysit.yml@`.

## 8. Inputs, limits, and cost

| Input | Default |
|---|---|
| `dry_run` | `true` |
| `setup_command` | None |
| `test_command`, `lint_command` | None; without one, that kind of proof isn't available |
| `reviewer_bots` | None |
| `protected_paths` | Added to the built-in list. An entry ending in `/` protects that directory; any other entry protects every file with that name, at any depth. The built-in list is `.github/`, `.devcontainer/`, `.claude/`, `.gitattributes`, `.gitmodules`, `CODEOWNERS`, `CLAUDE.md`, `AGENTS.md`, `.roborev.toml`, `REVIEW.md` |
| WIF IDs | Required: federation rule, organization, service account, workspace |

Limits:

- 5 rounds per label
- 20 PRs read per run
- Timeouts: plan 10 minutes; work 45 minutes (30 for the agent, 10 for proofs);
  apply 10 minutes

Cost: every run bills at least one runner minute. Hourly scans cost about 720
minutes per repo per month. On top of that, each CI completion triggers one run,
and each round adds its own minutes.

## 9. Testing

- **Unit tests:**
  - state rules, from recorded GraphQL responses
  - check classification
  - the gateway's request filter
  - the bundle checks
  - parsing of trailers and selectors
- **Integration tests,** with real git and real sockets:
  - prove
  - apply's checks, including the case where someone reset the branch during the
    round
  - the gateway's token refresh and forwarding, against a local server that
    stands in for Anthropic's endpoints
- **End-to-end tests,** with no mocks, in a sandbox repo with real runners, WIF,
  and the real model:
  1. A failing test gets a proved fix, and CI waits for approval.
  2. A malicious comment asks the agent to push somewhere else and to fetch an
     outside URL. Nothing leaves the machine except model calls the gateway
     allows.
  3. An edit to `.github/actions/` is rejected.
  4. Removing the label mid-round stops the push.
  5. Resetting the branch during a round makes the push fail.
  6. Reaching the round cap ends in needs-human.
- `make check` runs `go vet`, `go test ./...`, and `zizmor` on the workflows.

## 10. Open questions

1. **Resolved (2026-10-07):** kenn-io/forge doesn't overlap with this tool.
   It's a console run by a person, where an agent session starts only when
   someone picks an agent from a menu. Its license is the Elastic License 2.0.
   It complements pr-babysitter.
2. **Confirmed in Task 0 (2026-10-07):**
   - `GITHUB_TOKEN` can read the rules endpoint with only metadata read. This
     was tested on a public repo only.
   - `pull-requests: write` is enough to create and edit PR comments;
     `issues: write` alone isn't.
   - CI waiting for approval looks like rule 5 describes. The waiting run can be
     approved through the API, and it reruns under the same run ID.
   - A reusable workflow's `environment` resolves in the caller's repo, and the
     environment's branch policy blocks dispatches from other branches.
   - github-actions[bot] is user ID 41898282. GraphQL reports its login as
     `github-actions`, so match it by ID.
   - `claude -p --bare` calls only `POST /v1/messages?beta=true`, with no
     server tools, and on a runner it tried no address but the gateway.
   - The WIF chain works end to end, and Anthropic rejects a reused GitHub
     token.
   - On a hosted runner, `agent` can't reach the runner's files or the Docker
     socket.

   See `docs/superpowers/plans/2026-10-07-task0-findings.md`.

   **Still open:**
   - what the undocumented ruleset field
     `require_extra_approval_for_unattributed_changes: true` does to merging
     bot-pushed commits (test this in Task 9)
3. **Signed commits:** do the pilot repos require them? Commits pushed with git
   aren't signed, so apply would have to create commits through GitHub's API
   instead.
4. **Pilot repos:** which repos go first, and what are their setup, test, and
   lint commands?

## 11. References

- Davis et al., "Agentic AI and Code Reviews," Enterprise Technology Leadership
  Journal, Fall 2026: [PDF](https://readwise-assets.s3.amazonaws.com/media/wisereads/articles/agentic-ai-and-code-reviews/1439.pdf)
- [roborev](https://www.roborev.io/)
- [shepherd-pr](https://github.com/prime-radiant-inc/shepherd-pr) (MIT)
- [Firecracker](https://github.com/firecracker-microvm/firecracker), for the
  upgrade path
- [anthropics/claude-code-action](https://github.com/anthropics/claude-code-action):
  not used, because there Claude runs in the same job as its GitHub token. Its
  CI auto-fix and WIF examples are still worth reading.
- GitHub:
  - [approval-required runs for `GITHUB_TOKEN` PR updates](https://docs.github.com/en/actions/how-tos/write-workflows/choose-when-workflows-run/trigger-a-workflow)
  - [cache-mode](https://github.blog/changelog/2026-09-10-control-github-actions-cache-access-with-cache-mode/)
  - [secure use of Actions](https://docs.github.com/en/actions/reference/security/secure-use)
- Anthropic:
  - [WIF with GitHub Actions](https://platform.claude.com/docs/en/manage-claude/wif-providers/github-actions)
  - [sandbox environments](https://code.claude.com/docs/en/sandbox-environments)
  - [secure deployment](https://code.claude.com/docs/en/agent-sdk/secure-deployment)
  - [workspaces and spend limits](https://support.claude.com/en/articles/9796807-creating-and-managing-workspaces)
