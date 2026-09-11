package project

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"conductor/internal/deployspec"
	"conductor/internal/storage"
	"conductor/internal/target"
)

func ParseRegionCounts(args []string) (map[string]int32, error) {
	replicas := make(map[string]int32, len(args))
	for _, a := range args {
		region, countStr, ok := strings.Cut(a, "=")
		if !ok || region == "" {
			return nil, fmt.Errorf("invalid region=count pair %q", a)
		}
		count, err := strconv.Atoi(countStr)
		if err != nil {
			return nil, fmt.Errorf("count for %q must be an integer", region)
		}
		if count < 0 || count > deployspec.MaxReplicas {
			return nil, fmt.Errorf("count for %q must be between 0 and %d", region, deployspec.MaxReplicas)
		}
		if _, dup := replicas[region]; dup {
			return nil, fmt.Errorf("region %q given more than once", region)
		}
		replicas[region] = int32(count)
	}
	return replicas, nil
}

const maxStatefulReplicas = 1

type ScaleInput struct {
	target.Target
	Replicas map[string]int32
}

// Scale upserts desired replica counts per region on the active deployment, in
// one tx so a multi-region patch is all-or-nothing. An undeployed service
// surfaces as storage.ErrNotFound (run `up` first).
func (s *Service) Scale(ctx context.Context, in ScaleInput) error {
	// Spans both domains: statefulness lives on the service, replica counts on
	// the deployment, so the body narrows the Querier per step instead of
	// typing itself to one slice.
	return s.store.WithTx(ctx, func(st storage.Querier) error {
		if err := checkStatefulScale(ctx, st, in); err != nil {
			return err
		}
		return applyRegionCounts(ctx, st, in)
	})
}

func checkStatefulScale(ctx context.Context, st ProjectStore, in ScaleInput) error {
	svc, err := st.GetService(ctx, in.Project, in.Service)
	if err != nil {
		return err
	}
	if !svc.Stateful {
		return nil
	}
	if total := totalReplicas(in.Replicas); total > maxStatefulReplicas {
		return fmt.Errorf("%w: service %q is stateful and runs a single instance; total replicas must be <= %d, not %d", ErrInvalid, in.Service, maxStatefulReplicas, total)
	}
	return nil
}

func applyRegionCounts(ctx context.Context, st DeploymentStore, in ScaleInput) error {
	depID, err := st.CurrentDeploymentID(ctx, in.Project, in.Environment, in.Service)
	if err != nil {
		return err
	}
	// Sorted so concurrent multi-region patches take row locks in one order.
	for _, region := range sortedRegions(in.Replicas) {
		if err := st.SetDeploymentRegion(ctx, depID, region, in.Replicas[region]); err != nil {
			return err
		}
	}
	return nil
}

// Down zeroes every region of the current deployment — stops compute, leaves
// the deployment (and its volumes) intact. Like Scale it needs an active deployment.
func (s *Service) Down(ctx context.Context, t target.Target) error {
	return s.store.WithTx(ctx, func(st storage.Querier) error { return down(ctx, st, t) })
}

func down(ctx context.Context, st DeploymentStore, t target.Target) error {
	depID, err := st.CurrentDeploymentID(ctx, t.Project, t.Environment, t.Service)
	if err != nil {
		return err
	}
	return st.ZeroDeploymentRegions(ctx, depID)
}
