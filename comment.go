// ABOUTME: Status comment: the babysit-state line, the status, and the fenced round summary.
// ABOUTME: plan reads only line 1, and only from comments by github-actions[bot].

package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// State is the decoded state line, line 1 of the status comment (spec §5).
type State struct {
	V            int       `json:"v"`
	LabelEvent   string    `json:"label_event"` // node ID of the labeled event that started this run
	Owner        string    `json:"owner"`       // login of the writer who added the label
	Rounds       int       `json:"rounds"`
	RoundHead    string    `json:"round_head"`
	Outcome      string    `json:"outcome"`    // "running" while a round is in flight
	OutcomeAt    time.Time `json:"outcome_at"` // when plan or apply last set Outcome
	LastPushSHA  string    `json:"last_push_sha"`
	LastPushAt   time.Time `json:"last_push_at"`
	HeadSeenSHA  string    `json:"head_seen_sha"`
	WaitingSince time.Time `json:"waiting_since"` // when rule 6's current wait began; zero when not waiting
}

const (
	botUserID        = 41898282 // github-actions[bot]; logins differ between REST and GraphQL, IDs don't
	maxSummaryBytes  = 20 << 10
	summaryCutMarker = "_Summary cut at 20 KB._"
	stateLinePrefix  = "<!-- babysit-state "
	stateLineSuffix  = " -->"
)

// renderComment builds the comment body. status is ours and renders as markdown, so callers
// must keep untrusted text out of it. summary is agent text and always goes inside a fence.
func renderComment(s State, status, summary string) string {
	// json.Marshal escapes <, >, & and newlines, so no field can end the HTML comment or
	// line 1. It fails only on years past 9999; parseState then finds no state.
	line, _ := json.Marshal(s)
	cut := ""
	if len(summary) > maxSummaryBytes {
		// ToValidUTF8 drops the partial rune a byte cut can leave at the end.
		summary, cut = strings.ToValidUTF8(summary[:maxSummaryBytes], ""), "\n"+summaryCutMarker+"\n"
	}
	f := fence(summary)
	return fmt.Sprintf("%s%s%s\n%s\n\n%s\n%s\n%s\n%s", stateLinePrefix, line, stateLineSuffix, status, f, summary, f, cut)
}

// parseState decodes line 1 of a comment, and only of one written by github-actions[bot].
func parseState(authorID int64, body string) (State, bool) {
	line, _, _ := strings.Cut(body, "\n")
	j, okPrefix := strings.CutPrefix(strings.TrimSuffix(line, "\r"), stateLinePrefix)
	j, okSuffix := strings.CutSuffix(j, stateLineSuffix)
	var s State
	if authorID != botUserID || !okPrefix || !okSuffix || json.Unmarshal([]byte(j), &s) != nil {
		return State{}, false
	}
	return s, true
}

// fence returns a backtick fence longer than any backtick run in text. Under CommonMark §4.5
// (which GFM follows) only a line of at least that many backticks closes the block, and
// nothing inside a code block renders as a link, image, HTML, or @mention.
func fence(text string) string {
	longest, run := 0, 0
	for _, r := range text {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	return strings.Repeat("`", max(3, longest+1))
}
