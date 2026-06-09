package cmd

const environmentUsage = `conductor environment [name]
conductor environment new <name>

With no argument, list the project's environments. With a name, select it as
the active environment for subsequent commands. With "new", create a fresh
environment by cloning the project's services. Requires a project.`

func cmdEnvironment(args []string) error {
	rest, ctx := extractTarget(args)
	fs := newFlagSet("environment", environmentUsage)
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if err := ctx.require(true, false, false); err != nil {
		return usageErr(environmentUsage, err.Error())
	}

	switch fs.Arg(0) {
	case "":
		return engineTODO("list environments", ctx, "")
	case "new", "create":
		name := fs.Arg(1)
		if name == "" {
			return usageErr(environmentUsage, "environment new requires a name")
		}
		ctx.Environment = name
		return engineTODO("create environment "+name, ctx, "clones existing services")
	default:
		ctx.Environment = fs.Arg(0)
		return engineTODO("select environment "+fs.Arg(0), ctx, "")
	}
}
