package cmd

import (
	"fmt"
	"strconv"
	"strings"
)

const scaleUsage = `conductor scale <region=count> [region=count ...]

Patch the desired per-region replica counts for a service. Each argument is a
region=count pair, e.g. "us-west1=3 eu-west=2". The reconcile loop bin-packs
the new counts onto the fleet. Requires project, environment, and service.`

func cmdScale(args []string) error {
	rest, ctx := extractTarget(args)
	fs := newFlagSet("scale", scaleUsage)
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if err := ctx.require(true, true, true); err != nil {
		return usageErr(scaleUsage, err.Error())
	}
	if fs.NArg() == 0 {
		return usageErr(scaleUsage, "at least one region=count pair is required")
	}

	pairs := make([]string, 0, fs.NArg())
	for _, a := range fs.Args() {
		region, countStr, ok := strings.Cut(a, "=")
		if !ok || region == "" {
			return usageErr(scaleUsage, fmt.Sprintf("invalid region=count pair %q", a))
		}
		if _, err := strconv.Atoi(countStr); err != nil {
			return usageErr(scaleUsage, fmt.Sprintf("count for %q must be an integer", region))
		}
		pairs = append(pairs, a)
	}
	return engineTODO("scale", ctx, "replicas: "+strings.Join(pairs, " "))
}
