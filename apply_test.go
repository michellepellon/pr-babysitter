// ABOUTME: Integration tests for apply, with real git: a bare origin, a dev clone that builds bundles, and apply's clone.
// ABOUTME: Covers input validation, every bundle rejection, the lease push on reset and deleted branches, and dry runs.

package main

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const testBot = "babysit-bot <bot@example.invalid>"

// fixture holds a bare origin, a dev clone where the test plays the agent,
// and apply's own clone. base is the round head's parent.
type fixture struct {
	t                        *testing.T
	root, origin, dev, clone string
	base, head, branch       string
}

func runGit(t *testing.T, dir string, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %q: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newFixture(t *testing.T, branch string) *fixture {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, k := range []string{"GIT_AUTHOR", "GIT_COMMITTER"} {
		t.Setenv(k+"_NAME", "babysit-bot")
		t.Setenv(k+"_EMAIL", "bot@example.invalid")
	}
	root := t.TempDir()
	f := &fixture{t: t, root: root, origin: filepath.Join(root, "origin.git"),
		dev: filepath.Join(root, "dev"), clone: filepath.Join(root, "clone"), branch: branch}
	runGit(t, root, nil, "init", "-q", "--bare", "-b", "main", f.origin)
	runGit(t, root, nil, "clone", "-q", f.origin, f.dev)
	f.base = f.commit("base", map[string]string{"a.go": "package a\n"})
	f.head = f.commit("round head", map[string]string{"a.go": "package a\n\nvar X = 1\n"})
	f.git("push", "-q", "origin", "HEAD:refs/heads/main", "HEAD:refs/heads/"+branch)
	runGit(t, root, nil, "clone", "-q", f.origin, f.clone)
	return f
}

func (f *fixture) git(args ...string) string { f.t.Helper(); return runGit(f.t, f.dev, nil, args...) }

// commit writes files in dev, commits everything, and returns the new SHA.
func (f *fixture) commit(msg string, files map[string]string) string {
	f.t.Helper()
	for name, body := range files {
		p := filepath.Join(f.dev, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			f.t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			f.t.Fatal(err)
		}
	}
	f.git("add", "-A")
	f.git("commit", "-q", "--allow-empty", "-m", msg)
	return f.git("rev-parse", "HEAD")
}

// bundle builds a bundle from dev, of head..HEAD unless revs are given.
func (f *fixture) bundle(revs ...string) string {
	f.t.Helper()
	if len(revs) == 0 {
		revs = []string{f.head + "..HEAD"}
	}
	p := filepath.Join(f.root, "work.bundle")
	f.git(append([]string{"bundle", "create", "-q", p}, revs...)...)
	return p
}

// proofs writes a proof report keyed by the given commits, or by head..HEAD.
func (f *fixture) proofs(commits ...string) string {
	f.t.Helper()
	if commits == nil {
		commits = strings.Fields(f.git("rev-list", f.head+"..HEAD"))
	}
	report := map[string]any{}
	for _, c := range commits {
		report[c] = map[string]string{"result": "proved"}
	}
	data, _ := json.Marshal(report)
	p := filepath.Join(f.root, "proofs.json")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		f.t.Fatal(err)
	}
	return p
}

func (f *fixture) input() applyInput {
	return applyInput{dir: f.clone, pr: "7", head: f.head, branch: f.branch,
		bundle: f.bundle(), proofs: f.proofs(), bot: testBot,
		protected: append(slices.Clone(builtinProtected), "secrets/"),
		rereadPR:  func(pr, head string) error { return nil }}
}

func (f *fixture) originBranch() string {
	return runGit(f.t, f.root, nil, "--git-dir", f.origin, "for-each-ref", "--format=%(objectname)", "refs/heads/"+f.branch)
}

func (f *fixture) testAndFix() string {
	f.commit("test: cover X", map[string]string{"a_test.go": "package a\n"})
	return f.commit("fix: X\n\nBabysit-Proof: test TestX", map[string]string{"a.go": "package a\n\nvar X = 2\n"})
}

func TestApplyRejectsBadInputs(t *testing.T) {
	f := newFixture(t, "feat")
	f.testAndFix()
	for _, mod := range []func(*applyInput){
		func(in *applyInput) { in.pr = "7a" },
		func(in *applyInput) { in.pr = "" },
		func(in *applyInput) { in.pr = "-7" },
		func(in *applyInput) { in.head = in.head[:39] },
		func(in *applyInput) { in.head = strings.ToUpper(in.head) },
		func(in *applyInput) { in.branch = "a..b" },
		func(in *applyInput) { in.branch = "feat/$(touch pwned)" },
		func(in *applyInput) { in.branch = "@{-1}" },
		func(in *applyInput) { in.branch = "" },
	} {
		in := f.input()
		mod(&in)
		if _, err := apply(in); err == nil || !strings.HasPrefix(err.Error(), "invalid input") {
			t.Errorf("apply(pr=%q head=%q branch=%q) = %v, want invalid input", in.pr, in.head, in.branch, err)
		}
	}
}

func TestApplyPushesWithoutShell(t *testing.T) {
	// check-ref-format forbids spaces, so ${IFS} stands in for one.
	f := newFixture(t, "feat/$(touch${IFS}pwned)")
	tip := f.testAndFix()
	t.Chdir(f.root)
	out, err := apply(f.input())
	if err != nil || out != "pushed 2 commits" {
		t.Fatalf("apply = %q, %v; want pushed 2 commits", out, err)
	}
	if got := f.originBranch(); got != tip {
		t.Errorf("origin branch at %s, want %s", got, tip)
	}
	for _, dir := range []string{f.root, f.dev, f.clone} {
		if _, err := os.Stat(filepath.Join(dir, "pwned")); err == nil {
			t.Errorf("a shell ran: %s/pwned exists", dir)
		}
	}
}

func TestApplyRejectsBundles(t *testing.T) {
	big := make([]byte, 11<<20)
	rand.Read(big)
	lines := func(n int) string { return strings.Repeat("x\n", n) }
	cases := []struct {
		name, want string
		file, msg  string // a file to add, or a commit message, before setup
		setup      func(f *fixture, in *applyInput)
	}{
		{"two refs", "exactly one ref", "", "", func(f *fixture, in *applyInput) {
			f.git("branch", "extra")
			in.bundle = f.bundle(f.head+"..HEAD", "extra")
		}},
		{"tip not from round head", "descend", "", "", func(f *fixture, in *applyInput) {
			f.git("checkout", "-q", "--detach", f.base)
			f.commit("elsewhere", map[string]string{"b.go": "package a\n"})
			in.bundle, in.proofs = f.bundle(), f.proofs()
		}},
		{"merge commit", "merge commit", "", "", func(f *fixture, in *applyInput) {
			f.git("checkout", "-q", "-b", "side")
			f.commit("side", map[string]string{"b.go": "package a\n"})
			f.git("checkout", "-q", "-")
			f.git("merge", "-q", "--no-ff", "-m", "merge side", "side")
			in.bundle, in.proofs = f.bundle(), f.proofs()
		}},
		{"other author", "authored and committed", "", "", func(f *fixture, in *applyInput) {
			f.git("commit", "-q", "--amend", "--no-edit", "--author", "Eve <eve@example.invalid>")
			in.bundle, in.proofs = f.bundle(), f.proofs()
		}},
		{"other committer", "authored and committed", "", "", func(f *fixture, in *applyInput) {
			runGit(f.t, f.dev, []string{"GIT_COMMITTER_NAME=Eve"}, "commit", "-q", "--amend", "--no-edit")
			in.bundle, in.proofs = f.bundle(), f.proofs()
		}},
		{"Fixes #12", "closing keyword", "", "Fixes #12", nil},
		{"closes #3", "closing keyword", "", "closes #3", nil},
		{"also resolves #9 here", "closing keyword", "", "also resolves #9 here", nil},
		{"deleted file", "deletes", "", "", func(f *fixture, in *applyInput) {
			f.git("rm", "-q", "a_test.go")
			f.commit("drop test", nil)
			in.bundle, in.proofs = f.bundle(), f.proofs()
		}},
		{"renamed file", "deletes", "", "", func(f *fixture, in *applyInput) {
			f.git("mv", "a.go", "b.go")
			f.commit("rename", nil)
			in.bundle, in.proofs = f.bundle(), f.proofs()
		}},
		{".github/", "protected", ".github/workflows/ci.yml", "", nil},
		{"pkg/.claude/settings.json", "protected", "pkg/.claude/settings.json", "", nil},
		{"docs/CODEOWNERS", "protected", "docs/CODEOWNERS", "", nil},
		{".gitattributes", "protected", ".gitattributes", "", nil},
		{"secrets/key.txt", "protected", "secrets/key.txt", "", nil},
		{"21 commits", "allowed 1 to 20", "", "", func(f *fixture, in *applyInput) {
			for i := range 19 {
				f.commit("more", map[string]string{"m" + string(rune('a'+i)) + ".go": "x\n"})
			}
			in.bundle, in.proofs = f.bundle(), f.proofs()
		}},
		{"1,001 lines in total", "changed lines", "", "", func(f *fixture, in *applyInput) {
			f.commit("many", map[string]string{"x1": lines(334), "x2": lines(334), "x3": lines(331)})
			in.bundle, in.proofs = f.bundle(), f.proofs()
		}},
		{"401 lines in one file", "changed lines", "", "", func(f *fixture, in *applyInput) {
			f.commit("one", map[string]string{"x1": lines(200)})
			f.commit("two", map[string]string{"x1": lines(401)})
			in.bundle, in.proofs = f.bundle(), f.proofs()
		}},
		{"bundle over 10 MB", "10 MB", "", "", func(f *fixture, in *applyInput) {
			f.commit("big", map[string]string{"big.bin": string(big)})
			in.bundle, in.proofs = f.bundle(), f.proofs()
		}},
		{"proof report misses a commit", "proof report", "", "", func(f *fixture, in *applyInput) {
			in.proofs = f.proofs(f.git("rev-parse", "HEAD"))
		}},
		{"proof report has an extra commit", "proof report", "", "", func(f *fixture, in *applyInput) {
			in.proofs = f.proofs(append(strings.Fields(f.git("rev-list", f.head+"..HEAD")), f.base)...)
		}},
		{"proof report is not JSON", "proof report", "", "", func(f *fixture, in *applyInput) {
			os.WriteFile(in.proofs, []byte("nope"), 0o644)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, "feat")
			tip := f.testAndFix()
			if c.file != "" {
				f.commit("edit", map[string]string{c.file: "x\n"})
			}
			if c.msg != "" {
				f.commit(c.msg, map[string]string{"c.go": "package a\n"})
			}
			in := f.input()
			if c.setup != nil {
				c.setup(f, &in)
			}
			_, err := apply(in)
			if err == nil || !strings.HasPrefix(err.Error(), "rejected: ") || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("apply = %v, want rejected: ...%s...", err, c.want)
			}
			if got := f.originBranch(); got != f.head {
				t.Errorf("origin branch moved to %s (tip %s)", got, tip)
			}
		})
	}
}

func TestApplyLeaseRejectsResetAndDeletedBranches(t *testing.T) {
	for _, c := range []struct{ name, refspec, want string }{
		{"reset", "+%s:refs/heads/feat", "base"},
		{"deleted", ":refs/heads/feat", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, "feat")
			f.testAndFix()
			in := f.input()
			f.git("push", "-q", "origin", strings.Replace(c.refspec, "%s", f.base, 1))
			_, err := apply(in)
			if err == nil || !strings.HasPrefix(err.Error(), "push failed: ") {
				t.Fatalf("apply = %v, want push failed", err)
			}
			t.Log(err)
			want := map[string]string{"base": f.base}[c.want]
			if got := f.originBranch(); got != want {
				t.Errorf("origin branch at %q, want %q", got, want)
			}
		})
	}
}

func TestApplyDryRunPushesNothing(t *testing.T) {
	f := newFixture(t, "feat")
	f.testAndFix()
	in := f.input()
	in.dryRun = true
	out, err := apply(in)
	if err != nil || out != "dry-run: would push 2 commits" {
		t.Fatalf("apply = %q, %v; want dry-run: would push 2 commits", out, err)
	}
	if got := f.originBranch(); got != f.head {
		t.Errorf("origin branch moved to %s", got)
	}
}

func TestApplyStopsOnStalePR(t *testing.T) {
	f := newFixture(t, "feat")
	f.testAndFix()
	in := f.input()
	var gotPR, gotHead string
	in.rereadPR = func(pr, head string) error { gotPR, gotHead = pr, head; return errors.New("label removed") }
	if _, err := apply(in); err == nil || err.Error() != "stale: label removed" {
		t.Fatalf("apply = %v, want stale: label removed", err)
	}
	if gotPR != "7" || gotHead != f.head {
		t.Errorf("rereadPR(%q, %q), want (7, %s)", gotPR, gotHead, f.head)
	}
	if got := f.originBranch(); got != f.head {
		t.Errorf("origin branch moved to %s", got)
	}
}

// setApplyEnv sets cmdApply's inputs from f.input() and stubs the PR re-read.
func (f *fixture) setApplyEnv() {
	in := f.input()
	for k, v := range map[string]string{"BABYSIT_DIR": in.dir, "BABYSIT_PR": in.pr, "BABYSIT_HEAD": in.head,
		"BABYSIT_BRANCH": in.branch, "BABYSIT_BUNDLE": in.bundle, "BABYSIT_PROOFS": in.proofs, "BABYSIT_BOT": in.bot} {
		f.t.Setenv(k, v)
	}
	old := rereadPR
	f.t.Cleanup(func() { rereadPR = old })
	rereadPR = func(pr, head string) error { return nil }
}

func TestCmdApplyDryRunUnlessTurnedOff(t *testing.T) {
	for _, v := range []string{"unset", "", "true", "no", "0", "False"} {
		t.Run(v, func(t *testing.T) {
			f := newFixture(t, "feat")
			f.testAndFix()
			f.setApplyEnv()
			t.Setenv("BABYSIT_DRY_RUN", v)
			if v == "unset" {
				os.Unsetenv("BABYSIT_DRY_RUN")
			}
			if code := cmdApply(nil); code != 0 {
				t.Fatalf("cmdApply = %d, want 0", code)
			}
			if got := f.originBranch(); got != f.head {
				t.Errorf("BABYSIT_DRY_RUN=%q pushed: origin branch at %s", v, got)
			}
		})
	}
	f := newFixture(t, "feat")
	tip := f.testAndFix()
	f.setApplyEnv()
	t.Setenv("BABYSIT_DRY_RUN", "false")
	if code := cmdApply(nil); code != 0 || f.originBranch() != tip {
		t.Errorf("BABYSIT_DRY_RUN=false: cmdApply = %d, origin at %s; want 0 and %s", code, f.originBranch(), tip)
	}
}

func TestCmdApplyValidatesInputs(t *testing.T) {
	f := newFixture(t, "feat")
	f.testAndFix()
	f.setApplyEnv()
	t.Setenv("BABYSIT_DRY_RUN", "false")
	t.Setenv("BABYSIT_PROTECTED_PATHS", "a_test.go")
	if code := cmdApply(nil); code != 1 || f.originBranch() != f.head {
		t.Errorf("protected path from BABYSIT_PROTECTED_PATHS: cmdApply = %d, want 1 and no push", code)
	}
	t.Setenv("BABYSIT_PROTECTED_PATHS", "")
	t.Setenv("BABYSIT_PR", "7;x")
	if code := cmdApply(nil); code != 1 || f.originBranch() != f.head {
		t.Errorf("bad PR number: cmdApply = %d, want 1 and no push", code)
	}
	t.Setenv("BABYSIT_PR", "")
	if code := cmdApply(nil); code != 2 {
		t.Errorf("missing PR number: cmdApply = %d, want 2", code)
	}
}
