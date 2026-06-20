package cmd

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"conductor/internal/project"
	"conductor/internal/storage"
)

const scaleUsage = `conductor scale <region=count> [region=count ...]

Patch the desired per-region replica counts for a service. Each argument is a
region=count pair, e.g. "us-east-1=3 eu-west-1=2". The reconcile loop bin-packs
the new counts onto the fleet. Requires project, environment, and service, and
the service must already be deployed (run 'conductor up' first).`

func cmdScale(args []string) error {
	// region=count pairs are variadic positionals, but stdlib flag stops parsing
	// at the first positional — so `scale us-east-1=3 -s web` would drop -s. Pull
	// the pairs out first (a bare token containing '='; flag values here are
	// names, never '='-bearing) so flags and pairs work in any order.
	pairs, flagArgs := partitionRegionCounts(args)

	fs := newFlagSet("scale", scaleUsage)
	var t Target
	addTargetFlags(fs, &t)
	if err := fs.parse(flagArgs); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return usageErr(scaleUsage, fmt.Sprintf("invalid region=count pair %q", fs.Arg(0)))
	}
	resolve(&t, true)
	if err := t.require(true, true); err != nil {
		return usageErr(scaleUsage, err.Error())
	}
	if len(pairs) == 0 {
		return usageErr(scaleUsage, "at least one region=count pair is required")
	}

	replicas, err := project.ParseRegionCounts(pairs)
	if err != nil {
		return usageErr(scaleUsage, err.Error())
	}

	ctx := context.Background()
	store, err := storage.NewPostgresClient(ctx, databaseURL())
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = store.Close() }()

	if err := project.New(store).Scale(ctx, project.ScaleInput{Target: t.Target, Replicas: replicas}); err != nil {
		return err
	}

	fmt.Printf("→ scaled %s\n  %s\n", t, formatRegionCounts(replicas))
	return nil
}

// partitionRegionCounts splits argv into region=count pairs and everything else
// (flags and their values). A token is a pair when it does not start with '-'
// and contains '='; flag values in this command are project/env/service names,
// which never contain '=', so they stay with the flags.
func partitionRegionCounts(args []string) (pairs, flagArgs []string) {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") && strings.Contains(a, "=") {
			pairs = append(pairs, a)
			continue
		}
		flagArgs = append(flagArgs, a)
	}
	return pairs, flagArgs
}

// formatRegionCounts renders the patched counts in a stable (sorted) order so
// the echoed line is deterministic regardless of map iteration order.
func formatRegionCounts(replicas map[string]int32) string {
	regions := make([]string, 0, len(replicas))
	for r := range replicas {
		regions = append(regions, r)
	}
	sort.Strings(regions)

	pairs := make([]string, len(regions))
	for i, r := range regions {
		pairs[i] = fmt.Sprintf("%s=%d", r, replicas[r])
	}
	return "replicas: " + strings.Join(pairs, " ")
}
