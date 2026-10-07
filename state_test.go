// ABOUTME: Table tests for the state rules in state.go: check classification and the eleven rules of spec §3.
// ABOUTME: CI-approval and duplicate-run cases decode GitHub responses recorded in testdata/task0.

package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// head matches the PR head in testdata/task0/q3-*-before-approval.json.
const head = "c6ffea8a610078778f06b51ff059ceeb44fcf3a4"

var (
	now    = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	writer = Actor{ID: 1, Login: "michelle", Writer: true}
	reader = Actor{ID: 2, Login: "drive-by"}
	revBot = Actor{ID: 99, Login: "review-bot", Bot: true}
)

// readySnapshot is a PR that rule 11 calls ready; each case changes one thing.
func readySnapshot() Snapshot {
	return Snapshot{
		Head: head, Mergeable: "MERGEABLE", ReviewDecision: "APPROVED",
		LabelEvent: "LE_1", Labeler: writer,
		Checks:         []Check{{Name: "test", Status: "COMPLETED", Conclusion: "SUCCESS"}},
		RequiredChecks: true, DismissStale: true, LastPushApproval: true,
		ReviewerBots: []int64{revBot.ID},
		State: State{V: 1, LabelEvent: "LE_1", Owner: "michelle",
			LastPushSHA: "1111111111111111111111111111111111111111", LastPushAt: now.Add(-2 * time.Hour),
			HeadSeenSHA: head, HeadSeenAt: now.Add(-time.Minute)},
		Now: now,
	}
}

func decodeFile(t *testing.T, name string, v any) {
	t.Helper()
	b, err := os.ReadFile("testdata/task0/" + name)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}

func TestClassify(t *testing.T) {
	want := map[Class][]string{
		CheckPass:    {"success", "neutral", "skipped"},
		CheckFail:    {"failure", "timed_out", "startup_failure", "error"},
		CheckHuman:   {"cancelled", "action_required", "stale"},
		CheckPending: {"queued", "in_progress", "waiting", "pending", "requested", "expected", "", "something_new"},
	}
	for class, values := range want {
		for _, v := range values {
			// A completed check is judged on its conclusion; anything else on its status.
			for _, c := range []Check{{Status: "completed", Conclusion: v}, {Status: strings.ToUpper(v)}} {
				if v == "" && c.Status == "" {
					continue
				}
				if got := classify(c); got != class {
					t.Errorf("classify(%+v) = %v, want %v", c, got, class)
				}
			}
		}
	}
	// A completed check with no conclusion fails safe.
	if got := classify(Check{Status: "COMPLETED"}); got != CheckPending {
		t.Errorf("completed without conclusion = %v, want Pending", got)
	}
}

func TestDecide(t *testing.T) {
	thread := func(a Actor, at time.Time) Feedback {
		return Feedback{Kind: "thread", Author: a, Body: "fix this", CreatedAt: at}
	}
	newer, older := now.Add(-time.Hour), now.Add(-3*time.Hour)
	var approvalSuites struct {
		CheckSuites []Suite `json:"check_suites"`
	}
	decodeFile(t, "q3-check-suites-before-approval.json", &approvalSuites)

	cases := []struct {
		name   string
		change func(s *Snapshot)
		kind   Kind
		reason string // substring of the decision's reason
		items  int    // failed checks plus feedback items
	}{
		// Rule 1: adoption.
		{"fork", func(s *Snapshot) { s.Fork = true }, Skip, "not supported", 0},
		{"label from non-writer", func(s *Snapshot) { s.Labeler = reader }, Skip, "", 0},
		{"label from bot", func(s *Snapshot) { s.Labeler = Actor{ID: 3, Writer: true, Bot: true} }, Skip, "", 0},
		{"no label event", func(s *Snapshot) { s.LabelEvent = "" }, Skip, "", 0},
		// Rule 2: base branch setup.
		{"no required checks", func(s *Snapshot) { s.RequiredChecks = false }, NeedsHuman, "repo setup incomplete", 0},
		{"no stale dismissal", func(s *Snapshot) { s.DismissStale = false }, NeedsHuman, "repo setup incomplete", 0},
		{"no last-push approval", func(s *Snapshot) { s.LastPushApproval = false }, NeedsHuman, "repo setup incomplete", 0},
		// Rule 3: round limit.
		{"five rounds", func(s *Snapshot) { s.State.Rounds = 5 }, NeedsHuman, "5 rounds", 0},
		{"new label resets rounds", func(s *Snapshot) { s.State.Rounds = 5; s.LabelEvent = "LE_2" }, Ready, "", 0},
		// Rule 4: conflicts.
		{"conflicts", func(s *Snapshot) { s.Mergeable = "CONFLICTING" }, NeedsHuman, "resolve conflicts", 0},
		// Rule 5: CI on the bot's push awaits approval (recorded: suite action_required, rollup null).
		{"bot head awaiting approval", func(s *Snapshot) {
			s.State.LastPushSHA, s.Checks, s.Suites = head, nil, approvalSuites.CheckSuites
		}, NeedsHuman, "approve the workflow runs", 0},
		{"human head with same suites waits", func(s *Snapshot) {
			s.Checks, s.Suites = nil, approvalSuites.CheckSuites
		}, Waiting, "", 0},
		// Rule 6: pending or unknown.
		{"pending check", func(s *Snapshot) { s.Checks = append(s.Checks, Check{Name: "lint", Status: "IN_PROGRESS"}) }, Waiting, "lint", 0},
		{"failed and running", func(s *Snapshot) {
			s.Checks = []Check{{Name: "test", Status: "COMPLETED", Conclusion: "FAILURE"}, {Name: "lint", Status: "QUEUED"}}
		}, Waiting, "lint", 0},
		{"mergeability unknown", func(s *Snapshot) { s.Mergeable = "UNKNOWN" }, Waiting, "mergeability", 0},
		{"no checks yet", func(s *Snapshot) { s.Checks = nil }, Waiting, "checks", 0},
		{"stuck 60 minutes", func(s *Snapshot) {
			s.Checks = append(s.Checks, Check{Name: "lint", Status: "QUEUED"})
			s.State.HeadSeenAt = now.Add(-60 * time.Minute)
		}, NeedsHuman, "lint", 0},
		{"stuck clock restarts on new head", func(s *Snapshot) {
			s.Checks = append(s.Checks, Check{Name: "lint", Status: "QUEUED"})
			s.State.HeadSeenSHA, s.State.HeadSeenAt = "2222222", now.Add(-2*time.Hour)
		}, Waiting, "lint", 0},
		// Rule 7: a check needs a person.
		{"cancelled check", func(s *Snapshot) { s.Checks[0].Conclusion = "CANCELLED" }, NeedsHuman, "check test ended cancelled", 0},
		{"stale check", func(s *Snapshot) { s.Checks[0].Conclusion = "STALE" }, NeedsHuman, "test", 0},
		// Rule 8: last round made no push and nobody acted since.
		{"round without push", func(s *Snapshot) {
			s.State.Rounds, s.State.RoundHead, s.State.Outcome = 1, head, "no fix found"
			s.RoundEndedAt = now.Add(-10 * time.Minute)
			s.Feedback = []Feedback{thread(writer, newer)}
		}, NeedsHuman, "no fix found", 0},
		{"writer commented after round", func(s *Snapshot) {
			s.State.Rounds, s.State.RoundHead, s.State.Outcome = 1, head, "no fix found"
			s.RoundEndedAt = now.Add(-10 * time.Minute)
			s.Feedback = []Feedback{thread(writer, newer), {Kind: "comment", Author: writer, CreatedAt: now.Add(-time.Minute)}}
		}, Round, "", 1},
		{"non-writer comment after round doesn't count", func(s *Snapshot) {
			s.State.Rounds, s.State.RoundHead, s.State.Outcome = 1, head, "no fix found"
			s.RoundEndedAt = now.Add(-10 * time.Minute)
			s.Feedback = []Feedback{{Kind: "comment", Author: reader, CreatedAt: now.Add(-time.Minute)}}
		}, NeedsHuman, "no fix found", 0},
		{"someone pushed after round", func(s *Snapshot) {
			s.State.Rounds, s.State.RoundHead, s.State.Outcome = 1, "3333333", "no fix found"
			s.RoundEndedAt = now.Add(-10 * time.Minute)
		}, Ready, "", 0},
		{"round still running is recorded failed", func(s *Snapshot) {
			s.State.Rounds, s.State.RoundHead, s.State.Outcome = 1, head, "running"
			s.RoundEndedAt = now.Add(-10 * time.Minute)
		}, NeedsHuman, "failed", 0},
		// Rule 9: items.
		{"failed check", func(s *Snapshot) { s.Checks[0].Conclusion = "FAILURE" }, Round, "", 1},
		{"timed out check", func(s *Snapshot) { s.Checks[0].Conclusion = "TIMED_OUT" }, Round, "", 1},
		{"errored status", func(s *Snapshot) { s.Checks[0] = Check{Name: "ext", Status: "ERROR"} }, Round, "", 1},
		{"writer thread after last push", func(s *Snapshot) { s.Feedback = []Feedback{thread(writer, newer)} }, Round, "", 1},
		{"writer thread before last push", func(s *Snapshot) { s.Feedback = []Feedback{thread(writer, older)} }, Ready, "", 0},
		{"any writer thread before first push", func(s *Snapshot) {
			s.State.LastPushSHA, s.State.LastPushAt = "", time.Time{}
			s.Feedback = []Feedback{thread(writer, older)}
		}, Round, "", 1},
		{"resolved thread", func(s *Snapshot) {
			f := thread(writer, newer)
			f.Resolved = true
			s.Feedback = []Feedback{f}
		}, Ready, "", 0},
		{"non-writer thread", func(s *Snapshot) { s.Feedback = []Feedback{thread(reader, newer)} }, Ready, "", 0},
		{"writer bot thread", func(s *Snapshot) {
			s.Feedback = []Feedback{thread(Actor{ID: 4, Writer: true, Bot: true}, newer)}
		}, Ready, "", 0},
		{"writer change request", func(s *Snapshot) {
			s.Feedback = []Feedback{{Kind: "review", Author: writer, State: "CHANGES_REQUESTED", CreatedAt: newer}}
		}, Round, "", 1},
		{"writer plain review", func(s *Snapshot) {
			s.Feedback = []Feedback{{Kind: "review", Author: writer, State: "COMMENTED", CreatedAt: newer}}
		}, Ready, "", 0},
		{"writer plain comment", func(s *Snapshot) {
			s.Feedback = []Feedback{{Kind: "comment", Author: writer, Body: "fix", CreatedAt: newer}}
		}, Ready, "", 0},
		{"listed bot names head", func(s *Snapshot) {
			s.Feedback = []Feedback{{Kind: "comment", Author: revBot, Body: "Review of " + head[:7] + ": 1 finding", CreatedAt: older}}
		}, Round, "", 1},
		{"listed bot names head in capitals", func(s *Snapshot) {
			s.Feedback = []Feedback{{Kind: "comment", Author: revBot, Body: "at " + strings.ToUpper(head), CreatedAt: older}}
		}, Round, "", 1},
		{"listed bot with 6-hex prefix", func(s *Snapshot) {
			s.Feedback = []Feedback{{Kind: "comment", Author: revBot, Body: "Review of " + head[:6], CreatedAt: newer}}
		}, Ready, "", 0},
		{"listed bot names old commit", func(s *Snapshot) {
			s.Feedback = []Feedback{{Kind: "comment", Author: revBot, Body: "Review of 5ae3167", CreatedAt: newer}}
		}, Ready, "", 0},
		{"unlisted bot names head", func(s *Snapshot) {
			s.Feedback = []Feedback{{Kind: "comment", Author: Actor{ID: 5, Bot: true}, Body: head, CreatedAt: newer}}
		}, Ready, "", 0},
		// Rules 10 and 11.
		{"not approved", func(s *Snapshot) { s.ReviewDecision = "REVIEW_REQUIRED" }, NeedsHuman, "needs review", 0},
		{"ready", func(s *Snapshot) {}, Ready, "merge or enable auto-merge", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := readySnapshot()
			c.change(&s)
			d := decide(s)
			if d.Kind != c.kind || !strings.Contains(d.Reason, c.reason) {
				t.Errorf("decide = %v %q, want %v containing %q", d.Kind, d.Reason, c.kind, c.reason)
			}
			if got := len(d.Failed) + len(d.Feedback); got != c.items {
				t.Errorf("items = %d (%+v %+v), want %d", got, d.Failed, d.Feedback, c.items)
			}
		})
	}
}

func TestDecideState(t *testing.T) {
	s := readySnapshot()
	s.LabelEvent, s.Labeler.Login = "LE_2", "pat"
	s.State.Rounds, s.State.RoundHead, s.State.Outcome = 5, "3333333", "no fix found"
	got := decide(s).State
	if got.LabelEvent != "LE_2" || got.Owner != "pat" || got.Rounds != 0 || got.Outcome != "" {
		t.Errorf("new label: state = %+v, want reset to LE_2, owner pat, 0 rounds", got)
	}
	if got.LastPushSHA != s.State.LastPushSHA || !got.LastPushAt.Equal(s.State.LastPushAt) {
		t.Errorf("new label lost the bot's last push: %+v", got)
	}

	s = readySnapshot()
	s.State.Rounds, s.State.RoundHead, s.State.Outcome = 2, head, "running"
	if got := decide(s).State; !strings.HasPrefix(got.Outcome, "failed") || got.Rounds != 2 {
		t.Errorf("running round: state = %+v, want outcome failed, rounds 2", got)
	}

	s = readySnapshot()
	s.Checks[0].Conclusion = "CANCELLED"
	if got := decide(s).Reason; got != "check test ended cancelled" {
		t.Errorf("cancelled check: reason = %q", got)
	}

	s = readySnapshot()
	s.State.HeadSeenSHA = "2222222"
	if got := decide(s).State; got.HeadSeenSHA != head || !got.HeadSeenAt.Equal(now) {
		t.Errorf("new head: state = %+v, want head seen %s at %v", got, head, now)
	}
}

func TestDecideDuplicateRuns(t *testing.T) {
	var runs struct {
		WorkflowRuns []Check `json:"workflow_runs"`
	}
	decodeFile(t, "setup-main-push-runs.json", &runs)
	if len(runs.WorkflowRuns) != 2 || runs.WorkflowRuns[0].Name != runs.WorkflowRuns[1].Name {
		t.Fatalf("testdata should hold two runs of one check, got %+v", runs.WorkflowRuns)
	}
	s := readySnapshot()
	s.Checks = runs.WorkflowRuns
	if d := decide(s); d.Kind != Ready {
		t.Errorf("two passing runs: %v %q, want ready", d.Kind, d.Reason)
	}
	s.Checks[1].Conclusion = "failure"
	if d := decide(s); d.Kind != Round || len(d.Failed) != 1 {
		t.Errorf("one failing run of two: %v %+v, want a round with one failed check", d.Kind, d.Failed)
	}
}
