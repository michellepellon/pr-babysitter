<!-- ABOUTME: Design spec for pr-babysitter, a team service that takes GitHub PRs to an authorized merge or a named blocker. -->
<!-- ABOUTME: Draft for review: decisions, architecture, security model, milestones, and open questions. -->

# pr-babysitter design

- **Date:** 2026-10-06
- **Status:** Draft for review. Nothing here is built yet.
- **Decided:** pr-babysitter serves a team and runs on a shared Linux server
  (Michelle, 2026-10-06).

## 1. Summary

pr-babysitter takes each GitHub pull request a person hands it from open to
merged, or to a blocker that names who must act, without anyone watching. It
differs from [shepherd-pr][shepherd] in one way that matters. shepherd-pr writes
its rules as instructions to a model; pr-babysitter turns them into code:

- A **controller** with no model decides what each PR is waiting on.
- A **worker** agent runs only when a step needs judgment, inside a throwaway
  Firecracker microVM that holds no credentials.
- A **verifier** re-runs the tests and checks the worker's proof that each fix
  works.
- A **gate**, the only part with a GitHub write token, makes every change on
  GitHub after checking policy and human grants.

Review stays with [roborev][roborev] and people. pr-babysitter never reviews code
and never approves a PR.

## 2. Goals and non-goals

Goals:

- Shepherd many PRs across a team's repos, unattended, surviving restarts.
- Fix failing checks and valid review findings, one push per round, with a test
  that proves each fix.
- Make unsafe actions impossible in code instead of discouraged in a prompt.
- Tell the right person promptly when only a person can act.
- Record every observation, decision, and write.
- Measure whether it helps: time to merge, human touches, fixes that stick.

Non-goals:

- Reviewing code. roborev and people do that.
- Approving PRs or bypassing branch protection, ever.
- A webhook server, a web UI, or a hosted service.
- GitLab or GitHub Enterprise in v1. Keep the API base URL configurable.
- Agents other than Claude Code in v1.
- Changing fork PRs from people without write access. Those get observe and
  notify only.

## 3. Background

Three sources, read on 2026-10-06, shape this design:

- **"Agentic AI and Code Reviews"** ([IT Revolution, Fall 2026][paper]) supplies
  the frame: layered verification, human involvement tiered by risk, durable
  records, and "no human in the loop" as a configuration choice backed by
  evidence. Its gaps (attacks on the reviewer, no quality data, circular
  validation, triage as a single point of failure) drive §11 and §13.
- **roborev** is a local review daemon with a findings ledger and a GitHub
  poller. We use it as a reviewer and copy its PR rules: config from the default
  branch, track by head SHA and recheck before posting, throttle per PR, never
  post without usable output, never auto-fix without repo guidelines.
- **shepherd-pr** (MIT) is Jesse Vincent's PR-shepherding skill. We adapt its
  triage rules for the worker prompt, keeping its license notice, and avoid its
  known problems: review signals anyone can forge, failed checks read as still
  running, no CI-only mode, and checks keyed by name alone.

Isolation follows Anthropic's guidance for unattended agents that read untrusted
input ([sandbox environments][anthropic-envs], [secure deployment][anthropic-secure]):
a VM with no network interface that talks over vsock to a host proxy, which
enforces an allowlist and injects credentials.

## 4. Architecture

| Part | Job | Model? | GitHub access | Runs |
|---|---|---|---|---|
| Controller | Polls GitHub, derives each PR's state, starts rounds, records everything | No | Read | Host: `pr-babysitter tick` |
| Worker | Fixes the round's items in a scratch clone | Claude Code | None | Fresh microVM |
| Verifier | Runs the fix proofs and the full test commands | No | None | Second fresh microVM |
| Gate | Checks policy and grants, then pushes, comments, labels, or merges | No | Write (GitHub App) | Host |
| Launcher | Starts microVMs through Firecracker's jailer | No | None | Host, privileged, narrow |
| Proxy and gateway | The only route out of a microVM; the gateway adds the model API key | No | None | Host |

One round, end to end:

```
controller  PR is "fixable": write round spec + git bundle + policy to the round dir
launcher    boot worker VM: read-only image, scratch disk, vsock only
worker      agent fixes items, one commit per item, writes evidence.json,
            streams bundle + evidence back over vsock
controller  validate the bundle on the host: fsck, size, protected paths, test deletions
launcher    boot verifier VM: per-commit proofs, then the full test commands
gate        every hard check passed and policy allows: push, update status comment
controller  PR is "waiting" until CI and review finish for the new head
```

## 5. PR lifecycle

### 5.1 States

On every tick the controller derives one state per adopted PR from GitHub and
the ledger. It applies these rules in order and stops at the first match:

1. Merged or closed: **done**.
2. A `babysit:hold` label or the global kill switch: **paused**.
3. A blocker only a person can clear (round budget spent, last round failed
   verification, push denied, the worker asked for a decision, a fork PR that
   needs changes): **needs-human**.
4. The head hasn't settled (a required check or a configured review is still
   running for it): **waiting**. One exception: once a check has failed, stop
   waiting after a settle timeout (default 20 minutes).
5. A required check failed on the head, or the head has open findings at or
   above the threshold, open change requests, or conflicts: **fixable**. If the
   repo's level forbids that fix, **needs-human** instead.
6. An approval or another human-only gate is missing: **needs-human**.
7. Otherwise: **ready**.

| State | Action |
|---|---|
| done | Stop; record the outcome |
| paused | Nothing |
| waiting | Poll again; escalate if a check stays queued past a limit |
| fixable | Start one worker round |
| needs-human | Update the status comment, notify the owner, escalate on a timer |
| ready | Merge if the level and a grant allow it; otherwise notify the owner |

Rounds start only on a settled head, so each round sees every failure and
finding at once, and a failed check never looks like a running one.

### 5.2 Observation rules

- **Check identity.** A check run is app + workflow + name; its current run is
  the newest within that identity for the head SHA. A commit status is its
  context.
- **Check classes.** Success, neutral, and skipped pass. Failure, timed out, and
  startup failure fail. Queued and in progress are pending. Action required is
  needs-human. A cancelled run on the head gets one automatic rerun if policy
  allows, then needs-human.
- **Required checks** come from the base branch's protection rules or rulesets.
  Confirm the exact API in M1. Failures in checks that aren't required get
  reported but don't block ready.
- **Pre-existing failures.** If the same check also fails on the base branch's
  latest commit, mark the failure pre-existing. Report it with evidence and an
  owner, and don't try to fix it in this PR.
- **Reviews** are adapters:
  - GitHub reviews and unresolved review threads, judged against the commit
    they reviewed.
  - roborev, read through the roborev daemon's local API on the same server as
    structured findings for a head SHA, never parsed from PR comment text.
  - No reviewer configured: CI alone decides.
- **Trusted comments.** Only comments written by our own App's bot user (the
  status comment) or by users with write access (commands) carry meaning. All
  other text is untrusted data for the worker.
- **Polling.** Use GitHub's conditional requests, which don't count against the
  rate limit when nothing changed. Poll each minute while waiting on CI, and
  every 15 minutes when idle.

### 5.3 Rounds

- One round per fixable state, one push per round. A round includes every item
  open on the settled head:
  - failed required checks, with log excerpts
  - findings at or above the threshold
  - change requests and unresolved threads from people
- Budgets per PR: at most 5 rounds, a model-spend cap, and wall-clock limits
  per round (default 45 minutes for the worker, 30 for the verifier). A spent
  budget means needs-human.
- Outcomes:
  - **pushed**
  - **nothing to do**: every item was refuted or out of scope, and the status
    comment says why
  - **failed verification**: the patch is kept for a person
  - **errored**: an infrastructure failure, retried with backoff, then
    needs-human

## 6. Worker

### 6.1 Inputs (the round spec)

- A git bundle of the head and base, on a scratch disk.
- The items. Each has an ID, a source (check, roborev, or person), and its text,
  marked as untrusted data.
- The repo's `AGENTS.md` or `CLAUDE.md`, the PR title and description as the
  statement of intent, and the linked issue if any.
- Test commands and limits from policy.
- Instructions adapted from shepherd-pr's "Triage and verify":
  - Prove or refute each item against the reviewed commit.
  - Fix valid items that are in scope.
  - Leave items that are out of scope or unclear for people.
  - Follow the codebase's existing precedent for each fix.

### 6.2 Outputs

- Commits, one per fixed item. Each commit holds the item's regression test and
  its fix, and its message names the item ID. They are authored as the bot.
- `evidence.json`, checked against a schema and capped in size. For each item:
  - a disposition: fixed, refuted, out-of-scope, unclear, or unproven
  - an explanation
  - the files, test IDs, and commit SHA

### 6.3 Execution

- Claude Code runs in print mode (`claude -p`) as a non-root user inside the VM,
  with permission prompts off. The VM is the boundary. Anthropic's docs say to
  skip permissions only inside a container, VM, or sandbox runtime, and Claude
  Code refuses to do so as root.
- `ANTHROPIC_BASE_URL` points at the host model gateway over vsock. The VM holds
  only a placeholder key.
- `HTTPS_PROXY` and `HTTP_PROXY` point at a local bridge that forwards over vsock
  to the host egress proxy. Tools that ignore proxy settings get no network.

## 7. Verifier

Checks on the host, by inspecting git objects only. No repo code runs.

- The bundle passes `git bundle verify` and a fetch with `transfer.fsckObjects`.
  It stays within the size cap and descends from the round's head.
- No changes to protected paths. The defaults are `.github/**`,
  `.devcontainer/**`, `.gitmodules`, `.gitattributes`, `CODEOWNERS`,
  `.roborev.toml`, `REVIEW.md`, and `.pr-babysitter.toml`, plus the repo's own
  list. The agent can't edit what builds its sandbox or judges its work.
- Deleted test files or test functions fail verification.
- Commit count and diff size stay within policy.

Checks in a fresh verifier VM, where repo code does run:

- For each commit marked fixed: check out its parent plus only that commit's
  test changes, and the named tests must fail. Then check out the commit, and
  they must pass.
- On the final head, every policy test command must pass.
- Items that can't be proven this way must be declared `unproven` with a
  reason. By default (`require_proof = true`), any unproven item fails the
  round, which sends the patch to a person.

The verifier VM reports JSON over vsock, and the host decides.

## 8. Gate and policy

### 8.1 Policy layers

- **Server policy** is host config that only admins can change. It sets:
  - allowed repos and the maximum level
  - the package registries the egress proxy may reach
  - budgets, notification targets, and retention
- **Repo policy** is `.pr-babysitter.toml`, always read from the default branch.
  It sets:
  - the level, up to the server maximum
  - test commands and the image reference
  - extra protected paths
  - review sources and the severity threshold
  - owner defaults

  A repo policy can narrow the server policy but never widen it. If it can't be
  parsed, that repo drops to observe-only.

```toml
level = "fix"                      # observe | fix | update | merge
test = ["make test", "make lint"]
image = "ghcr.io/acme/babysitter-images/api@sha256:<digest>"
protected = ["migrations/**"]
review = ["github", "roborev"]
min_severity = "medium"
max_rounds = 5
```

### 8.2 Levels

| Level | Allows |
|---|---|
| observe | Watch, write the status comment, notify |
| fix | Push fix commits to the PR's branch (same-repo branches only) |
| update | Also bring the branch up to date with base (GitHub's update-branch call, pinned to the expected head SHA), and resolve conflicts confined to the PR's own files |
| merge | Also merge when ready and a person has granted it |

### 8.3 Identity, grants, and merging

- Writes use a GitHub App bot with least privilege. Confirm the exact
  permissions in M1. The App never goes on a branch-protection bypass list.
- Pilot repos should turn on "dismiss stale approvals" and "require approval of
  the most recent push", so every bot push needs fresh human approval before
  merge. Under that rule, a bot pushing as a person would stop that person from
  approving, which is one more reason the bot needs its own identity.
- A merge needs all of these:
  - the repo is at the merge level
  - an active grant (`/babysit merge`, expiring, from a user with write access)
  - GitHub reports the PR mergeable, with required checks and approvals met
  - no open findings at or above the threshold
  - the head equals the last verified head

  The merge call pins that head SHA.
- Pushes are fast-forward only. There are no force-pushes in v1. After each push
  the gate confirms the remote head.
- The gate logs every allow and deny, with the rule that decided it.

## 9. People

- **Adopt.** A user with write access adds the `babysit` label. Before acting,
  the controller confirms who added it (from the issue's events) and that
  person's permission (from the collaborator-permission API). It ignores labels
  it can't verify and notes them in the status comment.
- **Owner.** Whoever added the label owns the PR, unless they name someone else
  with `/babysit owner @name`. This covers agent-written PRs that have no human
  author to chase, which was Block's problem in the paper.
- **Commands.** These are PR comments from users with write access, one per line,
  each starting with `/babysit`, outside quotes and code blocks:
  - `merge [until <time>]`
  - `hold` and `resume`
  - `release`
  - `owner @name`
  - `round`, to force a round
  - `status`
- **Status comment.** The bot writes one comment per PR and updates it in place.
  It shows:
  - the state, what the PR is waiting on, and who must act
  - each round so far, with item dispositions
  - what each pushed commit changed, and why

  People reading the PR learn what the bot did.
- **Notifications.** When a PR enters needs-human, or is ready but waiting on a
  grant, the bot tells the owner with a PR mention and a Slack message. It
  reminds them after a set time, then escalates to a backup, and holds off
  during quiet hours. Messages carry links and states, never code or logs.
- **Daily sweep.** For each owner, the sweep lists every adopted PR waiting on a
  person. It also lists unadopted PRs, drafts included, that have sat idle past
  a threshold. It never closes anything.

## 10. Sandbox

### 10.1 Host requirements

- Linux on bare metal, or a cloud instance with nested virtualization, with KVM.
- A host kernel on Firecracker's tested list: today 5.10, 6.1, and 6.18. Prefer
  6.18.
- Firecracker and its jailer from an official release (v1.17.0 at writing),
  with versions pinned together. The guest kernel comes from Firecracker's CI
  builds, also pinned.
- Follow Firecracker's production host guide:
  - current CPU microcode
  - no swap to disk, or encrypted swap
  - one round per Firecracker process
  - an explicit decision on SMT: the guide advises turning it off against
    side-channel attacks, which halves the logical CPUs
- Firecracker filters no traffic. With no network card in the guest, the host
  proxy is the only route out.

### 10.2 Images

- One read-only root image per repo, built from the repo's `.devcontainer` or
  CI Dockerfile on the default branch, never from a PR branch. It holds:
  - the toolchain
  - a pinned Claude Code
  - git
  - the guest init script
- Build images in CI (GitHub Actions) and publish them as OCI images pinned by
  digest. The server converts each one to ext4 in user space (for example with
  `mkfs.ext4 -d`), without mounting it and without a Docker daemon.
- Each round gets a fresh scratch disk holding the bundle and the round spec.

### 10.3 Per-VM limits

- vCPUs and memory come from policy (default 4 vCPUs, 8 GiB). Disk sizes are
  fixed. Firecracker's rate limiters cap block I/O. The launcher enforces the
  wall-clock limit by killing the process.
- The jailer gives each VM:
  - a unique uid and gid from a reserved range
  - a new PID namespace
  - cgroup v2 limits
  - a chroot

  Firecracker's default seccomp filters stay on.

### 10.4 Network and credentials

- No network card. Two vsock ports: one to the egress proxy, one to the model
  gateway.
- The **egress proxy** is an HTTP CONNECT proxy with a hostname allowlist: the
  server-approved package registries a repo's policy selects, and no GitHub.
  There's no TLS inspection in v1, so keep the allowlist small. A proxy that
  only reads hostnames can be tricked by domain fronting.
- The **model gateway** is a stdlib reverse proxy to the model API. It:
  - adds the org's API key, which the VM never holds
  - enforces each round's token and spend budget
  - records usage

  The worker uses an API key, not subscription sign-in, because sign-in would
  put a token inside the VM.
- Nothing from a VM is ever mounted on the host. Results come back over vsock,
  with size caps.

### 10.5 Launcher

The launcher is the only privileged part: a small systemd service that takes a
round ID and nothing else. It validates the prepared inputs in a fixed
directory, then runs the jailer with a fixed config template
(`--config-file`).

## 11. Security model

| Threat | Mitigation |
|---|---|
| Prompt injection in PR text, comments, or CI logs | Items arrive marked as untrusted data. The worker has no token, no GitHub access, and a small allowlist. The verifier and gate check its output, and protected paths stay off limits |
| Malicious test or dependency code | It runs only in a microVM with no credentials, no host mounts, resource limits, and timeouts |
| Forged review results | roborev is read through its local API, and GitHub reviews with their author and commit. PR comment text never decides state |
| The agent edits what judges it | Protected paths cover CI, review config, babysitter policy, and the devcontainer |
| A PR widens its own permissions | Repo policy is read from the default branch, and server policy caps it |
| Unauthorized labels or commands | Every actor's permission is checked through the API |
| Bot code reaches main without review | Fresh approval is required after bot pushes, and the bot is never on a bypass list. A merge needs a grant plus GitHub's approvals |
| Key or code exfiltration | No secrets in the VM; the gateway holds the model key; egress is allowlisted. Domain fronting remains a residual risk |
| Host attack through a crafted disk image | Guest disks are never mounted. Results come over vsock with caps, and fetches run `transfer.fsckObjects` |
| VM escape | KVM, the jailer, and seccomp; a patched host kernel on Firecracker's tested list; current microcode; an explicit SMT decision |
| Runaway cost or resource use | Per-round limits, gateway budgets, round caps, a global concurrency cap, and a kill switch |
| Insider misuse | Permissions mirror GitHub roles, and the ledger is a full audit trail |

## 12. Data handling

- **What leaves the server:**
  - repo code, CI log excerpts, PR text, and review findings, sent to the model
    provider through the gateway
  - GitHub API traffic
  - notification text, which carries links and states only
- **What stays on the server:** the ledger, including prompts, diffs, logs, and
  evidence. These are deleted after a retention period (default 90 days); counts
  and outcomes are kept for metrics. Only admins can read the server.
- The gateway doesn't log prompt or response bodies.
- **Open:** confirm that BlueSprig's agreement with the model provider covers
  code and CI logs, and whether any repos must be excluded, for example where
  fixtures or logs might hold PHI.

## 13. Records and metrics

The ledger is SQLite with a single writer, backed up off the server nightly. It
holds:

- PRs and observations (normalized snapshot hashes and diffs)
- state changes
- rounds, items, and dispositions
- verifier results
- gate decisions, with the rule that decided each
- GitHub writes, with response IDs
- notifications and grants

Metrics are exported as JSON:

- time in each state, and time to merge
- rounds and human touches per PR
- items fixed, refuted, out of scope, and unproven
- refutations people later overturned
- verification failures, by cause
- model spend per round and per merged PR
- babysat merges reverted within 14 days

Together these answer the paper's question: "If something were wrong, how would
we know?"

## 14. Deployment and operations

- **Services:**
  - `pr-babysitter tick`: a systemd timer, every minute, run as an unprivileged
    user
  - `pr-babysitter-launcher`: privileged, narrow
  - `pr-babysitter proxy`: the egress proxy and model gateway, unprivileged
  - the roborev daemon: optional, as the reviewer
- `pr-babysitter doctor` checks:
  - KVM and the host kernel
  - Firecracker and jailer versions
  - the images
  - App authentication
  - the gateway
  - disk space and backups
- `pr-babysitter pause-all` is the kill switch. It stops new rounds and kills
  running VMs.
- State can be rebuilt from GitHub plus the ledger backup. Losing the server
  costs only the rounds in progress.

## 15. Stack

- Go, with one binary per service. The standard library's `net/http` covers
  GitHub REST and GraphQL, the CONNECT proxy, and the gateway.
- SQLite for the ledger. Choose the driver in M1, preferring pure Go for static
  builds.
- Firecracker, run through the jailer with `--config-file`. Use the official Go
  SDK only if running the binaries directly proves awkward.
- The `git` command on the host, for bundles and pushes.
- roborev's Go client for the roborev adapter, pinned to the deployed roborev
  version.

## 16. Testing

- TDD for every feature and every bug fix.
- **Unit:**
  - state derivation, as a pure function of a snapshot
  - check classification
  - policy layering
  - command and grant parsing
  - evidence validation
- **Integration:**
  - the GitHub client against recorded API responses at the HTTP boundary,
    starting from shepherd-pr's scenarios
  - the launcher booting a real microVM from a test image
  - the proxy and gateway over real sockets
- **End to end, no mocks:** a sandbox GitHub org with the real App, real Actions
  CI, roborev, microVMs, and real model calls. Scripted scenarios:
  - a failing test
  - a valid review finding, and a refuted one
  - a malicious comment telling the agent to push and merge
  - an attempt to edit a protected path
  - the base branch moving mid-round
  - a missing approval
  - a cancelled check
  - a spent budget
- `make check` runs formatting, vet, and the unit and integration tests.
  `make e2e` runs the live suite.

## 17. Milestones

Sizes are rough lines of Go, with about as much test code again.

1. **Observe** (~1,200): App authentication, the poller, state derivation, the
   ledger, labels and commands with permission checks, the status comment,
   Slack notifications, the daily sweep, and `doctor`. Pilot at the observe
   level.
2. **Sandbox** (~1,000): the image pipeline, the launcher and jailer, the vsock
   proxy and gateway with budgets, and result collection. Finish by running a
   canned round end to end.
3. **Fix** (~1,200): the round spec and worker prompt, the evidence schema, the
   verifier with proofs and path rules, gate pushes, round budgets, and the
   roborev adapter. Pilot at the fix level.
4. **Team operations** (~500): escalation and quiet hours, metrics export,
   retention, the kill switch, and backups.

Later, once the ledger shows fix rounds can be trusted: the update and merge
levels, GitHub Enterprise, a package-registry cache, and other agents.

## 18. Risks

1. **Fix quality is the big unknown.** This design limits the harm a bad round
   can do; it doesn't make rounds good. Measure in the M3 pilot before raising
   any level.
2. **Images are ongoing work** for every toolchain.
3. **Firecracker operations:** a privileged launcher, supported kernel lines,
   and KVM availability in the cloud.
4. **Model spend**, especially with roborev panels running on every push.
5. **Compliance** could rule out some repos.
6. **GitHub rate limits** as the PR count grows.
7. **Overlap with existing tools.** kenn-io/forge (unread) may already cover the
   outer loop. Check it before M1.
8. **One server is a single point of failure.** That's acceptable for v1, since
   state can be rebuilt.

## 19. Open questions

1. **Data:** may code, CI logs, and PR text go to the model provider under the
   current agreement? Are any repos excluded?
2. **Server:** where does it run, and who operates it? Bare metal or nested
   virtualization? Can it run a 6.18 host kernel?
3. **GitHub:** is the org on github.com? Who creates and approves the App? Which
   repos pilot first?
4. **Branch protection:** will the pilot repos require fresh approval after bot
   pushes?
5. **Signed commits:** do the pilot repos require them? If so, the gate must
   create commits through GitHub's API instead of `git push`. Confirm GitHub's
   signing behavior in M3.
6. **Reviewer:** roborev on the same server from M3, or GitHub reviews only?
7. **Slack:** which workspace and channel, which quiet hours, which time zone?
8. **Spending caps:** per round, per PR, and per month.
9. **Toolchains:** what the pilot repos need, for the first images.
10. **Retention:** how long to keep prompts, diffs, and logs.

## 20. References

- Davis et al., "Agentic AI and Code Reviews," Enterprise Technology Leadership
  Journal, Fall 2026: [PDF][paper]
- roborev: [site][roborev], [repository](https://github.com/kenn-io/roborev)
- shepherd-pr (MIT): [repository][shepherd]
- Firecracker: [repository](https://github.com/firecracker-microvm/firecracker),
  including the production host setup, jailer, and vsock docs
- Anthropic: [sandbox environments][anthropic-envs],
  [secure deployment][anthropic-secure]

[paper]: https://readwise-assets.s3.amazonaws.com/media/wisereads/articles/agentic-ai-and-code-reviews/1439.pdf
[roborev]: https://www.roborev.io/
[shepherd]: https://github.com/prime-radiant-inc/shepherd-pr
[anthropic-envs]: https://code.claude.com/docs/en/sandbox-environments
[anthropic-secure]: https://code.claude.com/docs/en/agent-sdk/secure-deployment
