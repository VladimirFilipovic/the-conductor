package cmd

import (
	"context"
	"fmt"
	"os"

	"conductor/internal/link"
	"conductor/internal/project"
	"conductor/internal/storage"
	"conductor/internal/target"
)

const initUsage = `conductor init -n NAME

Create a new project and its default "production" environment, then link this
directory to it (writes .conductor/config.json).`

func cmdInit(args []string) error {
	fs := newFlagSet("init", initUsage)

	var name string
	fs.StringVar(&name, "n", "", "project name")
	fs.StringVar(&name, "name", "", "project name")

	if err := fs.parse(args); err != nil {
		return err
	}

	if name == "" {
		return usageErr(initUsage, "a project name is required (-n NAME)")
	}

	ctx := context.Background()
	store, err := storage.NewPostgresClient(ctx, databaseURL())
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = store.Close() }()

	projects := project.New(store)
	if _, err := projects.Create(ctx, name, link.DefaultEnvironment); err != nil {
		return err
	}

	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	if err := link.Save(dir, target.Target{Project: name, Environment: link.DefaultEnvironment}); err != nil {
		return err
	}

	fmt.Printf("created project %q (environment %q) and linked %s\n", name, link.DefaultEnvironment, link.Path(dir))
	return nil
}
