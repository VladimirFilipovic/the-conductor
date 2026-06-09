package cmd

const serviceUsage = `conductor service [name]

With no argument, list the services in the current environment. With a name,
select it as the active service for subsequent commands. Requires a project
and environment.`

func cmdService(args []string) error {
	rest, ctx := extractTarget(args)
	fs := newFlagSet("service", serviceUsage)
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if err := ctx.require(true, true, false); err != nil {
		return usageErr(serviceUsage, err.Error())
	}

	if name := fs.Arg(0); name != "" {
		ctx.Service = name
		return engineTODO("select service "+name, ctx, "")
	}
	return engineTODO("list services", ctx, "")
}
