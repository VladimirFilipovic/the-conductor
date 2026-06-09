package cmd

const downUsage = `conductor down [--yes]

Scale the service's replicas to zero. Stateful data (volumes) is preserved —
this stops compute, it does not delete the service. Requires project,
environment, and service.

Flags:
  --yes   skip the confirmation prompt`

func cmdDown(args []string) error {
	rest, ctx := extractTarget(args)
	fs := newFlagSet("down", downUsage)
	yes := fs.Bool("yes", false, "skip confirmation")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if err := ctx.require(true, true, true); err != nil {
		return usageErr(downUsage, err.Error())
	}

	detail := "replicas → 0 (volumes preserved)"
	if *yes {
		detail += "  [confirmed]"
	}
	return engineTODO("scale down", ctx, detail)
}
