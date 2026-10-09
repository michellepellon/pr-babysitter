# pr-babysitter

pr-babysitter fixes what blocks a pull request labeled `babysit`: failing checks
and review findings from people with write access. It tells the PR's owner when
only a person can act. It never reviews, approves, or merges.

It runs on GitHub Actions in your repo, with no server, GitHub App, or stored
secrets. Each run picks at most one PR, runs Claude Code on it in a sandbox,
proves each fix with your test or lint command, and pushes as
github-actions[bot]. CI on those commits waits for a writer's approval.

## Setup

In the repo:

1. Add a ruleset on the default branch that requires status checks, dismisses
   stale approvals, and requires approval of the most recent push. Bind each
   required check to the GitHub Actions app (integration ID 15368).
   pr-babysitter won't run a round until all three are in place.
2. Create an environment `babysit`, limited to the default branch, no secrets.
3. Under Settings → Actions, let workflows request write access. If
   pr-babysitter is private, allow your repo under its Settings → Actions → Access.
4. Copy `examples/caller.yml` to `.github/workflows/babysit.yml`, pin it to a
   full commit SHA of pr-babysitter's main branch, set `workflows:` to your CI
   workflow names, and fill in the inputs below.

In the Claude Console (Settings → Workload identity → Connect workload):

1. Create a service account in a workspace that has a spend limit.
2. Create a federation rule for the repo:
   - `subject_prefix` exactly
     `repo:<owner>@<owner_id>/<repo>@<repo_id>:environment:babysit`, with no
     trailing `*`;
   - claims `repository_owner: <owner>` and `ref: refs/heads/<default branch>`;
   - a 600-second token lifetime.
3. The Console offers only scope `workspace:developer`. Narrow the rule to
   `workspace:inference` with an `org:admin` login:

   ```bash
   ant auth login --profile admin --scope "org:admin"
   export ANTHROPIC_AUTH_TOKEN=$(ant auth print-credentials --profile admin --access-token)
   curl --fail-with-body -sS "https://api.anthropic.com/v1/organizations/federation_rules/<rule id>" \
     -H "authorization: Bearer $ANTHROPIC_AUTH_TOKEN" -H "anthropic-version: 2023-06-01" \
     -H "content-type: application/json" -d '{"oauth_scope": "workspace:inference"}'
   ```

## Inputs

| Input | Default | Meaning |
|---|---|---|
| `dry_run` | `true` | Run rounds and record what would be pushed, but push nothing |
| `setup_command` | none | Runs as the agent before the network closes, such as `npm ci` |
| `test_command` | none | Proves a fix: run with a test selector appended, such as `go test ./... -run` |
| `lint_command` | none | Proves a lint fix; without it, lint fixes can't be proved |
| `reviewer_bots` | none | User IDs of review bots whose comments count when they name the head commit |
| `protected_paths` | none | Added to the built-in list; a round that changes one is thrown away |
| WIF IDs | required | `federation_rule_id`, `organization_id`, `service_account_id`, `workspace_id` |

A `protected_paths` entry ending in `/` protects that directory. Any other entry
protects every file with that name. The built-in list is `.github/`,
`.devcontainer/`, `.claude/`, `.gitattributes`, `.gitmodules`, `CODEOWNERS`,
`CLAUDE.md`, `AGENTS.md`, `.roborev.toml`, and `REVIEW.md`.

Limits: 5 rounds per label, 20 PRs per run, 45 minutes per round. The hourly
scan alone bills about 720 runner minutes per repo per month.

## Using it

- **Start:** a writer adds the `babysit` label and becomes the PR's owner.
- **Pause:** remove the label (re-adding resets the round count), or
  `gh workflow disable babysit.yml` for the whole repo.
- **Run now:** `gh workflow run babysit.yml`.

## Reading the status comment

github-actions[bot] keeps one comment on each labeled PR. Its first line is a
hidden state line; don't edit it. Below that is the state, which is one of:

- **waiting:** on a check or on mergeability; needs-human after 60 minutes.
- **needs-human:** a person must act, and the comment @mentions the owner with
  the reason: resolve conflicts, approve the bot's CI runs after reading its
  commits, review the PR, fix the repo setup, or re-add the label after 5
  rounds.
- **ready:** approved and green; merge it or enable auto-merge.

The fenced block at the end is the last round's outcome, then the agent's
summary. The outcome is one of `pushed N commits`, `dry-run: would push N
commits`, `rejected: <reason>`, `stale: <reason>`, or `failed: <reason>`. A
round that ends without a push waits for a writer to comment, review, or push
before it tries again.

To read a dry run's commits, download its round artifact within 3 days:

```bash
gh run download <run id> -n round -D round
git fetch round/round.bundle HEAD && git log -p <PR head>..FETCH_HEAD
```
