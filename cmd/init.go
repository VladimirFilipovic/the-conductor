package cmd

import (
	"context"
	"fmt"
	"os"

	"conductor/internal/project"
	"conductor/internal/storage"
)

const initUsage = `conductor init -n NAME

Create a new project and its default "production" environment.`

func cmdInit(args []string) error {
	fs := newFlagSet("init", initUsage)

	var name string
	fs.StringVar(&name, "n", "", "project name")
	fs.StringVar(&name, "name", "", "project name")

	if err := fs.Parse(args); err != nil {
		return err
	}

	if name == "" {
		return usageErr(initUsage, "a project name is required (-n NAME)")
	}

	dsn := os.Getenv("CONDUCTOR_DATABASE_URL")
	if dsn == "" {
		return fmt.Errorf("CONDUCTOR_DATABASE_URL is not set")
	}

	ctx := context.Background()
	store, err := storage.NewPostgresClient(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer store.Close()

	projects := project.NewService(store)
	if _, err := projects.Create(ctx, name, "production"); err != nil {
		return err
	}

	fmt.Printf("created project %q with environment \"production\"\n", name)
	return nil
}
