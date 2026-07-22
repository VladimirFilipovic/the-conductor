package control

import (
	"context"
	"fmt"
	"os"
	"strings"

	"conductor/internal/config"
	"conductor/internal/link"
	"conductor/internal/project"
	"conductor/internal/storage"
	"conductor/internal/target"
)

const linkUsage = `conductor link -p PROJECT [-e ENVIRONMENT] [-s SERVICE]

Point this directory at a project in the control plane by writing
.conductor/config.json. Subsequent commands resolve their target from this file
when -p/-e/-s are omitted. The environment defaults to "production"; the service
pointer is optional.

The pointers are per-developer; you probably want to gitignore .conductor/.

Use 'conductor environment select' / 'conductor service select' to set the
environment/service pointers, and 'conductor unlink' to remove the link.`

func cmdLink(args []string) error {
	fs := newFlagSet("link", linkUsage)
	var t Target
	addProjectFlag(fs, &t)
	addEnvironmentFlag(fs, &t)
	addServiceFlag(fs, &t)
	if err := fs.parse(args); err != nil {
		return err
	}
	// useLink=false: an existing link must not silently satisfy a re-link.
	resolve(&t, false)
	if t.Project == "" {
		return usageErr(linkUsage, "link requires a project (--project/-p or "+config.VarProject+")")
	}

	dir, err := os.Getwd()
	if err != nil {
		return err
	}

	l := target.Target{Project: t.Project, Environment: link.DefaultEnvironment, Service: t.Service}
	if t.Environment != "" {
		l.Environment = t.Environment
	}

	// Validate before persisting so a typo'd pointer fails here, not on the
	// first command that resolves the link.
	ctx := context.Background()
	store, err := storage.NewPostgresClient(ctx, databaseURL())
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = store.Close() }()
	if err := project.New(store).Verify(ctx, l); err != nil {
		return err
	}

	if err := link.Save(dir, l); err != nil {
		return err
	}

	fmt.Printf("linked %s to project %q (environment %q", link.Path(dir), l.Project, l.Environment)
	if l.Service != "" {
		fmt.Printf(", service %q", l.Service)
	}
	fmt.Println(")")
	fmt.Printf("  tip: add %s/ to .gitignore — these pointers are per-developer\n", link.DirName)
	return nil
}

const unlinkUsage = `conductor unlink

Remove the nearest .conductor/config.json, detaching this directory from its
project. Does not touch the control plane.`

func cmdUnlink(args []string) error {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return usageErr(unlinkUsage, "unlink takes no arguments")
	}
	dir, err := link.Remove()
	if err != nil {
		return err
	}
	fmt.Printf("unlinked %s\n", link.Path(dir))
	return nil
}
