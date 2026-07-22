package control

import (
	"context"
	"errors"
	"fmt"

	"conductor/internal/link"
	"conductor/internal/project"
	"conductor/internal/storage"
)

const environmentsUsage = `conductor environments [<subcommand>] --project P

Subcommands:
  (none)            Print the currently selected environment for the project.
  list              List the project's environments.
  create -n NAME    Create a fresh environment by cloning the project's services.
  select NAME       Set NAME as this CLI's active environment for the project.
  use NAME          Alias for select.

Requires a project (--project/-p or CONDUCTOR_PROJECT, or the linked project).
select/use writes the environment pointer into the nearest .conductor/config.json
and is used whenever -e/--environment is omitted.`

func cmdEnvironment(args []string) error {
	sub, rest := splitSubcommand(args)
	switch sub {
	case "":
		return printCurrentEnvironment(rest)
	case "list":
		return listEnvironments(rest)
	case "create", "new":
		return createEnvironment(rest)
	case "select", "use":
		return selectEnvironmentCmd(rest)
	default:
		return usageErr(environmentsUsage, fmt.Sprintf("unknown subcommand %q", sub))
	}
}

func environmentTarget(args []string) (Target, error) {
	fs := newFlagSet("environment", environmentsUsage)
	var t Target
	addProjectFlag(fs, &t)
	addEnvironmentFlag(fs, &t)
	if err := fs.parse(args); err != nil {
		return Target{}, err
	}
	resolve(&t, true)
	return t, nil
}

func printCurrentEnvironment(args []string) error {
	t, err := environmentTarget(args)
	if err != nil {
		return err
	}
	if err := t.require(false, false); err != nil {
		return usageErr(environmentsUsage, err.Error())
	}
	if t.Environment == "" {
		fmt.Printf("no environment selected for project %q (conductor environments select -p %s NAME)\n", t.Project, t.Project)
		return nil
	}
	fmt.Println(t.Environment)
	return nil
}

func listEnvironments(args []string) error {
	t, err := environmentTarget(args)
	if err != nil {
		return err
	}
	if err := t.require(false, false); err != nil {
		return usageErr(environmentsUsage, err.Error())
	}

	ctx := context.Background()
	store, err := storage.NewPostgresClient(ctx, databaseURL())
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = store.Close() }()

	envs, err := project.New(store).ListEnvironments(ctx, t.Project)
	if err != nil {
		return err
	}
	if len(envs) == 0 {
		fmt.Printf("no environments in project %q\n", t.Project)
		return nil
	}
	// Mark the resolved environment so list doubles as "which one am I on".
	for _, e := range envs {
		marker := "  "
		if e.Name == t.Environment {
			marker = "* "
		}
		fmt.Printf("%s%s\n", marker, e.Name)
	}
	return nil
}

func createEnvironment(args []string) error {
	fs := newFlagSet("environment create", environmentsUsage)
	var t Target
	addProjectFlag(fs, &t)
	addEnvironmentFlag(fs, &t)
	var name string
	fs.StringVar(&name, "n", "", "environment name")
	fs.StringVar(&name, "name", "", "environment name")
	if err := fs.parse(args); err != nil {
		return err
	}
	resolve(&t, true)
	if err := t.require(false, false); err != nil {
		return usageErr(environmentsUsage, err.Error())
	}
	if name == "" {
		return usageErr(environmentsUsage, "create requires a name (-n NAME)")
	}
	// The resolved environment (link or -e) is the clone source; capture it
	// before the new name takes its place in the target.
	sourceEnv := t.Environment

	ctx := context.Background()
	store, err := storage.NewPostgresClient(ctx, databaseURL())
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = store.Close() }()

	res, err := project.New(store).CreateEnvironment(ctx, t.Project, sourceEnv, name)
	if err != nil {
		return err
	}

	if res.SourceEnv == "" {
		fmt.Printf("created environment %q in project %q (empty — no source environment to clone)\n", name, t.Project)
		return nil
	}
	fmt.Printf("created environment %q in project %q (cloned %d service(s) from %q)\n", name, t.Project, res.ServicesCloned, res.SourceEnv)
	return nil
}

func selectEnvironmentCmd(args []string) error {
	fs := newFlagSet("environment select", environmentsUsage)
	var t Target
	addProjectFlag(fs, &t)
	if err := fs.parse(args); err != nil {
		return err
	}
	resolve(&t, true)
	if fs.NArg() != 1 || fs.Arg(0) == "" {
		return usageErr(environmentsUsage, "select requires exactly one environment name")
	}
	name := fs.Arg(0)
	dir, err := link.SetEnvironment(name)
	if errors.Is(err, link.ErrNotFound) {
		return usageErr(environmentsUsage, "no link here — run `conductor link -p "+t.Project+"` first")
	}
	if err != nil {
		return err
	}
	fmt.Printf("selected environment %q in %s\n", name, dir)
	return nil
}
