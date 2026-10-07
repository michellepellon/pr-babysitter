// ABOUTME: Tests for subcommand dispatch in main.go.
// ABOUTME: A missing or unknown subcommand must print usage and exit 2.

package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunRejectsMissingOrUnknownSubcommand(t *testing.T) {
	for _, args := range [][]string{nil, {"bogus"}} {
		var stderr bytes.Buffer
		if code := run(args, &stderr); code != 2 {
			t.Errorf("run(%q) = %d, want 2", args, code)
		}
		if !strings.HasPrefix(stderr.String(), "usage: ") {
			t.Errorf("run(%q) printed %q, want usage", args, stderr.String())
		}
	}
}
