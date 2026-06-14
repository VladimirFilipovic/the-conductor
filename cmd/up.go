package cmd

const upUsage = `conductor up [path]

Tarball the code directory (default "."), build it, and commit the result as
the service's desired state. The reconcile loop then schedules, health-gates,
shifts traffic, and drains the old replicas. Requires project, environment,
and service.`

func cmdUp(args []string) error {
	// Peel the leading [path] positional before flag parsing: stdlib flag stops
	// at the first positional, so `up ./dir -s foo` would otherwise drop -s foo.
	path, rest := splitSubcommand(args)

	fs := newFlagSet("up", upUsage)
	var t Target
	addTargetFlags(fs, &t)
	if err := fs.parse(rest); err != nil {
		return err
	}
	resolve(&t, true)
	if err := t.require(true, true); err != nil {
		return usageErr(upUsage, err.Error())
	}
	if fs.NArg() > 0 {
		return usageErr(upUsage, "unexpected argument: "+fs.Arg(0)+" (path goes first: up [path])")
	}

	if path == "" {
		path = "."
	}
	return engineTODO("deploy", t, "code: "+path)
}
