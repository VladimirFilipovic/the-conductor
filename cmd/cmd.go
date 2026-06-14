package cmd

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"conductor/internal/link"
	"conductor/internal/target"
)

// version is set by the linker (`-ldflags "-X conductor/cmd.version=..."`)
// for release builds; otherwise it falls back to "dev".
var version = "dev"

// envDatabaseURL holds the control-plane Postgres DSN.
const envDatabaseURL = "CONDUCTOR_DATABASE_URL"

// defaultDatabaseURL targets the local docker-compose Postgres (see
// docker-compose.yml) so dev commands work out of the box without exporting
// CONDUCTOR_DATABASE_URL. The Makefile sets the same value for `make` recipes.
const defaultDatabaseURL = "postgres://conductor:conductor@localhost:5432/conductor?sslmode=disable"

// databaseURL resolves the control-plane DSN, falling back to the local dev
// database when CONDUCTOR_DATABASE_URL is unset.
func databaseURL() string {
	if dsn := os.Getenv(envDatabaseURL); dsn != "" {
		return dsn
	}
	return defaultDatabaseURL
}

const usage = `conductor — drive the orchestration engine

Usage:
  conductor <command> [args] [--project P --environment E --service S]

Project lifecycle:
  init -n NAME             Create a new project (+ default "production" env)
  link -p NAME             Point this directory at a project (.conductor/config.json)
  unlink                   Remove this directory's link
  config                   Print resolved identity + conductor.toml manifest
  add                      Add a service or database to the project
  environment [sub]        Print/list/create/select environments
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
	case "link":
		err = cmdLink(cmdArgs)
	case "unlink":
		err = cmdUnlink(cmdArgs)
	case "config":
		err = cmdConfig(cmdArgs)
	case "add":
		err = cmdAdd(cmdArgs)
	case "environment", "environments", "env", "envs":
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
		fmt.Fprintf(os.Stderr, "conductor: %s\n", err)
		return 1
	}
}

// flagSet wraps *flag.FlagSet with the command's usage text so parse failures
// can be rendered uniformly (see parse).
type flagSet struct {
	*flag.FlagSet
	usage string
}

// newFlagSet builds a flag set whose own error/usage output is silenced; parse
// turns failures into a clear usageError instead of the flag package's dump.
func newFlagSet(name, usageText string) *flagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	return &flagSet{FlagSet: fs, usage: usageText}
}

// parse runs the underlying Parse, translating the flag package's terse
// failures: an unknown flag becomes an explicit "unsupported flag" usageError
// (exit 2), and -h/--help prints the usage and exits 0.
func (fs *flagSet) parse(args []string) error {
	switch err := fs.Parse(args); {
	case err == nil:
		return nil
	case errors.Is(err, flag.ErrHelp):
		fmt.Println(fs.usage)
		return err
	default:
		msg := err.Error()
		if name, ok := strings.CutPrefix(msg, "flag provided but not defined: "); ok {
			msg = fmt.Sprintf("unsupported flag %s for %q", name, fs.Name())
		}
		return usageErr(fs.usage, msg)
	}
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
func engineTODO(action string, c Target, detail string) error {
	fmt.Printf("→ %s  %s\n", action, c)
	if detail != "" {
		fmt.Printf("  %s\n", detail)
	}
	fmt.Println("  (not implemented — would patch desired state and reconcile)")
	return nil
}

// Environment-variable fallbacks, consulted when the matching flag is empty.
const (
	envProject     = "CONDUCTOR_PROJECT"
	envEnvironment = "CONDUCTOR_ENVIRONMENT"
	envService     = "CONDUCTOR_SERVICE"
	envToken       = "CONDUCTOR_TOKEN" //nolint:unused // consumed by the engine client
)

// Target is the (project, environment, service) triple every command resolves
// before talking to the orchestration engine. Identity comes, in precedence
// order, from flags, then environment variables, then the folder-link file
// (.conductor/config.json) discovered by walking up from cwd (see link.go).
// The link file is the lowest tier so an explicit -e always overrides it.
type Target struct {
	target.Target
}

func (c Target) String() string {
	return fmt.Sprintf("%s / %s / %s", dash(c.Project), dash(c.Environment), dash(c.Service))
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// require checks that the target is present, returning an error listing
// everything that is missing so the user learns the whole gap at once. Project
// is always required; environment/service are gated by the flags.
func (c Target) require(environment, service bool) error {
	var missing []string
	if c.Project == "" {
		missing = append(missing, "--project/-p (or "+envProject+")")
	}
	if environment && c.Environment == "" {
		missing = append(missing, "--environment/-e (or "+envEnvironment+")")
	}
	if service && c.Service == "" {
		missing = append(missing, "--service/-s (or "+envService+")")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required target: %s", strings.Join(missing, ", "))
	}
	return nil
}

// addTargetFlags registers the universal -p/-e/-s flags (and their long forms)
// on a command's flag set, writing straight into t. It is split per field so
// a command that overloads one of the names can opt out of just that flag —
// `add --service NAME` means the service to create, not a target, so add
// registers project+environment only.
func addTargetFlags(fs *flagSet, t *Target) {
	addProjectFlag(fs, t)
	addEnvironmentFlag(fs, t)
	addServiceFlag(fs, t)
}

func addProjectFlag(fs *flagSet, t *Target) {
	fs.StringVar(&t.Project, "p", "", "project name or id")
	fs.StringVar(&t.Project, "project", "", "project name or id")
}

func addEnvironmentFlag(fs *flagSet, t *Target) {
	fs.StringVar(&t.Environment, "e", "", "environment name")
	fs.StringVar(&t.Environment, "environment", "", "environment name")
}

func addServiceFlag(fs *flagSet, t *Target) {
	fs.StringVar(&t.Service, "s", "", "service name")
	fs.StringVar(&t.Service, "service", "", "service name")
}

// resolve fills any Target field the flags left empty from the matching env
// var, then — when useLink is true — from the folder-link file discovered by
// walking up from cwd. Precedence is flag > env var > link file. `link` passes
// useLink=false so an existing link's project can't silently satisfy a re-link
// (the whole point of `link` is to (re)set it).
func resolve(t *Target, useLink bool) {
	t.Project = orEnv(t.Project, envProject)
	t.Environment = orEnv(t.Environment, envEnvironment)
	t.Service = orEnv(t.Service, envService)
	if !useLink {
		return
	}
	if l, _, ok, err := link.Load(); err == nil && ok {
		t.Project = orDefault(t.Project, l.Project)
		t.Environment = orDefault(t.Environment, l.Environment)
		t.Service = orDefault(t.Service, l.Service)
	}
}

// splitSubcommand peels the leading verb off args. A leading flag (or empty
// args) means no verb was given; the universal target flags are then parsed
// from what remains. Verb comes first so the command's flag set never has to
// see a positional before its flags (stdlib flag stops at the first positional).
func splitSubcommand(args []string) (sub string, rest []string) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return "", args
	}
	return args[0], args[1:]
}

func orEnv(flagVal, key string) string {
	if flagVal != "" {
		return flagVal
	}
	return os.Getenv(key)
}

func orDefault(val, fallback string) string {
	if val != "" {
		return val
	}
	return fallback
}
