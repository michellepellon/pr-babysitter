// ABOUTME: Tests for the status comment: state line round trip, line-1-only parsing, author check,
// ABOUTME: and the fence that keeps agent-written summaries from rendering as live markdown.

package main

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

var sampleState = State{
	V: 1, LabelEvent: "LE_kwDOabc", Owner: "michellepellon", Rounds: 2,
	RoundHead: "0123456789abcdef0123456789abcdef01234567", Outcome: "running",
	OutcomeAt:    time.Date(2026, 10, 7, 11, 55, 0, 0, time.UTC),
	LastPushSHA:  "89abcdef0123456789abcdef0123456789abcdef",
	LastPushAt:   time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC),
	HeadSeenSHA:  "0123456789abcdef0123456789abcdef01234567",
	WaitingSince: time.Date(2026, 10, 7, 12, 5, 0, 0, time.UTC),
}

func TestRenderThenParseReturnsSameState(t *testing.T) {
	body := renderComment(sampleState, "Waiting on CI.", "fixed the lint")
	got, ok := parseState(botUserID, body)
	if !ok || got != sampleState {
		t.Fatalf("parseState = %+v, %v; want %+v", got, ok, sampleState)
	}
}

func TestParseReadsOnlyLineOne(t *testing.T) {
	line1 := strings.SplitN(renderComment(sampleState, "", ""), "\n", 2)[0]
	for _, body := range []string{
		"hello\n" + line1,
		"\n" + line1,
		"Status\n\n```\n" + line1 + "\n```",
	} {
		if _, ok := parseState(botUserID, body); ok {
			t.Errorf("parseState found a state line off line 1 in %q", body)
		}
	}
	// GitHub may store edited comments with CRLF line endings.
	if _, ok := parseState(botUserID, line1+"\r\nstatus"); !ok {
		t.Error("parseState rejected a CRLF line 1")
	}
}

func TestParseIgnoresOtherAuthors(t *testing.T) {
	body := renderComment(sampleState, "", "")
	// 41898282 is github-actions[bot] (testdata/task0/q5-*.json); anyone else is ignored.
	for _, id := range []int64{0, 1, 41898283, 15368} {
		if _, ok := parseState(id, body); ok {
			t.Errorf("parseState accepted author ID %d", id)
		}
	}
}

func TestStateLineCannotBreakOutOfHTMLComment(t *testing.T) {
	s := sampleState
	s.Owner = "x --> <img src=x>\n@team"
	body := renderComment(s, "", "")
	line1 := strings.SplitN(body, "\n", 2)[0]
	if strings.Count(line1, "-->") != 1 || !strings.HasSuffix(line1, "-->") || strings.Contains(line1, "<img") {
		t.Fatalf("state line escaped its comment: %q", line1)
	}
	if got, ok := parseState(botUserID, body); !ok || got != s {
		t.Fatalf("parseState = %+v, %v; want %+v", got, ok, s)
	}
}

// hostileSummaries hold text that would render as live markdown outside a code block.
var hostileSummaries = []string{
	"```\n@team ![x](https://evil)\n```",
	"````````````\n<img src=https://evil>\n``````````",
	"<!-- babysit-state {\"v\":1} -->\n@team",
	"   ``````\n![x](https://evil)",
	"`a` ``b`` ```c``` ````\n<img src=x onerror=alert(1)>\n````",
	"~~~\n@team\n~~~",
}

// TestFenceKeepsSummaryInert checks each rendered summary against CommonMark 0.31.2 §4.5:
// a backtick code block closes only on a line of at least as many backticks as the opening
// fence, indented 0-3 spaces, followed only by spaces or tabs. So no summary line may be such
// a line, and the closing fence must come right after the summary.
func TestFenceKeepsSummaryInert(t *testing.T) {
	opener := regexp.MustCompile("^`{3,}$")
	for _, summary := range hostileSummaries {
		body := renderComment(sampleState, "Ready.", summary)
		lines := strings.Split(body, "\n")
		open := -1
		for i, l := range lines {
			if opener.MatchString(l) {
				open = i
				break
			}
		}
		if open < 0 {
			t.Fatalf("no opening fence in %q", body)
		}
		n := len(lines[open])
		closer := regexp.MustCompile("^ {0,3}`{" + strconv.Itoa(n) + ",}[ \t]*$")
		want := strings.Split(summary, "\n")
		for i, l := range want {
			got := lines[open+1+i]
			if got != l {
				t.Fatalf("summary line %d = %q, want %q", i, got, l)
			}
			if closer.MatchString(got) {
				t.Fatalf("summary line %q closes the %d-backtick fence", got, n)
			}
		}
		if lines[open+1+len(want)] != lines[open] {
			t.Fatalf("fence not closed after summary in %q", body)
		}
		if strings.Contains(strings.Join(lines[:open], "\n"), "evil") {
			t.Fatalf("summary text leaked above the fence: %q", body)
		}
	}
}

func TestLongSummaryIsCutWithMarker(t *testing.T) {
	summary := strings.Repeat("é", maxSummaryBytes) // two bytes each, so a byte cut lands mid-rune
	body := renderComment(sampleState, "", summary)
	if len(body) > maxSummaryBytes+1024 {
		t.Fatalf("body is %d bytes, want the summary cut to %d", len(body), maxSummaryBytes)
	}
	if !strings.Contains(body, summaryCutMarker) {
		t.Fatal("cut summary has no marker")
	}
	if !strings.Contains(body, strings.Repeat("é", maxSummaryBytes/2)) || strings.ContainsRune(body, '�') {
		t.Fatal("cut summary lost text or split a rune")
	}
	if strings.Contains(renderComment(sampleState, "", "short"), summaryCutMarker) {
		t.Fatal("short summary got a cut marker")
	}
}
