// ABOUTME: Integration tests for prove.go against a tiny real git repo in a temp directory.
// ABOUTME: Proofs run as the current user; each case builds commits with Babysit-Proof trailers.

package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The repo's test_command runs tests/<selector>.sh; its lint_command fails
// while code.txt contains "bad".
const (
	testCommand = "sh proof.sh"
	lintCommand = "sh lint.sh"
)

// newProofRepo makes a repo whose base commit holds the proof scripts and
// code.txt, and returns its directory and base SHA.
func newProofRepo(t *testing.T) (string, string) {
	t.Helper()
	agentPrefix = nil
	for k, v := range map[string]string{
		"GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_NOSYSTEM": "1",
		"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@example.com",
		"GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@example.com",
	} {
		t.Setenv(k, v)
	}
	dir := t.TempDir()
	gitT(t, dir, "init", "-q", "-b", "main")
	return dir, commitT(t, dir, "base", map[string]string{
		"proof.sh": `exec sh "tests/$1.sh"` + "\n",
		"lint.sh":  "! grep -q bad code.txt\n",
		"code.txt": "bad\n",
	})
}

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// commitT writes files and commits them with message msg, returning the SHA.
func commitT(t *testing.T, dir, msg string, files map[string]string) string {
	t.Helper()
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitT(t, dir, "add", "-A")
	gitT(t, dir, "commit", "-q", "--allow-empty", "-m", msg)
	return gitT(t, dir, "rev-parse", "HEAD")
}

// addTest commits tests/<sel>.sh, which passes once code.txt holds want.
func addTest(t *testing.T, dir, sel, want string) string {
	return commitT(t, dir, "test "+sel, map[string]string{"tests/" + sel + ".sh": "grep -q " + want + " code.txt\n"})
}

func addFix(t *testing.T, dir, code, trailer string) string {
	return commitT(t, dir, "fix\n\nBabysit-Proof: "+trailer, map[string]string{"code.txt": code + "\n"})
}

func TestSelectorPattern(t *testing.T) {
	for _, s := range []string{"pkg/foo_test.go::TestBar[1]", "a-b.c:d", strings.Repeat("a", 200)} {
		if !selectorRE.MatchString(s) {
			t.Errorf("selector %q rejected, want accepted", s)
		}
	}
	for _, s := range []string{"", "a b", "a;b", "a$b", "a|b", "a`b`", "a\nb", "a\n", strings.Repeat("a", 201)} {
		if selectorRE.MatchString(s) {
			t.Errorf("selector %q accepted, want rejected", s)
		}
	}
}

func TestProveAcceptsTestThenFix(t *testing.T) {
	dir, base := newProofRepo(t)
	test := addTest(t, dir, "fixed", "fixed")
	fix := addFix(t, dir, "bad fixed", "test fixed")
	lint := addFix(t, dir, "fixed", "lint")

	out := filepath.Join(t.TempDir(), "report.json")
	for k, v := range map[string]string{"BABYSIT_REPO": dir, "BABYSIT_BASE": base, "BABYSIT_OUT": out,
		"BABYSIT_TEST_COMMAND": testCommand, "BABYSIT_LINT_COMMAND": lintCommand} {
		t.Setenv(k, v)
	}
	if code := cmdProve(nil); code != 0 {
		t.Fatalf("cmdProve = %d, want 0", code)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var report map[string]proofResult
	if err := json.Unmarshal(b, &report); err != nil {
		t.Fatal(err)
	}
	want := map[string]proofResult{
		test: {Precedes: fix},
		fix:  {Proof: "test fixed"},
		lint: {Proof: "lint"},
	}
	if len(report) != len(want) {
		t.Errorf("report = %s, want %d entries", b, len(want))
	}
	for sha, r := range want {
		if report[sha] != r {
			t.Errorf("report[%s] = %+v, want %+v", sha, report[sha], r)
		}
	}
}

func TestProveRejects(t *testing.T) {
	cases := map[string]struct {
		reason string // part of the error prove must return
		build  func(t *testing.T, dir string)
	}{
		"proof passes at parent": {"passes at", func(t *testing.T, dir string) {
			addTest(t, dir, "always", "bad")
			addFix(t, dir, "bad fixed", "test always")
		}},
		"proof fails at commit": {"fails at", func(t *testing.T, dir string) {
			addTest(t, dir, "fixed", "fixed")
			addFix(t, dir, "still bad", "test fixed")
		}},
		"unproved commit not followed by a proved one": {"no proof", func(t *testing.T, dir string) {
			addTest(t, dir, "fixed", "fixed")
			addFix(t, dir, "fixed", "test fixed")
			commitT(t, dir, "stray", map[string]string{"other.txt": "x\n"})
		}},
		"later fix breaks an earlier proof": {"fails at", func(t *testing.T, dir string) {
			addTest(t, dir, "fixed", "fixed")
			addFix(t, dir, "fixed", "test fixed")
			addTest(t, dir, "other", "other")
			addFix(t, dir, "other", "test other")
		}},
		"unknown proof kind": {"unusable", func(t *testing.T, dir string) {
			addTest(t, dir, "fixed", "fixed")
			addFix(t, dir, "fixed", "shell fixed")
		}},
		"selector with shell characters": {"unusable", func(t *testing.T, dir string) {
			addTest(t, dir, "fixed", "fixed")
			addFix(t, dir, "fixed", "test fixed;touch pwned")
		}},
		"two proof trailers": {"unusable", func(t *testing.T, dir string) {
			addTest(t, dir, "fixed", "fixed")
			addFix(t, dir, "fixed", "test fixed\nBabysit-Proof: lint")
		}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			dir, base := newProofRepo(t)
			c.build(t, dir)
			report, err := prove(dir, base, testCommand, lintCommand)
			if err == nil || !strings.Contains(err.Error(), c.reason) {
				t.Fatalf("prove = %+v, %v; want an error containing %q", report, err, c.reason)
			}
			if _, err := os.Stat(filepath.Join(dir, "pwned")); !errors.Is(err, os.ErrNotExist) {
				t.Error("a selector reached a shell")
			}
		})
	}
}

func TestProveRejectsProofKindWithoutCommand(t *testing.T) {
	dir, base := newProofRepo(t)
	addFix(t, dir, "good", "lint")
	if _, err := prove(dir, base, testCommand, ""); err == nil {
		t.Fatal("lint proof accepted without lint_command")
	}
}

func TestProveTimeoutKillsProcessGroup(t *testing.T) {
	dir, base := newProofRepo(t)
	pidFile := filepath.Join(t.TempDir(), "pid")
	addTest(t, dir, "hang", "fixed")
	commitT(t, dir, "fix\n\nBabysit-Proof: test hang", map[string]string{
		"code.txt":      "fixed\n",
		"tests/hang.sh": "sleep 300 & echo $! > " + pidFile + "\nwait\n",
	})
	defer func(d time.Duration) { proofTimeout = d }(proofTimeout)
	proofTimeout = time.Second

	start := time.Now()
	if _, err := prove(dir, base, testCommand, lintCommand); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("prove error = %v, want a timeout", err)
	}
	if d := time.Since(start); d > 20*time.Second {
		t.Errorf("prove took %v after the timeout", d)
	}
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	for i := 0; syscall.Kill(pid, 0) == nil; i++ {
		if i == 50 {
			t.Fatalf("grandchild %d still alive after the timeout", pid)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
