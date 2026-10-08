// ABOUTME: Tests for github.go: decoding recorded GitHub responses, and plan driven against a fake gh.
// ABOUTME: Fixtures under testdata/ are real responses from the sandbox repo unless a test builds its own JSON.

package main

import (
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	sandbox     = "michellepellon/pr-babysitter-sandbox"
	sandboxHead = "c6ffea8a610078778f06b51ff059ceeb44fcf3a4"
)

func readFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// jsonLines prints each element of a JSON array, or of the array under key,
// one per line, as gh does with --jq '.[]'.
func jsonLines(t *testing.T, data, key string) string {
	t.Helper()
	var all []json.RawMessage
	if key != "" {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal([]byte(data), &obj); err != nil {
			t.Fatal(err)
		}
		data = string(obj[key])
	}
	if err := json.Unmarshal([]byte(data), &all); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, v := range all {
		b.Write(v)
		b.WriteByte('\n')
	}
	return b.String()
}

// fakeGH answers plan's gh calls from fixtures, records writes, and serves
// back the last comment it was sent as the PR's existing comment.
type fakeGH struct {
	t        *testing.T
	graphql  string
	roles    map[string]string // login → role_name
	comments string            // issue comments, as gh prints them with --jq '.[]'
	writes   [][]string
	lookups  []string
}

func newFakeGH(t *testing.T, graphql string) *fakeGH {
	f := &fakeGH{t: t, graphql: graphql, roles: map[string]string{"michellepellon": "admin"},
		comments: jsonLines(t, readFixture(t, "plan/issue-comments.json"), "")}
	old, oldWriters := gh, writers
	t.Cleanup(func() { gh, writers = old, oldWriters })
	gh, writers = f.run, map[string]bool{}
	return f
}

func (f *fakeGH) run(args ...string) (string, error) {
	joined := strings.Join(args, " ")
	switch {
	case slices.Contains(args, "graphql"):
		if !slices.Contains(args, "owner=michellepellon") || !slices.Contains(args, "name=pr-babysitter-sandbox") {
			f.t.Errorf("graphql called without owner and name variables: %q", args)
		}
		return f.graphql, nil
	case strings.HasSuffix(joined, "/permission"):
		login := strings.Split(args[1], "/")[4]
		f.lookups = append(f.lookups, login)
		return `{"permission":"x","role_name":"` + f.roles[login] + `"}`, nil
	case strings.Contains(joined, "/rules/branches/main "):
		return jsonLines(f.t, readFixture(f.t, "task0/q1-rules-branch-main-user-auth.json"), ""), nil
	case strings.Contains(joined, "/commits/"+sandboxHead+"/check-suites "):
		return jsonLines(f.t, readFixture(f.t, "task0/q3-check-suites-baseline-human-push.json"), "check_suites"), nil
	case slices.Contains(args, "-f") && strings.Contains(joined, "/issues/2/"):
		return "", errors.New("HTTP 502")
	case slices.Contains(args, "-f"): // a comment write
		f.writes = append(f.writes, args)
		body := strings.TrimPrefix(args[len(args)-1], "body=")
		c, _ := json.Marshal(map[string]any{"id": 99, "body": body, "user": map[string]any{"id": botUserID, "login": "github-actions[bot]", "type": "Bot"}})
		f.comments = string(c) + "\n"
		return "{}", nil
	case strings.Contains(joined, "/issues/1/comments --paginate"):
		return f.comments, nil
	case strings.Contains(joined, "/issues/2/comments --paginate"):
		return "", nil
	case args[0] == "run" && args[1] == "view":
		return strings.Repeat("x", 300<<10) + "TAIL", nil
	}
	f.t.Fatalf("unexpected gh call: %q", args)
	return "", nil
}

func decodePRs(t *testing.T, data string) []ghPR {
	t.Helper()
	var r prResponse
	if err := json.Unmarshal([]byte(data), &r); err != nil {
		t.Fatal(err)
	}
	return r.Data.Repository.PullRequests.Nodes
}

func TestToSnapshotDecodesRecordedPR(t *testing.T) {
	newFakeGH(t, "")
	prs := decodePRs(t, readFixture(t, "plan/graphql-prs.json"))
	if len(prs) != 1 {
		t.Fatalf("got %d PRs, want 1", len(prs))
	}
	s := toSnapshot(sandbox, prs[0], nil)
	want := Actor{ID: 122621769, Login: "michellepellon", Writer: true}
	if s.Number != 1 || s.Branch != "spike/test-pr" || s.Base != "main" || s.Fork || s.Head != sandboxHead ||
		s.Mergeable != "MERGEABLE" || s.ReviewDecision != "REVIEW_REQUIRED" {
		t.Errorf("PR fields = %+v", s)
	}
	if !strings.HasPrefix(s.LabelEvent, "LE_") || s.Labeler != want || s.LabelAt.IsZero() {
		t.Errorf("label event = %q by %+v at %v; want an LE_ node by %+v", s.LabelEvent, s.Labeler, s.LabelAt, want)
	}
	if len(s.Checks) != 1 || s.Checks[0] != (Check{Name: "test", Status: "COMPLETED", Conclusion: "SUCCESS", Run: 37653357605}) {
		t.Errorf("checks = %+v", s.Checks)
	}
}

func TestToSnapshotPicksLabelEventsThreadsAndStatuses(t *testing.T) {
	f := newFakeGH(t, "")
	f.roles["writer"], f.roles["stranger"] = "write", "triage"
	pr := `{"timelineItems": {"nodes": [
	    {"id": "LE_old", "createdAt": "2026-10-01T00:00:00Z", "label": {"name": "babysit"}, "author": {"__typename": "User", "login": "writer", "databaseId": 2}},
	    {"id": "LE_new", "createdAt": "2026-10-02T00:00:00Z", "label": {"name": "babysit"}, "author": {"__typename": "User", "login": "stranger", "databaseId": 3}},
	    {"id": "LE_other", "createdAt": "2026-10-03T00:00:00Z", "label": {"name": "bug"}, "author": {"__typename": "User", "login": "writer", "databaseId": 2}}]},
	  "commits": {"nodes": [{"commit": {"statusCheckRollup": {"contexts": {"nodes": [{"context": "ci/legacy", "state": "PENDING"}]}}}}]},
	  "reviewThreads": {"nodes": [{"isResolved": false, "comments": {"nodes": [
	    {"author": {"__typename": "User", "login": "writer", "databaseId": 2}, "body": "first", "createdAt": "2026-10-04T00:00:00Z"},
	    {"author": {"__typename": "User", "login": "writer", "databaseId": 2}, "body": "newest writer", "createdAt": "2026-10-05T00:00:00Z"},
	    {"author": {"__typename": "User", "login": "stranger", "databaseId": 3}, "body": "ignore me", "createdAt": "2026-10-06T00:00:00Z"},
	    {"author": {"__typename": "Bot", "login": "some-bot", "databaseId": 9}, "body": "unlisted bot", "createdAt": "2026-10-07T00:00:00Z"}]}}]},
	  "reviews": {"nodes": [{"author": null, "body": "ghost", "state": "CHANGES_REQUESTED", "createdAt": "2026-10-04T00:00:00Z"}]}}`
	var p ghPR
	if err := json.Unmarshal([]byte(pr), &p); err != nil {
		t.Fatal(err)
	}
	s := toSnapshot(sandbox, p, nil)
	if s.LabelEvent != "LE_new" || s.Labeler.Login != "stranger" || s.Labeler.Writer {
		t.Errorf("label event = %q by %+v; want LE_new by a non-writer", s.LabelEvent, s.Labeler)
	}
	if len(s.Checks) != 1 || s.Checks[0] != (Check{Name: "ci/legacy", Status: "PENDING"}) {
		t.Errorf("status context = %+v; want its state in Status", s.Checks)
	}
	want := Feedback{Kind: "thread", Author: Actor{ID: 2, Login: "writer", Writer: true}, Body: "newest writer",
		CreatedAt: time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)}
	if len(s.Feedback) != 2 || s.Feedback[0] != want {
		t.Errorf("thread = %+v; want %+v", s.Feedback, want)
	}
	if s.Feedback[1].Author != (Actor{}) {
		t.Errorf("review by a deleted user = %+v; want a zero author", s.Feedback[1])
	}
	if slices.Compare(f.lookups, []string{"writer", "stranger"}) != 0 {
		t.Errorf("writer lookups = %q; want each person once, and no bots", f.lookups)
	}
}

func TestToSnapshotTakesListedBotThreadComment(t *testing.T) {
	newFakeGH(t, "")
	var p ghPR
	json.Unmarshal([]byte(`{"reviewThreads": {"nodes": [{"comments": {"nodes": [
	  {"author": {"__typename": "Bot", "login": "roborev", "databaseId": 9}, "body": "review of abc1234", "createdAt": "2026-10-07T00:00:00Z"}]}}]}}`), &p)
	s := toSnapshot(sandbox, p, []int64{9})
	if len(s.Feedback) != 1 || s.Feedback[0].Body != "review of abc1234" || s.Feedback[0].Author.ID != 9 {
		t.Errorf("listed bot thread = %+v", s.Feedback)
	}
}

func TestRuleFlags(t *testing.T) {
	var rules []ghRule
	if err := json.Unmarshal([]byte(readFixture(t, "task0/q1-rules-branch-main-user-auth.json")), &rules); err != nil {
		t.Fatal(err)
	}
	if a, b, c := ruleFlags(rules); !a || !b || !c {
		t.Errorf("sandbox ruleset = %v %v %v; want all true", a, b, c)
	}
	rules[0].Parameters.Checks = nil
	rules[1].Parameters.LastPush = false
	if a, b, c := ruleFlags(rules); a || !b || c {
		t.Errorf("no required checks, no last-push approval = %v %v %v; want false true false", a, b, c)
	}
}

func TestSuiteDecodesAwaitingApproval(t *testing.T) {
	var r struct {
		CheckSuites []Suite `json:"check_suites"`
	}
	json.Unmarshal([]byte(readFixture(t, "task0/q3-check-suites-before-approval.json")), &r)
	if len(r.CheckSuites) != 1 || r.CheckSuites[0] != (Suite{Conclusion: "action_required", Runs: 0}) {
		t.Errorf("suites = %+v", r.CheckSuites)
	}
}

func TestWriterRoles(t *testing.T) {
	var p ghPermission
	json.Unmarshal([]byte(readFixture(t, "plan/permission-admin.json")), &p)
	if p.Role != "admin" {
		t.Fatalf("decoded role %q, want admin", p.Role)
	}
	f := newFakeGH(t, "")
	for role, want := range map[string]bool{"admin": true, "maintain": true, "write": true, "triage": false, "read": false, "": false} {
		f.roles["u-"+role] = role
		if got := toActor(sandbox, ghActor{Type: "User", Login: "u-" + role, ID: 5}); got.Writer != want {
			t.Errorf("role %q: writer = %v, want %v", role, got.Writer, want)
		}
	}
	if got := toActor(sandbox, ghActor{Type: "User", Login: "../../x", ID: 5}); got.Writer || slices.Contains(f.lookups, "../../x") {
		t.Errorf("odd login was looked up or made a writer: %+v", got)
	}
}

func TestCommentBodyKeepsSummaryAndFencesReason(t *testing.T) {
	first := commentBody("", sampleState, "**waiting**", "waiting on check @evil ![x](https://e.invalid/x.png)")
	withSummary := renderComment(sampleState, "**round 1 running**", "3 items\n\nFixed the lint.\n```\nmore")
	got := strings.ReplaceAll(commentBody(strings.ReplaceAll(withSummary, "\n", "\r\n"), sampleState, "**ready** @michellepellon", "approved"), "\r", "")
	if !strings.Contains(got, "approved\n\nFixed the lint.\n```\nmore\n````") {
		t.Errorf("rerender lost the summary or kept the old reason:\n%s", got)
	}
	if lines := strings.Split(first, "\n"); lines[1] != "**waiting**" || !strings.HasPrefix(lines[3], "```") {
		t.Errorf("reason escaped the fence:\n%s", first)
	}
	if again := commentBody(got, sampleState, "**ready** @michellepellon", "approved"); again != got {
		t.Errorf("rerender with no change altered the text:\n%s\n---\n%s", got, again)
	}
}

func TestStillAdopted(t *testing.T) {
	f := newFakeGH(t, "")
	prs := decodePRs(t, readFixture(t, "plan/graphql-prs.json"))
	if err := stillAdopted(sandbox, prs, "1", sandboxHead); err != nil {
		t.Errorf("open, labeled, same head: %v", err)
	}
	for _, c := range [][2]string{{"1", strings.Repeat("0", 40)}, {"2", sandboxHead}} {
		if err := stillAdopted(sandbox, prs, c[0], c[1]); err == nil {
			t.Errorf("PR %s at %s: got nil, want stale", c[0], c[1])
		}
	}
	prs[0].IsCrossRepository = true
	if err := stillAdopted(sandbox, prs, "1", sandboxHead); err == nil {
		t.Error("fork: got nil, want stale")
	}
	prs[0].IsCrossRepository, f.roles["michellepellon"], writers = false, "read", map[string]bool{}
	if err := stillAdopted(sandbox, prs, "1", sandboxHead); err == nil {
		t.Error("labeler lost write access: got nil, want stale")
	}
}

func planOnce(t *testing.T, f *fakeGH) (State, string, string) {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "output")
	if err := plan(sandbox, filepath.Join(dir, "items"), out, nil, time.Date(2026, 10, 8, 3, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	outputs, _ := os.ReadFile(out)
	items, _ := os.ReadFile(filepath.Join(dir, "items", "items.json"))
	var c struct{ Body string }
	json.Unmarshal([]byte(f.comments), &c)
	st, _ := parseState(botUserID, c.Body)
	return st, string(outputs), string(items)
}

func TestPlanStartsRoundAndWritesStateFirst(t *testing.T) {
	f := newFakeGH(t, strings.Replace(readFixture(t, "plan/graphql-prs.json"), `"SUCCESS"`, `"FAILURE"`, 1))
	st, outputs, items := planOnce(t, f)
	if len(f.writes) != 1 || !strings.HasSuffix(f.writes[0][1], "/issues/1/comments") {
		t.Fatalf("writes = %q; want one new comment on PR 1", f.writes)
	}
	if st.Rounds != 1 || st.RoundHead != sandboxHead || st.Outcome != "running" || st.Owner != "michellepellon" {
		t.Errorf("state = %+v; want round 1 running on the head", st)
	}
	if outputs != "pr=1\nhead="+sandboxHead+"\nbranch=spike/test-pr\n" {
		t.Errorf("outputs = %q", outputs)
	}
	if !strings.Contains(items, `"name": "test"`) || !strings.Contains(items, `"Branch": "spike/test-pr"`) {
		t.Errorf("items = %s", items)
	}
}

func TestPlanKeepsFailedLogTail(t *testing.T) {
	newFakeGH(t, strings.Replace(readFixture(t, "plan/graphql-prs.json"), `"SUCCESS"`, `"FAILURE"`, 1))
	dir := t.TempDir()
	if err := plan(sandbox, dir, filepath.Join(dir, "out"), nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	log, err := os.ReadFile(filepath.Join(dir, "logs", "37653357605.log"))
	if err != nil || len(log) != 200<<10 || !strings.HasSuffix(string(log), "TAIL") {
		t.Errorf("log = %d bytes, %v; want the last 200 KB", len(log), err)
	}
}

func TestPlanEditsOnlyWhenTextChanges(t *testing.T) {
	f := newFakeGH(t, readFixture(t, "plan/graphql-prs.json"))
	st, outputs, _ := planOnce(t, f)
	if len(f.writes) != 1 || st.Rounds != 0 || outputs != "" {
		t.Fatalf("first run: writes %q, state %+v, outputs %q; want one comment, no round", f.writes, st, outputs)
	}
	if body := f.writes[0][len(f.writes[0])-1]; strings.Split(body, "\n")[1] != "**needs-human** @michellepellon" {
		t.Errorf("status = %q; want needs-human with an @mention", body)
	}
	planOnce(t, f)
	if len(f.writes) != 1 {
		t.Errorf("second run with nothing new wrote again: %q", f.writes[1:])
	}
}

func TestPlanSkipsForksOnceAndNonWritersSilently(t *testing.T) {
	f := newFakeGH(t, strings.Replace(readFixture(t, "plan/graphql-prs.json"), `"isCrossRepository":false`, `"isCrossRepository":true`, 1))
	planOnce(t, f)
	planOnce(t, f)
	if len(f.writes) != 1 || !strings.Contains(f.writes[0][len(f.writes[0])-1], "fork PRs are not supported") {
		t.Errorf("fork: writes = %q; want one not-supported comment", f.writes)
	}
	f = newFakeGH(t, readFixture(t, "plan/graphql-prs.json"))
	f.roles["michellepellon"] = "read"
	if _, outputs, _ := planOnce(t, f); len(f.writes) != 0 || outputs != "" {
		t.Errorf("non-writer label: writes %q, outputs %q; want nothing", f.writes, outputs)
	}
}

func TestPlanWritesRunningStateLast(t *testing.T) {
	// Two identical failing PRs: 1 is picked, and 2's comment write fails. PR 1 must not be left running.
	data := strings.Replace(readFixture(t, "plan/graphql-prs.json"), `"SUCCESS"`, `"FAILURE"`, 1)
	var r map[string]map[string]map[string]map[string][]map[string]any
	json.Unmarshal([]byte(data), &r)
	prs := r["data"]["repository"]["pullRequests"]
	second := maps.Clone(prs["nodes"][0])
	second["number"] = 2
	prs["nodes"] = append(prs["nodes"], second)
	b, _ := json.Marshal(r)
	f := newFakeGH(t, string(b))
	dir := t.TempDir()
	if err := plan(sandbox, dir, filepath.Join(dir, "out"), nil, time.Now()); err == nil || len(f.writes) != 0 {
		t.Errorf("plan = %v, writes %q; want the error and no write to the picked PR", err, f.writes)
	}
}
