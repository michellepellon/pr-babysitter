// ABOUTME: GitHub calls through the gh CLI, and the plan subcommand that decides each labeled PR and starts one round.
// ABOUTME: Every GitHub value is untrusted: it reaches gh as a GraphQL variable or an argument, never through a shell.

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// prQuery reads up to 20 open babysit PRs, passing values only as variables.
const prQuery = `fragment A on Actor { __typename login ... on User { databaseId } ... on Bot { databaseId } }
query($owner: String!, $name: String!) { repository(owner: $owner, name: $name) {
  pullRequests(states: OPEN, labels: ["babysit"], first: 20) { nodes {
    number isCrossRepository headRefOid headRefName baseRefName mergeable reviewDecision
    timelineItems(itemTypes: LABELED_EVENT, last: 100) { nodes { ... on LabeledEvent { id createdAt label { name } author: actor { ...A } } } }
    commits(last: 1) { nodes { commit { statusCheckRollup { contexts(first: 100) { nodes {
      ... on CheckRun { name status conclusion checkSuite { workflowRun { databaseId } } }
      ... on StatusContext { context state } } } } } } }
    reviewThreads(first: 100) { nodes { isResolved comments(first: 100) { nodes { author { ...A } body createdAt } } } }
    reviews(last: 100) { nodes { author { ...A } body state createdAt: submittedAt } } } } } }`

const label = "babysit"

type prResponse struct {
	Data struct {
		Repository struct{ PullRequests nodes[ghPR] }
	}
}

type nodes[T any] struct{ Nodes []T }

// ghPR is one PR from prQuery. encoding/json matches field names without regard to case.
type ghPR struct {
	Number                                                          int
	IsCrossRepository                                               bool
	HeadRefOid, HeadRefName, BaseRefName, Mergeable, ReviewDecision string
	TimelineItems                                                   nodes[ghComment] // babysit labeled events
	Commits                                                         nodes[struct {
		Commit struct {
			StatusCheckRollup struct{ Contexts nodes[ghContext] }
		}
	}]
	ReviewThreads nodes[struct {
		IsResolved bool
		Comments   nodes[ghComment]
	}]
	Reviews nodes[ghComment]
}

// ghContext is a CheckRun (Name, Status, Conclusion) or a StatusContext (Context, State).
type ghContext struct {
	Name, Status, Conclusion, Context, State string
	CheckSuite                               struct {
		WorkflowRun struct {
			ID int64 `json:"databaseId"`
		}
	}
}

type ghActor struct {
	Type  string `json:"__typename"`
	Login string
	ID    int64 `json:"databaseId"`
}

// ghComment is a comment, a review, or a labeled event, whose actor prQuery calls author.
type ghComment struct {
	ID        string // labeled events only
	Label     struct{ Name string }
	Author    ghActor
	Body      string
	State     string // reviews only
	CreatedAt time.Time
}

type ghPermission struct {
	Role string `json:"role_name"`
}

type ghRule struct {
	Parameters struct {
		Checks       []json.RawMessage `json:"required_status_checks"`
		DismissStale bool              `json:"dismiss_stale_reviews_on_push"`
		LastPush     bool              `json:"require_last_push_approval"`
	}
}

type ghIssueComment struct {
	ID        int64
	Body      string
	CreatedAt time.Time `json:"created_at"`
	User      struct {
		ID          int64
		Login, Type string
	}
}

// gh runs the gh CLI with an argument list. Tests replace it.
var gh = func(args ...string) (string, error) {
	var stderr bytes.Buffer
	cmd := exec.Command("gh", args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil { // only args[:2], so a comment body never reaches the log
		return "", fmt.Errorf("gh %s: %v: %s", strings.Join(args[:2], " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

// ghJSON runs gh and decodes each JSON value it prints: one per --jq result, or one response.
func ghJSON[T any](args ...string) ([]T, error) {
	out, err := gh(args...)
	var all []T
	for dec := json.NewDecoder(strings.NewReader(out)); err == nil; {
		var v T
		if err = dec.Decode(&v); err == nil {
			all = append(all, v)
		}
	}
	if err != io.EOF {
		return nil, err
	}
	return all, nil
}

func listPRs(repo string) ([]ghPR, error) {
	owner, name, _ := strings.Cut(repo, "/")
	r, err := ghJSON[prResponse]("api", "graphql", "-f", "query="+prQuery, "-f", "owner="+owner, "-f", "name="+name)
	if err != nil || len(r) != 1 {
		return nil, fmt.Errorf("reading PRs: %v", err)
	}
	return r[0].Data.Repository.PullRequests.Nodes, nil
}

var (
	loginRE = regexp.MustCompile(`^[A-Za-z0-9-]{1,39}$`)
	writers = map[string]bool{} // login → writer, looked up once per run
)

// toActor identifies a GitHub user. A bot is never a writer; a lookup that fails means not a writer.
func toActor(repo string, a ghActor) Actor {
	bot := a.Type == "Bot"
	w, ok := writers[a.Login]
	if !ok && !bot && loginRE.MatchString(a.Login) {
		p, err := ghJSON[ghPermission]("api", "repos/"+repo+"/collaborators/"+a.Login+"/permission")
		w = err == nil && len(p) == 1 && slices.Contains([]string{"admin", "maintain", "write"}, p[0].Role)
		writers[a.Login] = w
	}
	return Actor{ID: a.ID, Login: a.Login, Writer: w && !bot, Bot: bot}
}

// toSnapshot maps one PR from prQuery to what decide reads; loadPR adds the REST parts.
func toSnapshot(repo string, p ghPR, bots []int64) Snapshot {
	s := Snapshot{Number: p.Number, Branch: p.HeadRefName, Base: p.BaseRefName, Fork: p.IsCrossRepository,
		Head: p.HeadRefOid, Mergeable: p.Mergeable, ReviewDecision: p.ReviewDecision, ReviewerBots: bots}
	for _, e := range p.TimelineItems.Nodes { // oldest first, so the newest babysit event wins
		if e.Label.Name == label {
			s.LabelEvent, s.LabelAt, s.Labeler = e.ID, e.CreatedAt, toActor(repo, e.Author)
		}
	}
	for _, c := range p.Commits.Nodes {
		for _, x := range c.Commit.StatusCheckRollup.Contexts.Nodes { // one of each pair is empty
			s.Checks = append(s.Checks, Check{Name: x.Name + x.Context, Status: x.Status + x.State,
				Conclusion: x.Conclusion, Run: x.CheckSuite.WorkflowRun.ID})
		}
	}
	for _, t := range p.ReviewThreads.Nodes { // a thread speaks as its newest writer or listed bot comment
		f := Feedback{Kind: "thread", Resolved: t.IsResolved}
		for _, c := range t.Comments.Nodes {
			if a := toActor(repo, c.Author); isWriter(a) || slices.Contains(bots, a.ID) {
				f.Author, f.Body, f.CreatedAt = a, c.Body, c.CreatedAt
			}
		}
		s.Feedback = append(s.Feedback, f)
	}
	for _, r := range p.Reviews.Nodes {
		s.Feedback = append(s.Feedback, Feedback{Kind: "review", Author: toActor(repo, r.Author), Body: r.Body, State: r.State, CreatedAt: r.CreatedAt})
	}
	return s
}

func ruleFlags(rules []ghRule) (checks, dismiss, lastPush bool) {
	for _, r := range rules { // rules from every ruleset that covers the branch
		checks, dismiss, lastPush = checks || len(r.Parameters.Checks) > 0, dismiss || r.Parameters.DismissStale, lastPush || r.Parameters.LastPush
	}
	return
}

// loadPR adds the base branch's rules, the head's check suites, and the PR's comments.
func loadPR(repo string, p ghPR, bots []int64, now time.Time) (Snapshot, error) {
	s := toSnapshot(repo, p, bots)
	s.Now = now
	if !shaRE.MatchString(s.Head) {
		return s, fmt.Errorf("PR %d: bad head", s.Number)
	}
	base := strings.ReplaceAll(url.PathEscape(s.Base), "%2F", "/")
	rules, err := ghJSON[ghRule]("api", "repos/"+repo+"/rules/branches/"+base, "--paginate", "--jq", ".[]")
	s.RequiredChecks, s.DismissStale, s.LastPushApproval = ruleFlags(rules)
	if err == nil {
		s.Suites, err = ghJSON[Suite]("api", "repos/"+repo+"/commits/"+s.Head+"/check-suites", "--paginate", "--jq", ".check_suites[]")
	}
	var comments []ghIssueComment
	if err == nil {
		comments, err = issueComments(repo, strconv.Itoa(s.Number))
	}
	i, st := statusComment(comments)
	if i >= 0 {
		s.State, s.CommentID, s.Comment = st, comments[i].ID, comments[i].Body
	}
	for j, c := range comments {
		if j == i {
			continue
		}
		a := toActor(repo, ghActor{Type: c.User.Type, Login: c.User.Login, ID: c.User.ID})
		s.Feedback = append(s.Feedback, Feedback{Kind: "comment", Author: a, Body: c.Body, CreatedAt: c.CreatedAt})
	}
	return s, err
}

func issueComments(repo, pr string) ([]ghIssueComment, error) {
	return ghJSON[ghIssueComment]("api", "repos/"+repo+"/issues/"+pr+"/comments", "--paginate", "--jq", ".[]")
}

// statusComment finds the status comment: the first by github-actions[bot] whose line 1 is a
// state line. It returns -1 if there is none.
func statusComment(comments []ghIssueComment) (int, State) {
	for i, c := range comments {
		if st, ok := parseState(c.User.ID, c.Body); ok {
			return i, st
		}
	}
	return -1, State{}
}

// commentBody renders the status comment, keeping old's round summary. The fenced block holds
// the reason on its first line, then a blank line, then the summary, so neither renders.
func commentBody(old string, st State, status, reason string) string {
	_, block, _ := strings.Cut(strings.ReplaceAll(old, "\r\n", "\n"), "\n\n")
	f, rest, _ := strings.Cut(block, "\n")
	rest, _ = strings.CutSuffix(strings.TrimSuffix(rest, "\n"+summaryCutMarker+"\n"), "\n"+f+"\n")
	_, summary, _ := strings.Cut(rest, "\n\n")
	return renderComment(st, status, strings.Join(strings.Fields(reason), " ")+"\n\n"+summary)
}

// plan decides every labeled PR, updates their status comments, and starts a round on the
// qualifying PR labeled longest ago: its state says running before items or outputs exist.
func plan(repo, items, output string, bots []int64, now time.Time) error {
	prs, err := listPRs(repo)
	if err != nil {
		return err
	}
	snaps, decisions, pick := make([]Snapshot, len(prs)), make([]Decision, len(prs)), -1
	for i, p := range prs {
		if snaps[i], err = loadPR(repo, p, bots, now); err != nil {
			return err
		}
		decisions[i] = decide(snaps[i])
		if decisions[i].Kind == Round && (pick < 0 || snaps[i].LabelAt.Before(snaps[pick].LabelAt)) {
			pick = i
		}
	}
	if pick >= 0 {
		if _, err := git(".", "check-ref-format", "refs/heads/"+snaps[pick].Branch); err != nil {
			return fmt.Errorf("PR %d: bad branch name", snaps[pick].Number)
		}
		st := &decisions[pick].State
		st.Rounds, st.RoundHead, st.Outcome, st.OutcomeAt = st.Rounds+1, snaps[pick].Head, "running", now
	}
	for n := range snaps {
		i := (pick + 1 + n) % len(snaps) // the picked PR goes last, so a failed write elsewhere leaves it idle
		s, d := snaps[i], decisions[i]
		fmt.Printf("PR %d: %s\n", s.Number, d.Kind) // no reason: it can hold check names
		status := "**" + string(d.Kind) + "**"
		switch {
		case d.Kind == Skip && !s.Fork:
			continue // the spec wants no comment when a non-writer adds the label
		case i == pick:
			status = fmt.Sprintf("**round %d running**", d.State.Rounds)
		case d.Kind == Round:
			status = "**queued** behind another PR's round"
		case (d.Kind == NeedsHuman || d.Kind == Ready) && loginRE.MatchString(d.State.Owner):
			status += " @" + d.State.Owner
		}
		body := commentBody(s.Comment, d.State, status, d.Reason)
		if body == strings.ReplaceAll(s.Comment, "\r\n", "\n") {
			continue
		}
		if s.CommentID == 0 {
			_, err = gh("api", fmt.Sprintf("repos/%s/issues/%d/comments", repo, s.Number), "-f", "body="+body)
		} else {
			_, err = gh("api", fmt.Sprintf("repos/%s/issues/comments/%d", repo, s.CommentID), "-X", "PATCH", "-f", "body="+body)
		}
		if err != nil {
			return err
		}
	}
	if pick < 0 {
		return nil
	}
	return writeRound(repo, items, output, snaps[pick], decisions[pick])
}

// writeRound writes the round's items and failed-job logs, then the job outputs.
func writeRound(repo, items, output string, s Snapshot, d Decision) error {
	os.MkdirAll(filepath.Join(items, "logs"), 0o755) // a failure shows up as WriteFile's error
	data, _ := json.MarshalIndent(struct {
		PR           int
		Head, Branch string
		Decision
	}{s.Number, s.Head, s.Branch, d}, "", "  ")
	if err := os.WriteFile(filepath.Join(items, "items.json"), data, 0o644); err != nil {
		return err
	}
	for _, c := range d.Failed {
		name := filepath.Join(items, "logs", fmt.Sprintf("%d.log", c.Run))
		if _, err := os.Stat(name); c.Run == 0 || err == nil {
			continue // a status context has no log, and a run's log holds all its failed jobs
		}
		log, err := gh("run", "view", strconv.FormatInt(c.Run, 10), "-R", repo, "--log-failed")
		if err == nil {
			err = os.WriteFile(name, []byte(log[max(0, len(log)-200<<10):]), 0o644)
		}
		if err != nil {
			return err
		}
	}
	f, err := os.OpenFile(output, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err == nil {
		_, err = fmt.Fprintf(f, "pr=%d\nhead=%s\nbranch=%s\n", s.Number, s.Head, s.Branch)
		err = errors.Join(err, f.Close())
	}
	return err
}

// stillAdopted is apply's re-read: the PR is open, labeled by a writer, not a fork, and on head.
func stillAdopted(repo string, prs []ghPR, pr, head string) error {
	for _, p := range prs {
		if strconv.Itoa(p.Number) != pr {
			continue
		}
		if s := toSnapshot(repo, p, nil); s.Fork || s.Head != head || !isWriter(s.Labeler) {
			return errors.New("the PR moved, became a fork, or lost its writer's label")
		}
		return nil
	}
	// ponytail: only the first 20 labeled PRs are read, so a PR pushed out of them reads as stale.
	return errors.New("the PR is closed or no longer labeled")
}

var rereadPR = func(pr, head string) error {
	repo := os.Getenv("GITHUB_REPOSITORY")
	prs, err := listPRs(repo)
	if err != nil {
		return err
	}
	return stillAdopted(repo, prs, pr, head)
}

// cmdPlan's inputs: GITHUB_REPOSITORY, GITHUB_OUTPUT, the BABYSIT_ITEMS artifact folder, and
// BABYSIT_REVIEWER_BOTS, whitespace-separated user IDs. gh reads its token from GH_TOKEN.
func cmdPlan(args []string) int {
	env, ok := envInputs("plan", args, []string{"GITHUB_REPOSITORY", "GITHUB_OUTPUT", "BABYSIT_ITEMS"}, "BABYSIT_REVIEWER_BOTS")
	if !ok {
		return 2
	}
	var bots []int64
	for _, f := range strings.Fields(env["BABYSIT_REVIEWER_BOTS"]) {
		id, err := strconv.ParseInt(f, 10, 64)
		if err != nil {
			fmt.Fprintln(os.Stderr, "BABYSIT_REVIEWER_BOTS must hold user IDs")
			return 2
		}
		bots = append(bots, id)
	}
	if err := plan(env["GITHUB_REPOSITORY"], env["BABYSIT_ITEMS"], env["GITHUB_OUTPUT"], bots, time.Now()); err != nil {
		fmt.Println(err)
		return 1
	}
	return 0
}
