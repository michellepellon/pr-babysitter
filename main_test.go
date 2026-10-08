// ABOUTME: Tests for subcommand dispatch and environment inputs in main.go.
// ABOUTME: A missing or unknown subcommand, or a missing required input, must print usage and exit 2.

package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// stderrOf runs fn with os.Stderr sent to a temp file and returns what fn wrote there.
func stderrOf(t *testing.T, fn func()) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	defer func(old *os.File) { os.Stderr = old }(os.Stderr)
	os.Stderr = f
	fn()
	b, _ := os.ReadFile(f.Name())
	return string(b)
}

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

func TestEnvInputsReportsEveryMissingName(t *testing.T) {
	t.Setenv("BABYSIT_A", "a")
	t.Setenv("BABYSIT_B", "")
	t.Setenv("BABYSIT_C", "")
	t.Setenv("BABYSIT_D", "")
	required := []string{"BABYSIT_A", "BABYSIT_B", "BABYSIT_C"}
	var ok bool
	msg := stderrOf(t, func() { _, ok = envInputs("x", nil, required, "BABYSIT_D") })
	if ok {
		t.Fatal("envInputs accepted missing inputs")
	}
	for _, want := range []string{"missing BABYSIT_B, BABYSIT_C\n", "usage: pr-babysitter x"} {
		if !strings.Contains(msg, want) {
			t.Errorf("envInputs printed %q, want it to contain %q", msg, want)
		}
	}

	t.Setenv("BABYSIT_B", "b")
	t.Setenv("BABYSIT_C", "c")
	env, ok := envInputs("x", nil, required, "BABYSIT_D")
	if !ok || env["BABYSIT_A"] != "a" || env["BABYSIT_C"] != "c" || env["BABYSIT_D"] != "" {
		t.Errorf("envInputs = %v, %v; want A, B, C set and optional D empty", env, ok)
	}
	if msg := stderrOf(t, func() { _, ok = envInputs("x", []string{"-pr", "7"}, required) }); ok || !strings.Contains(msg, "usage: ") {
		t.Errorf("envInputs with arguments = %v, printed %q; want usage", ok, msg)
	}
}

func TestRunDispatchesSubcommands(t *testing.T) {
	for _, k := range []string{"BABYSIT_OUT", "BABYSIT_BOT", "BABYSIT_GATEWAY"} {
		t.Setenv(k, "")
	}
	for _, name := range []string{"prove", "apply", "gateway"} {
		var top bytes.Buffer
		var code int
		msg := stderrOf(t, func() { code = run([]string{name}, &top) })
		if code != 2 || top.Len() > 0 || !strings.Contains(msg, "usage: pr-babysitter "+name+",") {
			t.Errorf("run(%q) = %d, printed %q and %q; want 2 and %s's own usage", name, code, top.String(), msg, name)
		}
	}
}
