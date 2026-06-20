package control

import (
	"context"
	"fmt"
	"os"

	"conductor/build"
	"conductor/internal/deployspec"
	"conductor/internal/link"
	"conductor/internal/project"
	"conductor/internal/storage"
)

const upUsage = `conductor up [-f config.toml] [-p P -e E -s S]

Deploy a service: read its build/deploy spec (default "config.toml" in the
current directory), build its source into an image, and commit that as a new
deployment version. The reconcile loop then converges the fleet to it.

Which project/environment/service this deploys to comes from the folder link
(.conductor/config.json) and -p/-e/-s — NOT from the spec file, which carries
build/deploy settings only. The service must already exist (run 'conductor add'
first). See example/config.toml for an annotated spec.

Flags:
  -f, --file PATH   build/deploy spec to read (default "config.toml")`

// upBuilder is the source-to-image backend `up` uses for repo services. It
// defaults to build.NoOp (a synthesized local ref, no real build); an embedder
// wiring conductor into their own binary overrides it with a Builder that shells
// out to nixpacks, a Dockerfile build, or their own pipeline.
var upBuilder build.Builder = build.NoOp{}

func cmdUp(args []string) error {
	fs := newFlagSet("up", upUsage)
	var t Target
	addTargetFlags(fs, &t)
	// Spec path is a flag, not a positional, so flags parse in any order (stdlib
	// flag stops at the first positional, which would otherwise drop trailing flags).
	var file string
	fs.StringVar(&file, "f", deployspec.FileName, `build/deploy spec path`)
	fs.StringVar(&file, "file", deployspec.FileName, `build/deploy spec path`)
	if err := fs.parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return usageErr(upUsage, "unexpected argument: "+fs.Arg(0)+" (the spec path is a flag: -f PATH)")
	}
	resolve(&t, true)
	if err := t.require(true, true); err != nil {
		return usageErr(upUsage, err.Error())
	}

	if _, statErr := os.Stat(file); os.IsNotExist(statErr) {
		if file == deployspec.FileName {
			return fmt.Errorf("no %s in the current directory — pass -f PATH or create one (see example/config.toml)", deployspec.FileName)
		}
		return fmt.Errorf("spec file %q does not exist", file)
	}
	spec, err := deployspec.Load(file)
	if err != nil {
		return err
	}
	buildCfg, deployCfg := spec.Resolve(t.Environment)

	ctx := context.Background()
	store, err := storage.NewPostgresClient(ctx, databaseURL())
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = store.Close() }()
	proj := project.New(store)

	// Source (repo/image) lives in the control plane, recorded by `add`; the spec
	// never carries it. Resolve it to a concrete image before opening the deploy
	// tx — a build must not run with a DB transaction held open.
	src, err := proj.ServiceSource(ctx, t.Target)
	if err != nil {
		return err
	}
	req := build.Request{
		Project:      t.Project,
		Environment:  t.Environment,
		Service:      t.Service,
		Repo:         src.Source.Repo,
		Root:         buildCfg.Root,
		Strategy:     build.Strategy(buildCfg.Builder),
		Dockerfile:   buildCfg.Dockerfile,
		BuildCommand: buildCfg.BuildCommand,
	}
	// A service with neither a recorded image nor repo builds from the linked
	// working tree (the dir holding .conductor, else cwd) — the "source lives
	// where you run up" contract (see project.Source). cwd is the build context
	// only the CLI knows, so it is resolved here, not in build.Resolve.
	if src.Source.Image == "" && req.Repo == "" {
		if req.Repo, err = workingTree(); err != nil {
			return err
		}
	}
	image, err := build.Resolve(ctx, upBuilder, req, src.Source.Image)
	if err != nil {
		return err
	}

	res, err := proj.Deploy(ctx, project.DeployInput{
		Target:        t.Target,
		ImageRef:      image,
		CPUMillicores: deployCfg.CPUMillicores(),
		MemBytes:      deployCfg.MemBytes(),
		Healthcheck:   deployCfg.HealthcheckJSON(),
		DrainSeconds:  deployCfg.DrainSecondsOrDefault(),
		RestartMax:    deployCfg.RestartMaxOrDefault(),
		Region:        deployCfg.RegionOrDefault(),
		NumReplicas:   deployCfg.ReplicasOrDefault(),
		CreatedBy:     os.Getenv("USER"),
	})
	if err != nil {
		return err
	}

	fmt.Printf("→ deployed %s  v%d  (%s: %d replicas)  image=%s\n",
		t, res.Version, res.Region, res.Replicas, image)
	return nil
}

// workingTree is the directory `up` builds from when a service has no recorded
// repo or image: the linked working tree (the dir containing .conductor) if
// there is one, else the current directory.
func workingTree() (string, error) {
	dir, ok, err := link.Find()
	if err != nil {
		return "", err
	}
	if ok {
		return dir, nil
	}
	return os.Getwd()
}
