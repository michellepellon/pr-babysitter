// ABOUTME: Status comment: the babysit-state line, the status, and the fenced round summary.
// ABOUTME: plan reads only line 1, and only from comments by github-actions[bot].

package main

import "time"

// State is the decoded state line, line 1 of the status comment (spec §5).
type State struct {
	V           int       `json:"v"`
	LabelEvent  string    `json:"label_event"` // node ID of the labeled event that started this run
	Owner       string    `json:"owner"`       // login of the writer who added the label
	Rounds      int       `json:"rounds"`
	RoundHead   string    `json:"round_head"`
	Outcome     string    `json:"outcome"` // "running" while a round is in flight
	LastPushSHA string    `json:"last_push_sha"`
	LastPushAt  time.Time `json:"last_push_at"`
	HeadSeenSHA string    `json:"head_seen_sha"`
	HeadSeenAt  time.Time `json:"head_seen_at"`
}
