package cmd

const initUsage = `conductor init [name] [--name NAME]

Create a new project and its default "production" environment. The project
name may be given as a positional argument or via --name.`

func cmdInit(args []string) error {
	rest, _ := extractTarget(args)
	fs := newFlagSet("init", initUsage)
	name := fs.String("name", "", "project name (else the first positional arg)")
	if err := fs.Parse(rest); err != nil {
		return err
	}

	project := *name
	if project == "" && fs.NArg() > 0 {
		project = fs.Arg(0)
	}
	if project == "" {
		return usageErr(initUsage, "a project name is required")
	}

	return engineTODO("init project", Context{Project: project}, `creates default environment "production"`)
}
