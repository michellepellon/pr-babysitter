// ABOUTME: Entry point for pr-babysitter: dispatches to the plan, prove, apply, and gateway subcommands.
// ABOUTME: A missing or unknown subcommand prints usage and exits 2.

package main

import (
	"fmt"
	"io"
	"os"
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

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}
