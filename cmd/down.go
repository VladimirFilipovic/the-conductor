package cmd

const downUsage = `conductor down [--yes]

Scale the service's replicas to zero. Stateful data (volumes) is preserved —
this stops compute, it does not delete the service. Requires project,
environment, and service.

Flags:
  --yes   skip the confirmation prompt`

func cmdDown(args []string) error {
	fs := newFlagSet("down", downUsage)
	var t Target
	addTargetFlags(fs, &t)
	yes := fs.Bool("yes", false, "skip confirmation")
	if err := fs.parse(args); err != nil {
		return err
	}
	resolve(&t, true)
	if err := t.require(true, true); err != nil {
		return usageErr(downUsage, err.Error())
	}

	detail := "replicas → 0 (volumes preserved)"
	if *yes {
		detail += "  [confirmed]"
	}
	return engineTODO("scale down", t, detail)
}
