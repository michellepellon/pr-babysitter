// ABOUTME: The apply subcommand: validates plan's inputs, checks work's bundle and proof report, and pushes with a lease.
// ABOUTME: The bundle and everything in it are attacker-controlled; every git call is an argument list, never a shell.

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

type applyInput struct {
	dir, pr, head, branch, bundle, proofs string
	bot                                   string // "name <email>" that must author and commit every bundle commit
	protected                             []string
	dryRun                                bool
	rereadPR                              func(pr, head string) error
}

// builtinProtected is the built-in protected-path list from spec §8. An entry
// ending in "/" protects that directory; any other protects that file name.
var builtinProtected = []string{".github/", ".devcontainer/", ".claude/", ".gitattributes", ".gitmodules",
	"CODEOWNERS", "CLAUDE.md", "AGENTS.md", ".roborev.toml", "REVIEW.md"}

// rereadPR must confirm the PR is still open, not from a fork, labeled by a
// writer, and on head. github.go supplies it; until then apply fails closed.
var rereadPR = func(pr, head string) error { return errors.New("PR re-read is not wired to GitHub") }

var (
	prNumberRE = regexp.MustCompile(`^[1-9][0-9]{0,9}$`)
	shaRE      = regexp.MustCompile(`^[0-9a-f]{40}$`)
	// closingRE matches GitHub's issue-closing keywords, by number or URL.
	closingRE = regexp.MustCompile(`(?i)\b(close[sd]?|fix(e[sd])?|resolve[sd]?)\b[\s:]+\S*(#|issues/|pull/)\d+`)
)

func cmdApply(args []string) int {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	in := applyInput{rereadPR: rereadPR}
	fs.StringVar(&in.dir, "C", ".", "clone whose origin is the PR's repo")
	fs.StringVar(&in.pr, "pr", "", "PR number")
	fs.StringVar(&in.head, "head", "", "the round's head SHA")
	fs.StringVar(&in.branch, "branch", "", "the PR's head branch")
	fs.StringVar(&in.bundle, "bundle", "", "work's bundle")
	fs.StringVar(&in.proofs, "proofs", "", "work's proof report")
	fs.StringVar(&in.bot, "bot", "", `bot identity, "name <email>"`)
	extra := fs.String("protected-paths", "", "whitespace-separated paths added to the built-in list")
	fs.BoolVar(&in.dryRun, "dry-run", true, "check everything but push nothing")
	if fs.Parse(args) != nil {
		return 2
	}
	in.protected = slices.Concat(builtinProtected, strings.Fields(*extra))
	out, err := apply(in)
	if err != nil {
		fmt.Println(err)
		return 1
	}
	fmt.Println(out)
	return 0
}

func git(dir string, args ...string) (string, error) {
	var stderr bytes.Buffer
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

// apply returns the outcome to record, or an error whose text is the outcome.
func apply(in applyInput) (string, error) {
	if !prNumberRE.MatchString(in.pr) || !shaRE.MatchString(in.head) {
		return "", errors.New("invalid input: PR number or head SHA")
	}
	if _, err := git(in.dir, "check-ref-format", "refs/heads/"+in.branch); err != nil {
		return "", errors.New("invalid input: branch name")
	}
	if err := in.rereadPR(in.pr, in.head); err != nil {
		return "", fmt.Errorf("stale: %v", err)
	}
	tip, n, err := checkBundle(in)
	if err != nil {
		return "", fmt.Errorf("rejected: %v", err)
	}
	if in.dryRun {
		return fmt.Sprintf("dry-run: would push %d commits", n), nil
	}
	// The lease fails if a person reset, moved, or deleted the branch.
	if _, err := git(in.dir, "push", "--force-with-lease=refs/heads/"+in.branch+":"+in.head,
		"origin", tip+":refs/heads/"+in.branch); err != nil {
		return "", fmt.Errorf("push failed: %v", err)
	}
	return fmt.Sprintf("pushed %d commits", n), nil
}

// checkBundle runs spec §4's bundle checks and returns the tip and commit count.
func checkBundle(in applyInput) (string, int, error) {
	if fi, err := os.Stat(in.bundle); err != nil || fi.Size() > 10<<20 {
		return "", 0, errors.New("bundle missing or over 10 MB")
	}
	if _, err := git(in.dir, "fetch", "-q", "--no-tags", "origin", in.head); err != nil {
		return "", 0, err
	}
	heads, err := git(in.dir, "bundle", "list-heads", in.bundle)
	f := strings.Fields(heads)
	if err != nil || len(f) != 2 || !shaRE.MatchString(f[0]) {
		return "", 0, errors.New("bundle must hold exactly one ref")
	}
	tip, span := f[0], in.head+".."+f[0]
	if _, err := git(in.dir, "-c", "transfer.fsckObjects=true", "fetch", "-q", "--no-tags", in.bundle, tip); err != nil {
		return "", 0, err
	}
	if _, err := git(in.dir, "merge-base", "--is-ancestor", in.head, tip); err != nil {
		return "", 0, errors.New("tip does not descend from the round head")
	}
	out, err := git(in.dir, "rev-list", span)
	commits := strings.Fields(out)
	if err != nil || len(commits) == 0 || len(commits) > 20 {
		return "", 0, fmt.Errorf("%d commits; allowed 1 to 20", len(commits))
	}
	if merges, _ := git(in.dir, "rev-list", "--min-parents=2", "--count", span); merges != "0\n" {
		return "", 0, errors.New("merge commit")
	}
	total, perFile := 0, map[string]int{}
	for _, c := range commits {
		out, err := git(in.dir, "show", "-s", "--format=%an <%ae>%x00%cn <%ce>%x00%B", c)
		meta := strings.SplitN(out, "\x00", 3)
		if err != nil || len(meta) != 3 || meta[0] != in.bot || meta[1] != in.bot {
			return "", 0, fmt.Errorf("commit %s not authored and committed by %s", c, in.bot)
		}
		if closingRE.MatchString(meta[2]) {
			return "", 0, fmt.Errorf("commit %s has an issue-closing keyword", c)
		}
		// --no-renames makes a rename show as a deletion plus an addition.
		st, err := git(in.dir, "diff-tree", "-r", "--no-renames", "--no-commit-id", "-z", "--name-status", c)
		if err != nil {
			return "", 0, err
		}
		fields := strings.Split(st, "\x00")
		for i := 0; i+1 < len(fields); i += 2 {
			if fields[i] != "M" && fields[i] != "A" && fields[i] != "T" {
				return "", 0, fmt.Errorf("commit %s deletes %q (status %s)", c, fields[i+1], fields[i])
			}
			if protected(fields[i+1], in.protected) {
				return "", 0, fmt.Errorf("commit %s changes protected path %q", c, fields[i+1])
			}
		}
		ns, err := git(in.dir, "diff-tree", "-r", "--no-renames", "--no-commit-id", "-z", "--numstat", c)
		if err != nil {
			return "", 0, err
		}
		for _, rec := range strings.Split(strings.TrimSuffix(ns, "\x00"), "\x00") {
			f := strings.SplitN(rec, "\t", 3)
			if rec == "" {
				continue
			}
			a, errA := strconv.Atoi(f[0]) // a binary file shows "-", which Atoi rejects
			d, errD := strconv.Atoi(f[1%len(f)])
			if len(f) != 3 || errA != nil || errD != nil {
				return "", 0, fmt.Errorf("commit %s changes binary file %q", c, rec)
			}
			total, perFile[f[2]] = total+a+d, perFile[f[2]]+a+d
			if total > 1000 || perFile[f[2]] > 400 {
				return "", 0, fmt.Errorf("over 1,000 changed lines in total or 400 in %q", f[2])
			}
		}
	}
	data, err := os.ReadFile(in.proofs)
	var report map[string]json.RawMessage
	if err != nil || json.Unmarshal(data, &report) != nil || len(report) != len(commits) ||
		slices.ContainsFunc(commits, func(c string) bool { _, ok := report[c]; return !ok }) {
		return "", 0, errors.New("proof report does not cover exactly the bundle's commits")
	}
	return tip, len(commits), nil
}

// protected reports whether path falls under a protected entry, at any depth.
func protected(path string, entries []string) bool {
	p := "/" + strings.ToLower(path)
	return slices.ContainsFunc(entries, func(e string) bool {
		e = "/" + strings.ToLower(e)
		return strings.HasSuffix(e, "/") && strings.Contains(p+"/", e) || strings.HasSuffix(p, e)
	})
}
