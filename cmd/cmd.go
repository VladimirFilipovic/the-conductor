// Package cmd is the Conductor CLI dispatcher: Run parses argv, switches on the
// first token, and calls the matching subcommand. Each subcommand lives in its
// own file and follows the same shape — parse flags, resolve the
// (project, environment, service) target, then hand off to the orchestration
// engine.
//
// Unlike Railway there is no `link` command and no on-disk link file: every
// command resolves its target from flags (--project/-p, --environment/-e,
// --service/-s) or the matching CONDUCTOR_* environment variables. See
// context.go.
package cmd

import (
	"flag"
	"fmt"
	"os"
	"strconv"
)

// version is set by the linker (`-ldflags "-X conductor/cmd.version=..."`)
// for release builds; otherwise it falls back to "dev".
var version = "dev"

const usage = `conductor — drive the orchestration engine

Usage:
  conductor <command> [args] [--project P --environment E --service S]

Project lifecycle:
  init [name]              Create a new project (+ default "production" env)
  add                      Add a service or database to the project
  environment [name]       List, select, or create environments
  service [name]           List or select services

Desired-state mutations (the reconcile loop converges to these):
  up [path]                Build code & commit desired state for a service
  down                     Scale a service to zero (volumes/data preserved)
  scale <region=N ...>     Patch per-region replica counts
  volume <subcommand>      Manage persistent volumes (add/list/update/rm)

Observability:
  status                   Show observed state vs. desired state

Target resolution (flags win over env vars):
  --project,     -p   project name or id   (env: CONDUCTOR_PROJECT)
  --environment, -e   environment name     (env: CONDUCTOR_ENVIRONMENT)
  --service,     -s   service name         (env: CONDUCTOR_SERVICE)
  auth token          (env: CONDUCTOR_TOKEN)

Other:
  --help, -h           Show this message
  --version, -v        Print the version
`

// Run is the entry point invoked from main. It returns a process exit code.
func Run(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}

	switch args[0] {
	case "-h", "--help", "help":
		fmt.Print(usage)
		return 0
	case "-v", "--version", "version":
		fmt.Println("conductor", version)
		return 0
	}

	rest := args[1:]
	switch args[0] {
	case "init":
		return cmdInit(rest)
	case "add":
		return cmdAdd(rest)
	case "environment", "env":
		return cmdEnvironment(rest)
	case "service":
		return cmdService(rest)
	case "up", "deploy":
		return cmdUp(rest)
	case "down":
		return cmdDown(rest)
	case "scale":
		return cmdScale(rest)
	case "volume":
		return cmdVolume(rest)
	case "status":
		return cmdStatus(rest)
	}

	fmt.Fprintf(os.Stderr, "conductor: unknown command %q\n\n%s", args[0], usage)
	return 2
}

// --- shared helpers ---------------------------------------------------------

// contParse is a sentinel returned by parse to mean "parsing succeeded, keep
// going" — distinct from any real exit code.
const contParse = -1

// newFlagSet builds a flag set that prints the command's usage on error
// instead of the default noisy dump.
func newFlagSet(name, usageText string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprintln(os.Stderr, usageText) }
	return fs
}

// parse runs fs.Parse and maps the result to an exit code, or contParse to
// continue. flag.ContinueOnError already prints the error followed by usage.
func parse(fs *flag.FlagSet, args []string) int {
	switch err := fs.Parse(args); err {
	case nil:
		return contParse
	case flag.ErrHelp:
		return 0
	default:
		return 2
	}
}

// usageErr prints a one-line message plus the command usage and returns the
// "bad invocation" exit code (2).
func usageErr(usageText, msg string) int {
	fmt.Fprintf(os.Stderr, "conductor: %s\n\n%s\n", msg, usageText)
	return 2
}

// itoa is a small convenience wrapper around strconv.Itoa for building detail
// strings.
func itoa(n int) string { return strconv.Itoa(n) }

// engineTODO is the seam where a fully implemented command opens a client to
// the orchestration engine, sends a desired-state mutation (or reads observed
// state), and lets the reconcile loop converge. The interface layer stops here
// and just echoes the resolved target.
func engineTODO(action string, c Context, detail string) int {
	fmt.Printf("→ %s  %s\n", action, c)
	if detail != "" {
		fmt.Printf("  %s\n", detail)
	}
	fmt.Println("  (not implemented — would patch desired state and reconcile)")
	return 0
}
