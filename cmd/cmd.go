package cmd

import (
	"errors"
	"flag"
	"fmt"
	"os"
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

	cmdArgs := args[1:]
	var err error
	switch args[0] {
	case "init":
		err = cmdInit(cmdArgs)
	case "add":
		err = cmdAdd(cmdArgs)
	case "environment", "env":
		err = cmdEnvironment(cmdArgs)
	case "service":
		err = cmdService(cmdArgs)
	case "up", "deploy":
		err = cmdUp(cmdArgs)
	case "down":
		err = cmdDown(cmdArgs)
	case "scale":
		err = cmdScale(cmdArgs)
	case "volume":
		err = cmdVolume(cmdArgs)
	case "status":
		err = cmdStatus(cmdArgs)
	default:
		fmt.Fprintf(os.Stderr, "conductor: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
	return exitCode(err)
}

func exitCode(err error) int {
	var uerr *usageError
	switch {
	case err == nil:
		return 0
	case errors.Is(err, flag.ErrHelp):
		return 0
	case errors.As(err, &uerr):
		fmt.Fprintf(os.Stderr, "conductor: %s\n", err)
		return 2
	default:
		// A flag-parse error: the flag package already wrote the message and
		// usage to stderr, so just signal a bad invocation.
		return 2
	}
}

// newFlagSet builds a flag set that prints the command's usage on error
// instead of the default noisy dump.
func newFlagSet(name, usageText string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprintln(os.Stderr, usageText) }
	return fs
}

// usageError marks a bad invocation. exitCode prints it (with the conductor
// prefix) and returns 2. Flag-parse errors are not wrapped in it because the
// flag package prints those itself.
type usageError struct{ text string }

func (e *usageError) Error() string { return e.text }

func usageErr(usageText, msg string) error {
	return &usageError{text: fmt.Sprintf("%s\n\n%s", msg, usageText)}
}

// engineTODO is the seam where a fully implemented command opens a client to
// the orchestration engine, sends a desired-state mutation (or reads observed
// state), and lets the reconcile loop converge. The interface layer stops here
// and just echoes the resolved target.
func engineTODO(action string, c Context, detail string) error {
	fmt.Printf("→ %s  %s\n", action, c)
	if detail != "" {
		fmt.Printf("  %s\n", detail)
	}
	fmt.Println("  (not implemented — would patch desired state and reconcile)")
	return nil
}
