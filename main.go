// ABOUTME: Entry point for pr-babysitter: dispatches to the plan, prove, apply, and gateway subcommands.
// ABOUTME: Subcommands read their inputs from the environment; a missing input or subcommand prints usage and exits 2.

package main

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// commands maps each subcommand name to its handler, which gets the remaining
// arguments and returns the exit code.
var commands = map[string]func(args []string) int{}

func run(args []string, stderr io.Writer) int {
	if len(args) > 0 {
		if cmd, ok := commands[args[0]]; ok {
			return cmd(args[1:])
		}
	}
	fmt.Fprintln(stderr, "usage: pr-babysitter <plan|prove|apply|gateway> [args]")
	return 2
}

// envInputs reads cmd's inputs from the environment, which is how the workflow
// passes them. If cmd got arguments or any required input is empty, it prints
// every missing name and cmd's usage, and returns false.
func envInputs(cmd string, args, required []string, optional ...string) (map[string]string, bool) {
	env, missing := map[string]string{}, []string{}
	for _, k := range required {
		if env[k] = os.Getenv(k); env[k] == "" {
			missing = append(missing, k)
		}
	}
	for _, k := range optional {
		env[k] = os.Getenv(k)
	}
	if len(missing) > 0 || len(args) > 0 {
		if len(missing) > 0 {
			fmt.Fprintf(os.Stderr, "missing %s\n", strings.Join(missing, ", "))
		}
		fmt.Fprintf(os.Stderr, "usage: pr-babysitter %s, with %s set in the environment", cmd, strings.Join(required, ", "))
		if len(optional) > 0 {
			fmt.Fprintf(os.Stderr, " and optionally %s", strings.Join(optional, ", "))
		}
		fmt.Fprintln(os.Stderr)
		return nil, false
	}
	return env, true
}

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}
