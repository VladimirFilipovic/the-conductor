package cmd

const statusUsage = `conductor status

Show the observed state of the target and how it compares to desired state —
replicas, regions, health, and the active deploy. Requires a project; narrows
to an environment and service when those are supplied.`

func cmdStatus(args []string) error {
	fs := newFlagSet("status", statusUsage)
	var t Target
	addTargetFlags(fs, &t)
	if err := fs.parse(args); err != nil {
		return err
	}
	resolve(&t, true)
	if err := t.require(false, false); err != nil {
		return usageErr(statusUsage, err.Error())
	}
	return engineTODO("status", t, "observed vs. desired state")
}
