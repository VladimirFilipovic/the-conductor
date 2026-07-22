package control

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"conductor/internal/config"
	"conductor/internal/link"
	"conductor/internal/target"
)

// Set by the linker (`-ldflags "-X conductor/cmd.version=..."`) for release builds.
var version = "dev"

// Loaded once in Run; commands read DSN / identity defaults from here, not os.Getenv.
var cfg config.Config

func databaseURL() string { return cfg.DatabaseURL }

const usage = `conductor — drive the orchestration engine

Usage:
  conductor <command> [args] [--project P --environment E --service S]

Project lifecycle:
  init -n NAME             Create a new project (+ default "production" env)
  link -p NAME             Point this directory at a project (.conductor/config.json)
  unlink                   Remove this directory's link
  config                   Print resolved identity + config.toml spec
  add                      Add a service or database to the project
  environment [sub]        Print/list/create/select environments
  service [name]           List or select services

Desired-state mutations (the reconcile loop converges to these):
  up [config.toml]         Build & deploy the linked service from its spec (see example/)
  rollback [--to vN]       Re-point the service at an earlier version (no rebuild)
  down                     Scale a service to zero (volumes/data preserved)
  scale <region=N ...>     Patch per-region replica counts
  volume <subcommand>      Manage persistent volumes (add/list/update/rm)

Observability:
  status [-e E -s S]       Show observed vs. desired state (table; -e/-s narrow)

Engine:
  engine                   Start the orchestration engine (the reconcile loop)

Target resolution (flags win over env vars):
  --project,     -p   project name or id   (env: CONDUCTOR_PROJECT)
  --environment, -e   environment name     (env: CONDUCTOR_ENVIRONMENT)
  --service,     -s   service name         (env: CONDUCTOR_SERVICE)

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

	loaded, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "conductor: %s\n", err)
		return 1
	}
	cfg = loaded

	cmdArgs := args[1:]
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
	case "rollback":
		err = cmdRollback(cmdArgs)
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

// flagSet carries the command's usage text so parse failures render uniformly.
type flagSet struct {
	*flag.FlagSet
	usage string
}

// newFlagSet silences the flag package's own error/usage dump; parse turns
// failures into a clear usageError instead.
func newFlagSet(name, usageText string) *flagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	return &flagSet{FlagSet: fs, usage: usageText}
}

// parse translates the flag package's terse failures: an unknown flag becomes
// an "unsupported flag" usageError (exit 2); -h/--help prints usage, exits 0.
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

// usageError marks a bad invocation; exitCode prints it and returns 2.
type usageError struct{ text string }

func (e *usageError) Error() string { return e.text }

func usageErr(usageText, msg string) error {
	return &usageError{text: fmt.Sprintf("%s\n\n%s", msg, usageText)}
}

// Target is the (project, environment, service) triple every command resolves
// before talking to the engine. Precedence: flags > env vars > folder-link file
// (.conductor/config.json, found by walking up from cwd) — the link is the
// lowest tier so an explicit -e always overrides it.
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

// require lists everything missing at once so the user learns the whole gap in
// one shot. Project is always required; environment/service per the flags.
func (c Target) require(environment, service bool) error {
	var missing []string
	if c.Project == "" {
		missing = append(missing, "--project/-p (or "+config.VarProject+")")
	}
	if environment && c.Environment == "" {
		missing = append(missing, "--environment/-e (or "+config.VarEnvironment+")")
	}
	if service && c.Service == "" {
		missing = append(missing, "--service/-s (or "+config.VarService+")")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required target: %s", strings.Join(missing, ", "))
	}
	return nil
}

// addTargetFlags registers -p/-e/-s (and long forms). Split per field so a
// command that overloads a name can opt out of just that flag — `add --service
// NAME` means the service to create, so add registers project+environment only.
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

// resolve fills empty Target fields: flag > env var > (when useLink) the
// folder-link file. `link` passes useLink=false so an existing link can't
// silently satisfy a re-link (the whole point of `link` is to (re)set it).
func resolve(t *Target, useLink bool) {
	t.Project = orDefault(t.Project, cfg.Project)
	t.Environment = orDefault(t.Environment, cfg.Environment)
	t.Service = orDefault(t.Service, cfg.Service)
	if !useLink {
		return
	}
	if l, _, ok, err := link.Load(); err == nil && ok {
		t.Project = orDefault(t.Project, l.Project)
		t.Environment = orDefault(t.Environment, l.Environment)
		t.Service = orDefault(t.Service, l.Service)
	}
}

// resolveProject fills only the project (flag > env var > link). Whole-project
// commands (status) use it so the link's env/service can't silently narrow the
// output — only an explicit -e/-s does.
func resolveProject(t *Target) {
	if t.Project = orDefault(t.Project, cfg.Project); t.Project != "" {
		return
	}
	if l, _, ok, err := link.Load(); err == nil && ok {
		t.Project = l.Project
	}
}

func splitSubcommand(args []string) (sub string, rest []string) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return "", args
	}
	return args[0], args[1:]
}

func orDefault(val, fallback string) string {
	if val != "" {
		return val
	}
	return fallback
}
