// ABOUTME: The prove subcommand: checks each agent commit's Babysit-Proof trailer by running its proof as agent.
// ABOUTME: Writes the JSON proof report only when every proof holds; any failure means no push.

package main

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"
)

// agentPrefix runs a command as the agent user with an empty environment
// except HOME and PATH. Tests set it to nil to run as the current user.
var agentPrefix = []string{"sudo", "-u", "agent", "env", "-i", "HOME=/home/agent", "PATH=" + os.Getenv("PATH")}

var proofTimeout, proofsTimeout = 5 * time.Minute, 10 * time.Minute

var selectorRE = regexp.MustCompile(`^[A-Za-z0-9_./:\[\]-]{1,200}$`)

// proofResult is one value in the proof report, a JSON object with one key per
// commit after the round head, each a full 40-hex SHA. apply reads only the
// keys. A proved commit names its Proof (the trailer value); any other commit
// names the proved commit it Precedes.
type proofResult struct {
	Proof    string `json:"proof,omitempty"`
	Precedes string `json:"precedes,omitempty"`
}

// cmdProve's inputs: BABYSIT_REPO is the agent's clone, checked out at the
// bot's last commit; BABYSIT_BASE the round head; BABYSIT_OUT where to write
// the proof report. A test proof appends its selector to BABYSIT_TEST_COMMAND.
func cmdProve(args []string) int {
	env, ok := envInputs("prove", args, []string{"BABYSIT_REPO", "BABYSIT_BASE", "BABYSIT_OUT"},
		"BABYSIT_TEST_COMMAND", "BABYSIT_LINT_COMMAND")
	if !ok {
		return 2
	}
	report, err := prove(env["BABYSIT_REPO"], env["BABYSIT_BASE"], env["BABYSIT_TEST_COMMAND"], env["BABYSIT_LINT_COMMAND"])
	if err == nil {
		b, _ := json.MarshalIndent(report, "", "  ")
		err = os.WriteFile(env["BABYSIT_OUT"], b, 0o644)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "prove:", err)
		return 1
	}
	return 0
}

func prove(repo, base, testCmd, lintCmd string) (map[string]proofResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), proofsTimeout)
	defer cancel()
	git := func(args ...string) (string, error) { return runAs(ctx, repo, append([]string{"git"}, args...)) }
	head, err := git("rev-parse", "HEAD")
	list, err2 := git("rev-list", "--reverse", base+".."+head)
	if err = cmp.Or(err, err2); err != nil {
		return nil, err
	}
	commits := strings.Fields(list)
	trailers, argvs := make([]string, len(commits)), make([][]string, len(commits))
	for i, c := range commits {
		if trailers[i], err = git("log", "-1", "--format=%(trailers:key=Babysit-Proof,valueonly,unfold)", c); err != nil {
			return nil, err
		}
		if argvs[i], err = proofArgv(trailers[i], testCmd, lintCmd); err != nil {
			return nil, fmt.Errorf("%s: %w", c, err)
		}
	}
	// check checks out rev and requires argv to pass there, or to fail.
	check := func(rev string, argv []string, wantPass bool) error {
		if _, err := git("checkout", "-q", "--force", "--detach", rev); err != nil {
			return err
		}
		pctx, cancel := context.WithTimeout(ctx, proofTimeout)
		defer cancel()
		out, err := runAs(pctx, repo, argv)
		fmt.Fprintln(os.Stderr, out)
		if pctx.Err() != nil {
			return fmt.Errorf("%q timed out at %s", argv, rev)
		}
		if (err == nil) != wantPass {
			return fmt.Errorf("%q %s at %s", argv, map[bool]string{true: "fails", false: "passes"}[wantPass], rev)
		}
		return nil
	}
	report := map[string]proofResult{}
	for i, c := range commits {
		if argvs[i] != nil {
			report[c] = proofResult{Proof: trailers[i]}
			err = cmp.Or(check(c+"^", argvs[i], false), check(c, argvs[i], true))
		} else if i+1 < len(commits) && argvs[i+1] != nil {
			report[c] = proofResult{Precedes: commits[i+1]}
		} else {
			err = fmt.Errorf("%s has no proof and isn't followed by a proved commit", c)
		}
		if err != nil {
			return nil, err
		}
	}
	for _, argv := range argvs {
		if argv != nil {
			if err := check(head, argv, true); err != nil {
				return nil, err
			}
		}
	}
	return report, nil
}

// proofArgv turns a Babysit-Proof trailer value into the command that proves
// it, or nil for no trailer. Commands split on whitespace and never see a shell.
func proofArgv(trailer, testCmd, lintCmd string) ([]string, error) {
	sel, isTest := strings.CutPrefix(trailer, "test ")
	switch test, lint := strings.Fields(testCmd), strings.Fields(lintCmd); {
	case trailer == "":
		return nil, nil
	case trailer == "lint" && len(lint) > 0:
		return lint, nil
	case isTest && len(test) > 0 && selectorRE.MatchString(sel):
		return append(test, sel), nil
	}
	return nil, fmt.Errorf("unusable Babysit-Proof trailer %q", trailer)
}

// runAs runs argv in dir as agent, with no shell, and returns its trimmed
// stdout. When ctx ends it kills the command's whole process group, as agent,
// because the runner user can't signal agent's processes.
func runAs(ctx context.Context, dir string, argv []string) (string, error) {
	full := append(slices.Clip(agentPrefix), argv...)
	cmd := exec.CommandContext(ctx, full[0], full[1:]...)
	cmd.Dir, cmd.Stderr = dir, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		kill := append(slices.Clip(agentPrefix), "kill", "-KILL", "--", fmt.Sprint(-cmd.Process.Pid))
		return exec.Command(kill[0], kill[1:]...).Run()
	}
	cmd.WaitDelay = 5 * time.Second // a process that left the group can't hold stdout open
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}
