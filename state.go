// ABOUTME: State rules: what plan reads about a labeled PR, check classification, and spec §3's eleven rules.
// ABOUTME: decide is pure; the snapshot carries the current time, and the decision carries the state line to record.

package main

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	maxRounds  = 5
	stuckAfter = 60 * time.Minute
)

// Snapshot holds only what the state rules read about one labeled PR.
type Snapshot struct {
	Fork           bool
	Head           string  // head commit SHA
	Mergeable      string  // GraphQL MergeableState: MERGEABLE, CONFLICTING, or UNKNOWN
	ReviewDecision string  // GraphQL PullRequestReviewDecision, or empty
	LabelEvent     string  // node ID of the newest babysit labeled event
	Labeler        Actor   // who added that label
	Checks         []Check // every check run and status context on the head, repeat runs included
	Suites         []Suite // the head's check suites; CI awaiting approval shows only here
	Feedback       []Feedback
	// The base branch's rules (spec §3 rule 2).
	RequiredChecks, DismissStale, LastPushApproval bool
	ReviewerBots                                   []int64 // user IDs from the reviewer_bots input
	State                                          State
	Now                                            time.Time
}

// Actor is a GitHub user, identified by ID; Login is only for @mentions.
type Actor struct {
	ID          int64
	Login       string
	Writer, Bot bool // Writer: admin, maintain, or write role
}

// Check is one check run, workflow run, or status context. A status
// context's state goes in Status.
type Check struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

// Suite is one check suite from the REST API.
type Suite struct {
	Conclusion string `json:"conclusion"`
	Runs       int    `json:"latest_check_runs_count"`
}

// Feedback is a review thread, review, or PR comment.
type Feedback struct {
	Kind      string // "thread", "review", or "comment"
	Author    Actor
	Body      string
	State     string // a review's state, such as CHANGES_REQUESTED
	CreatedAt time.Time
	Resolved  bool // threads only
}

type Class string

const (
	CheckPass, CheckFail, CheckHuman, CheckPending Class = "pass", "fail", "needs-human", "pending"
)

// classes maps lowercased conclusions and status-context states to a class.
// Anything missing, including every in-flight status, is pending.
var classes = map[string]Class{
	"success": CheckPass, "neutral": CheckPass, "skipped": CheckPass,
	"failure": CheckFail, "timed_out": CheckFail, "startup_failure": CheckFail, "error": CheckFail,
	"cancelled": CheckHuman, "action_required": CheckHuman, "stale": CheckHuman,
}

// checkState is a completed check's conclusion, or else its status, lowercased.
func checkState(c Check) string {
	if strings.EqualFold(c.Status, "completed") {
		return strings.ToLower(c.Conclusion)
	}
	return strings.ToLower(c.Status)
}

// classify judges a check on its state. A value we don't recognize counts as
// pending, which fails safe.
func classify(c Check) Class {
	if class, ok := classes[checkState(c)]; ok {
		return class
	}
	return CheckPending
}

type Kind string

const (
	Skip, NeedsHuman, Waiting, Round, Ready Kind = "skip", "needs-human", "waiting", "round", "ready"
)

// Decision is decide's verdict. For a round, Failed and Feedback are its
// items. State is the state line plan should record, whatever the kind.
type Decision struct {
	Kind     Kind
	Reason   string
	Failed   []Check
	Feedback []Feedback
	State    State
}

// decide applies spec §3's rules in order and returns at the first match.
func decide(s Snapshot) Decision {
	st := s.State
	if s.Fork {
		return Decision{Kind: Skip, Reason: "fork PRs are not supported", State: st}
	}
	if s.LabelEvent == "" || !s.Labeler.Writer || s.Labeler.Bot {
		return Decision{Kind: Skip, Reason: "the babysit label wasn't added by a person with write access", State: st}
	}
	if st.LabelEvent != s.LabelEvent { // a new label: new owner, round count starts over
		st = State{V: 1, LabelEvent: s.LabelEvent, Owner: s.Labeler.Login, LastPushSHA: st.LastPushSHA,
			LastPushAt: st.LastPushAt, HeadSeenSHA: st.HeadSeenSHA, HeadSeenAt: st.HeadSeenAt}
	}
	if st.Outcome == "running" { // plan runs one at a time, so that round died
		st.Outcome = "failed: the round didn't finish"
	}
	if st.HeadSeenSHA != s.Head {
		st.HeadSeenSHA, st.HeadSeenAt = s.Head, s.Now
	}
	verdict := func(k Kind, format string, a ...any) Decision {
		return Decision{Kind: k, Reason: fmt.Sprintf(format, a...), State: st}
	}

	switch {
	case !s.RequiredChecks || !s.DismissStale || !s.LastPushApproval:
		return verdict(NeedsHuman, "repo setup incomplete: the base branch must require status checks, dismiss stale approvals, and require approval of the most recent push")
	case st.Rounds >= maxRounds:
		return verdict(NeedsHuman, "%d rounds have run since the label was added; re-add the label to allow more", st.Rounds)
	case s.Mergeable == "CONFLICTING":
		return verdict(NeedsHuman, "resolve conflicts")
	case s.Head == st.LastPushSHA && slices.ContainsFunc(s.Suites, func(x Suite) bool {
		return strings.EqualFold(x.Conclusion, "action_required") && x.Runs == 0
	}):
		return verdict(NeedsHuman, "review the bot's commits, then approve the workflow runs")
	}
	if on := waitingOn(s); on != "" {
		if s.Now.Sub(st.HeadSeenAt) >= stuckAfter {
			return verdict(NeedsHuman, "stuck for 60 minutes waiting on %s", on)
		}
		return verdict(Waiting, "waiting on %s", on)
	}
	for _, c := range s.Checks {
		if classify(c) == CheckHuman {
			return verdict(NeedsHuman, "check %s ended %s", c.Name, checkState(c))
		}
	}
	if st.Rounds > 0 && st.RoundHead == s.Head && !slices.ContainsFunc(s.Feedback, func(f Feedback) bool {
		return isWriter(f.Author) && f.CreatedAt.After(st.OutcomeAt)
	}) {
		return verdict(NeedsHuman, "the last round ended without a push: %s", st.Outcome)
	}

	d := verdict(Round, "")
	for _, c := range s.Checks {
		if classify(c) == CheckFail {
			d.Failed = append(d.Failed, c)
		}
	}
	for _, f := range s.Feedback {
		if isItem(s, st, f) {
			d.Feedback = append(d.Feedback, f)
		}
	}
	if n := len(d.Failed) + len(d.Feedback); n > 0 {
		d.Reason = fmt.Sprintf("%d items", n)
		return d
	}
	if s.ReviewDecision != "APPROVED" {
		return verdict(NeedsHuman, "needs review")
	}
	return verdict(Ready, "approved and green; merge or enable auto-merge")
}

// waitingOn names what rule 6 waits on, or returns "" if nothing.
func waitingOn(s Snapshot) string {
	if s.Mergeable != "MERGEABLE" {
		return "mergeability"
	}
	if len(s.Checks) == 0 {
		return "checks to be reported"
	}
	for _, c := range s.Checks {
		if classify(c) == CheckPending {
			return "check " + c.Name
		}
	}
	return ""
}

func isWriter(a Actor) bool { return a.Writer && !a.Bot }

// shaLike matches a run of hex digits long enough to name a commit.
var shaLike = regexp.MustCompile(`(?i)\b[0-9a-f]{7,40}\b`)

// isItem reports whether rule 9 counts f. A listed bot's feedback counts only
// if it names the head; a writer's unresolved thread or change request counts
// only if newer than the bot's last push, or always before its first push.
func isItem(s Snapshot, st State, f Feedback) bool {
	if slices.Contains(s.ReviewerBots, f.Author.ID) {
		return !f.Resolved && slices.ContainsFunc(shaLike.FindAllString(f.Body, -1), func(m string) bool {
			return strings.HasPrefix(s.Head, strings.ToLower(m))
		})
	}
	if !isWriter(f.Author) || st.LastPushSHA != "" && !f.CreatedAt.After(st.LastPushAt) {
		return false
	}
	return f.Kind == "thread" && !f.Resolved || f.Kind == "review" && f.State == "CHANGES_REQUESTED"
}
