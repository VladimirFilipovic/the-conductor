package cmd

import (
	"context"
	"fmt"
	"os"

	"conductor/internal/link"
	"conductor/internal/project"
	"conductor/internal/storage"
)

const addUsage = `conductor add (--service | --database --engine TYPE) --name NAME [--image IMG] [--repo URL] [--stateful]

Add a service to the project. --service is a code service, built from this repo
(or the given --image); pass --stateful to run it as a single instance with a
persistent volume (recreate-on-deploy, no replicas). --database is a preset: a
stateful service on a managed image chosen by --engine. Requires a project and
environment.

Flags:
  --service        add a code service
  --database       add a managed, stateful database (a --service preset)
  --name NAME      service name (required, unique within the project)
  --engine TYPE    database engine: postgres, redis, mysql (required with --database)
  --image IMG      base image for a --service (skip the build step)
  --repo URL       source repo for a --service (default: current directory)
  --stateful       mark a --service stateful (persistent volume, single instance)
  --link, -l       point this directory's link at the new service`

var dbEngineImages = map[string]string{
	"postgres": "postgres:16",
	"redis":    "redis:7",
	"mysql":    "mysql:8",
}

func cmdAdd(args []string) error {
	fs := newFlagSet("add", addUsage)
	var t Target

	addProjectFlag(fs, &t)
	addEnvironmentFlag(fs, &t)

	service := fs.Bool("service", false, "add a code service")
	database := fs.Bool("database", false, "add a managed, stateful database")
	name := fs.String("name", "", "service name (unique within the project)")
	engine := fs.String("engine", "", "database engine: postgres, redis, mysql")
	image := fs.String("image", "", "base image for a code service")
	repo := fs.String("repo", "", "source repo for a code service")
	stateful := fs.Bool("stateful", false, "mark a --service stateful (persistent volume, single instance)")
	var linkAfter bool
	fs.BoolVar(&linkAfter, "l", false, "point this directory's link at the new service")
	fs.BoolVar(&linkAfter, "link", false, "point this directory's link at the new service")

	if err := fs.parse(args); err != nil {
		return err
	}

	resolve(&t, true)
	if err := t.require(true, false); err != nil {
		return usageErr(addUsage, err.Error())
	}

	if *service == *database {
		return usageErr(addUsage, "exactly one of --service or --database is required")
	}
	if *name == "" {
		return usageErr(addUsage, "--name is required")
	}

	t.Service = *name
	if *service && *engine != "" {
		return usageErr(addUsage, "--engine is only valid with --database")
	}
	if *stateful && *database {
		return usageErr(addUsage, "--stateful is redundant with --database (databases are already stateful)")
	}

	ctx := context.Background()
	store, err := storage.NewPostgresClient(ctx, databaseURL())
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = store.Close() }()

	proj := project.New(store)
	if *database {
		if *image != "" || *repo != "" {
			return usageErr(addUsage, "--image and --repo are only valid with --service")
		}
		if err := addDatabase(ctx, proj, t, *engine); err != nil {
			return err
		}
	} else if err := addService(ctx, proj, t, *image, *repo, *stateful); err != nil {
		return err
	}

	if linkAfter {
		return linkToService(t)
	}
	return nil
}

// linkToService points the nearest link at the just-added service (t already
// carries project/environment/service). The service is known to exist — it was
// just created in the same invocation — so no extra validation is needed.
func linkToService(t Target) error {
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	if err := link.Save(dir, t.Target); err != nil {
		return err
	}
	fmt.Printf("linked %s to service %q\n", link.Path(dir), t.Service)
	return nil
}

func addDatabase(ctx context.Context, proj *project.Service, t Target, engine string) error {
	image, validEngine := dbEngineImages[engine]
	if !validEngine {
		return usageErr(addUsage, "--engine "+engine+" is not one of postgres, redis, mysql")
	}
	db, err := proj.AddService(ctx, project.AddServiceInput{
		Target:   t.Target,
		Stateful: true,
		Source:   project.Source{Image: image},
	})
	if err != nil {
		return err
	}

	fmt.Printf("added service %q to %s / %s\n", db.Name, t.Project, t.Environment)
	return nil
}

func addService(ctx context.Context, proj *project.Service, t Target, image, repo string, stateful bool) error {
	if image != "" && repo != "" {
		return usageErr(addUsage, "--image and --repo are mutually exclusive")
	}

	svc, err := proj.AddService(ctx, project.AddServiceInput{
		Target:   t.Target,
		Stateful: stateful,
		Source:   project.Source{Repo: repo, Image: image},
	})
	if err != nil {
		return err
	}

	kind := "service"
	if stateful {
		kind = "stateful service"
	}
	fmt.Printf("added %s %q to %s / %s\n", kind, svc.Name, t.Project, t.Environment)
	return nil
}
