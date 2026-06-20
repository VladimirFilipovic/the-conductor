package control

import (
	"context"
	"errors"
	"fmt"
	"os"

	"conductor/internal/link"
	"conductor/internal/project"
	"conductor/internal/storage"
)

const serviceUsage = `conductor service [name]

Subcommands:
  (none)    List the services in the current environment.
  NAME      Select NAME as the active service: writes the service pointer into
            the nearest .conductor/config.json.

Requires a project and environment.`

func cmdService(args []string) error {
	fs := newFlagSet("service", serviceUsage)
	var t Target
	addProjectFlag(fs, &t)
	addEnvironmentFlag(fs, &t)
	if err := fs.parse(args); err != nil {
		return err
	}
	// At most one positional (the service name). stdlib flag stops at the first
	// positional, so a flag typed after the name (`service web -e prod`) lands here
	// as an extra arg — reject it loudly instead of silently dropping the flag.
	if fs.NArg() > 1 {
		return usageErr(serviceUsage, "unexpected argument "+fs.Arg(1)+" (flags must come before the service name)")
	}
	resolve(&t, true)
	if err := t.require(true, false); err != nil {
		return usageErr(serviceUsage, err.Error())
	}

	ctx := context.Background()
	store, err := storage.NewPostgresClient(ctx, databaseURL())
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = store.Close() }()
	proj := project.New(store)

	if name := fs.Arg(0); name != "" {
		return selectServiceCmd(ctx, proj, t, name)
	}
	return listServicesCmd(ctx, proj, t)
}

func listServicesCmd(ctx context.Context, proj *project.Service, t Target) error {
	services, err := proj.ListServices(ctx, t.Target)
	if err != nil {
		return err
	}
	if len(services) == 0 {
		fmt.Printf("no services in %s / %s\n", t.Project, t.Environment)
		return nil
	}
	for _, s := range services {
		kind := "service"
		if s.Stateful {
			kind = "stateful"
		}
		fmt.Printf("%-20s %s\n", s.Name, kind)
	}
	return nil
}

func selectServiceCmd(ctx context.Context, proj *project.Service, t Target, name string) error {
	// Validate before writing the pointer so a typo fails here rather than on the
	// next command that resolves the link. Reuse t for project/environment.
	t.Service = name
	if err := proj.Verify(ctx, t.Target); err != nil {
		return err
	}

	dir, err := link.SetService(name)
	if errors.Is(err, link.ErrNotFound) {
		// No link yet: create one from the resolved project/environment (flags or
		// CONDUCTOR_PROJECT/CONDUCTOR_ENVIRONMENT), so `service NAME` bootstraps a
		// link instead of demanding a separate `link` first.
		dir, err = os.Getwd()
		if err != nil {
			return err
		}
		if err := link.Save(dir, t.Target); err != nil {
			return err
		}
		fmt.Printf("linked %s and selected service %q\n", link.Path(dir), name)
		return nil
	}
	if err != nil {
		return err
	}
	fmt.Printf("selected service %q in %s\n", name, dir)
	return nil
}
