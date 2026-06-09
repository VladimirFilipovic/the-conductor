package cmd

const upUsage = `conductor up [path] [--ci] [--detach]

Tarball the code directory (default "."), build it, and commit the result as
the service's desired state. The reconcile loop then schedules, health-gates,
shifts traffic, and drains the old replicas. Requires project, environment,
and service.

Flags:
  --ci       non-interactive: never prompt, fail fast on ambiguity
  --detach   return immediately instead of streaming the deploy`

func cmdUp(args []string) error {
	rest, ctx := extractTarget(args)
	fs := newFlagSet("up", upUsage)
	ci := fs.Bool("ci", false, "non-interactive mode")
	detach := fs.Bool("detach", false, "do not stream the deploy")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if err := ctx.require(true, true, true); err != nil {
		return usageErr(upUsage, err.Error())
	}

	path := fs.Arg(0)
	if path == "" {
		path = "."
	}
	detail := "code: " + path
	if *ci {
		detail += "  [ci]"
	}
	if *detach {
		detail += "  [detached]"
	}
	return engineTODO("deploy", ctx, detail)
}
