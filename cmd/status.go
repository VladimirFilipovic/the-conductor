package cmd

const statusUsage = `conductor status

Show the observed state of the target and how it compares to desired state —
replicas, regions, health, and the active deploy. Requires a project; narrows
to an environment and service when those are supplied.`

func cmdStatus(args []string) error {
	rest, ctx := extractTarget(args)
	fs := newFlagSet("status", statusUsage)
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if err := ctx.require(true, false, false); err != nil {
		return usageErr(statusUsage, err.Error())
	}
	return engineTODO("status", ctx, "observed vs. desired state")
}
