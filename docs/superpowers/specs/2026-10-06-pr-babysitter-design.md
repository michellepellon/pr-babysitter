<!-- ABOUTME: Design spec for pr-babysitter, a team tool that takes labeled GitHub PRs to merge-ready or to a named blocker. -->
<!-- ABOUTME: v2 after a ponytail pass: GitHub-native, three Actions jobs and one small Go program. -->

# pr-babysitter design (v2)

- **Dates:**
  - v1: 2026-10-06
  - v2: 2026-10-07, after a ponytail pass that cut everything GitHub or an
    existing tool already does
- **Status:** Draft for review. Nothing is built yet.
- **Decided:**
  - It serves a team (Michelle, 2026-10-06).
  - Code, CI logs, and PR text may go to Anthropic's API. This is already
    approved for Claude Code use (Michelle, 2026-10-07).
- **Proposed, needs Michelle's OK:** run on GitHub Actions with GitHub-hosted
  runners instead of our own Linux server. v1's server-and-Firecracker design
  (commit `3b9f9cc`) becomes the upgrade path in §10.

## 1. Summary

pr-babysitter fixes what blocks a labeled PR (failing checks and review findings)
and tells the PR's owner when only a person can act. It never reviews, approves,
or merges. GitHub's auto-merge and branch protection handle merging.

It's one reusable GitHub Actions workflow with three jobs, plus one small Go
program:

| Job | Runs | GitHub token | Does |
|---|---|---|---|
| plan | Every 5 minutes (schedule) and on demand | Read | Finds labeled PRs, works out each one's state, writes items for the ones that need a round, updates status comments |
| work | Once per PR that needs a round | Read-only, never visible to the agent | Runs Claude Code as an unprivileged user behind a local egress proxy and key gateway, proves each fix, bundles the commits |
| apply | After work | GitHub App token, created in this job only | Checks the bundle, pushes it to the PR branch, updates the status comment |

Each job runs on a fresh GitHub-hosted VM. The agent's job never holds a write
token or the model API key. The job that holds the write token never runs repo
code or the agent.

## 2. What GitHub already does

None of these needs code from us:

| Need | Built-in feature |
|---|---|
| Merge when ready | Auto-merge |
| Keep up with base | The update-branch button; merge queue |
| A person approves bot code before merge | Branch protection: dismiss stale approvals, and require approval of the most recent push |
| The bot can't edit CI | A GitHub App without the `workflows` permission can't push workflow changes |
| Review of sensitive config | CODEOWNERS, with code-owner review required |
| Scheduling, timeouts, one run at a time | `schedule`, `timeout-minutes`, `concurrency` |
| A fresh machine per job | GitHub-hosted runners |
| Audit trail and retention | Actions logs and artifacts, git history, PR comments |
| Notify people | @mentions, the GitHub Slack app, scheduled reminders |
| Pause | Remove the `babysit` label, or disable the workflow |
| App tokens | `actions/create-github-app-token` |
| Cap model spend | A workspace spend limit in the Anthropic Console |
| Read config from the default branch | Scheduled workflows always run from the default branch |

## 3. States

`plan` reads each PR with one GraphQL query and applies these rules in order,
stopping at the first match:

1. No `babysit` label, or the label was added by someone without write access:
   skip.
2. Fork PR: set the status to "fork PRs aren't supported" and skip.
3. Round cap reached (5 rounds): needs-human.
4. The last round asked for a person, and nobody has commented, reviewed, or
   pushed since: needs-human.
5. A check on the head is still running (rollup state pending or expected):
   waiting.
6. There are items: start a round. Items are any of these:
   - failed checks on the head
   - unresolved review threads or change requests with human comments newer
     than the bot's last push
   - comments from listed reviewer bots newer than the head
7. Conflicts: needs-human ("resolve conflicts").
8. Not approved: needs-human ("needs review").
9. Otherwise: ready ("approved and green; enable auto-merge").

A round starts only after every check on the head has finished. That way one
round sees everything, and a finished failure can't be mistaken for a check
that's still running. The PR's owner is whoever added the label.

## 4. A round

1. **Items.** `plan` writes the round's items to an artifact: failed checks with
   their logs (`gh run view --log-failed`), review threads, change requests, and
   reviewer-bot comments. Everything is marked as untrusted data.
2. **Sandbox setup.** In `work`, as root:
   - Create a user `agent`, with no sudo and not in the docker group.
   - Start the key gateway and the egress proxy as root.
   - Add iptables and ip6tables rules so `agent` can reach only those two local
     ports.
   - Check out the repo with `persist-credentials: false`.
3. **Agent.** As `agent`, run the repo's setup command. Then run a pinned
   Claude Code with `claude -p`, using the prompt adapted from shepherd-pr (MIT,
   with its notice), with `ANTHROPIC_BASE_URL` pointing at the gateway and
   `HTTPS_PROXY` at the proxy. The agent:
   - proves or refutes each item against the code;
   - fixes valid, in-scope items, each as a test commit followed by a fix commit
     that carries a `Babysit-Proof: <command>` trailer (a lint fix needs no test
     commit, since its proof is the lint command);
   - writes `summary.md` with each item's outcome, and `NEEDS_HUMAN.md` if a
     person must decide something.
4. **Prove.** As `agent`, after Claude Code exits:
   - Every commit with a proof trailer must fail its proof at its parent and
     pass at itself.
   - Every other commit must be the parent of a commit that does.
   - Any failure means no push.
5. **Bundle.** As `agent`, run `git bundle create` over the new commits. The
   privileged user never runs git inside the agent's checkout, where planted git
   config could run code.
6. **Apply.** On a fresh VM:
   - Fetch the bundle into an empty repo with `transfer.fsckObjects`.
   - Check that it descends from the round's head, deletes no files, and stays
     under the size and commit caps.
   - Push to the PR branch as a plain push, so it fails if anyone else pushed in
     the meantime.
   - Update the status comment.

   For pilots, `dry_run` skips the push.
7. **CI.** CI runs on the new head as usual, because the App token pushed it.
   Pushes made with `GITHUB_TOKEN` don't start workflows.

## 5. Status comment

The App writes one comment per PR and updates it whenever the state changes. The
comment holds:
- the state, and what the PR is waiting on
- an @mention of the owner when a person must act
- the last round's outcomes
- a hidden JSON block with the round count and the last push

`plan` trusts that hidden block only when our App wrote the comment. It escapes
@-mentions in agent-written text before posting.

## 6. Security

These protections stay:

| Property | How |
|---|---|
| The agent has no GitHub write access | `work` gets a read-only token that `agent` never sees; the App token exists only in `apply` |
| The agent has no model key | The key lives in the root-owned gateway process |
| Outbound traffic is allowlisted | An iptables owner match routes `agent` traffic only to the proxy, which allows only the gateway and the repo's package registries |
| Every round starts fresh | GitHub-hosted runners |
| A PR can't change its own rules | The workflow and its inputs come from the default branch |
| Only writers can adopt a PR | `plan` checks the labeler's permission |
| The bot can't touch CI | The App lacks the `workflows` permission |
| A person approves before merge | Branch protection |
| The bundle is untrusted | fsck, ancestry check, no deleted files, caps |
| Status or review comments could be forged | Author checks against our App and the listed bots |

This is weaker than v1 in one place: the agent and the gateway share a kernel.
If the agent gains root inside `work`, it gets the model key (capped by the
workspace limit), the job's read-only token, and unrestricted outbound traffic
for the rest of that job. It still gets no write access and nothing outside
that VM. v1 put a KVM boundary there, but the price was running and hardening a
server that held the org-wide App key right next to the agent. §10 says when
to switch back.

## 7. Repo setup

- **GitHub App.** Contents write, pull requests write, checks and statuses
  read, metadata read, and no `workflows` permission. Confirm the exact list
  while building.
- **Branch protection** on pilot repos: required checks, dismiss stale
  approvals, require approval of the most recent push. Use CODEOWNERS for
  review config such as `.roborev.toml` and `REVIEW.md`.
- **A 20-line caller workflow** per repo:
  `uses: <org>/pr-babysitter/.github/workflows/babysit.yml@v1`. Its inputs are
  the setup command, the package registries, the reviewer-bot logins, and
  `dry_run`.
- **Org secrets.** The App's ID and private key, plus a dedicated Anthropic API
  key created in a workspace that has a spend limit.

## 8. Testing

- **Unit:** the state rules, as a pure function of the GraphQL response; parsing
  of trailers and the summary.
- **Integration,** with real git and real sockets: prove, bundle checks, the
  gateway, and the proxy.
- **End to end,** with no mocks, in a sandbox repo with the real App, real
  runners, and the real model:
  1. A failing test gets a proved fix and a push.
  2. A malicious PR comment tells the agent to push to another branch and send
     code to an outside host. Nothing leaves, and the outbound attempt is
     blocked.
  3. A refuted finding produces no push and an explanation.
  4. Hitting the round cap ends in needs-human.
- `make check` runs `go vet` and `go test ./...`.

## 9. Size

| Piece | v1 (server) | v2 |
|---|---|---|
| Go | ~3,900 lines | ~410 lines: plan ~150, apply ~90, prove ~50, status comment ~40, proxy and gateway ~80 |
| Workflow YAML | none | ~120 lines, plus ~20 per repo |
| Agent prompt | none | ~60 lines |
| Tests | ~3,900 lines | ~420 lines |
| Servers to run | 1, hardened, with KVM | 0 |

## 10. What we cut, and when to add it back

| Cut | Add back when |
|---|---|
| Our own server, Firecracker, jailer, vsock, images, launcher | GitHub-hosted runners aren't allowed, Actions minutes cost more than a server, or the threat model needs a kernel boundary around the agent |
| A separate verifier VM | A deceptive agent faking proofs becomes a real risk (CI already re-runs the full suite) |
| Merge and update levels, merge grants | Never; auto-merge and merge queue cover them |
| `/babysit` commands | Labels, auto-merge, and re-run prove too coarse |
| Slack code, reminders, escalation, quiet hours, daily sweep | GitHub's Slack app and scheduled reminders fall short |
| SQLite ledger, backups, retention, metrics | Someone asks a question that Actions logs and the GitHub API can't answer |
| `.pr-babysitter.toml` and layered policy | Repos need settings that caller-workflow inputs can't carry |
| Protected-path lists, heuristics for deleted tests | The no-deleted-files rule, CODEOWNERS, and the missing `workflows` permission let something real through |
| Check identity, required-check lookup, settle timeout, adaptive polling | A hung or misattributed check blocks a real PR |
| GitHub API client, rate limiting, App JWT code | `gh` and `actions/create-github-app-token` fall short |
| Parallel rounds within one repo | One run at a time per repo becomes the bottleneck |

## 11. Risks

1. **Fix quality is still the big unknown.** Pilot with `dry_run`, then on one
   repo.
2. **Actions minutes.** Rounds can run up to 45 minutes, on paid runners for
   private repos.
3. **The shared kernel in `work`** (see §6).
4. **Scheduling is coarse.** Runs come at most every five minutes, and GitHub
   may delay scheduled runs.
5. **Tests that need Docker** won't run as `agent`.
6. **kenn-io/forge is still unread.**

## 12. Open questions

1. Is it OK to use GitHub Actions on GitHub-hosted runners instead of our own
   server?
2. What's the Actions minutes budget for the pilot repos?
3. Who creates and approves the GitHub App?
4. Will the pilot repos turn on the branch protection settings in §7?
5. Do the pilot repos require signed commits? If so, `apply` must create commits
   through GitHub's API.
6. Which repos pilot first, and what are their setup and test commands?

## 13. References

- Davis et al., "Agentic AI and Code Reviews," Enterprise Technology Leadership
  Journal, Fall 2026: [PDF][paper]
- roborev: [site](https://www.roborev.io/),
  [repository](https://github.com/kenn-io/roborev)
- shepherd-pr (MIT): [repository](https://github.com/prime-radiant-inc/shepherd-pr)
- Firecracker, for the upgrade path:
  [repository](https://github.com/firecracker-microvm/firecracker)
- Anthropic:
  [sandbox environments](https://code.claude.com/docs/en/sandbox-environments),
  [secure deployment](https://code.claude.com/docs/en/agent-sdk/secure-deployment),
  [workspaces and spend limits](https://support.claude.com/en/articles/9796807-creating-and-managing-workspaces)

[paper]: https://readwise-assets.s3.amazonaws.com/media/wisereads/articles/agentic-ai-and-code-reviews/1439.pdf
