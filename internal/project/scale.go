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
	// CPUMillicores / MemBytes, when non-zero and different from the current
	// commit, turn the scale into a resize: limits live on the immutable
	// deployment, so new ones mean a new version (blue/green), the way a
	// Railway limit change redeploys. Zero keeps the current value.
	CPUMillicores int32
	MemBytes      int64
	CreatedBy     string
}

// ScaleResult's Version is the commit a resize created; 0 means the counts
// were patched in place on the current deployment and nothing restarted.
type ScaleResult struct {
	Version int32
}

// Scale upserts desired replica counts per region on the active deployment, in
// one tx so a multi-region patch is all-or-nothing. With new limits it instead
// re-commits the current spec under them, carrying the merged counts. An
// undeployed service surfaces as storage.ErrNotFound (run `up` first).
func (s *Service) Scale(ctx context.Context, in ScaleInput) (ScaleResult, error) {
	// Spans both domains: statefulness lives on the service, replica counts on
	// the deployment, so the body narrows the Querier per step instead of
	// typing itself to one slice.
	var res ScaleResult
	err := s.store.WithTx(ctx, func(st storage.Querier) error {
		if err := checkStatefulScale(ctx, st, in); err != nil {
			return err
		}
		if in.CPUMillicores == 0 && in.MemBytes == 0 {
			return applyRegionCounts(ctx, st, in)
		}
		var err error
		res, err = resize(ctx, st, in)
		return err
	})
	return res, err
}

// resize commits the current deployment again with the new limits. Everything
// else — image, env, healthcheck, rollout knobs — is copied verbatim, and the
// region counts are the current ones patched by in.Replicas, exactly what an
// in-place scale would have left behind.
func resize(ctx context.Context, st DeploymentStore, in ScaleInput) (ScaleResult, error) {
	es, err := st.GetEnvironmentService(ctx, in.Project, in.Environment, in.Service)
	if err != nil {
		return ScaleResult{}, err
	}
	cur, err := st.GetCurrentDeploymentSpec(ctx, es.ID)
	if err != nil {
		return ScaleResult{}, err
	}
	cpu, mem := orCurrent(in.CPUMillicores, cur.CpuMillicores), orCurrent(in.MemBytes, cur.MemBytes)
	if cpu == cur.CpuMillicores && mem == cur.MemBytes {
		return ScaleResult{}, applyRegionCounts(ctx, st, in)
	}

	regions, err := st.ListDeploymentRegions(ctx, cur.ID)
	if err != nil {
		return ScaleResult{}, err
	}
	replicas := make(map[string]int32, len(regions)+len(in.Replicas))
	for _, r := range regions {
		replicas[r.Region] = r.Replicas
	}
	for region, n := range in.Replicas {
		replicas[region] = n
	}

	out, err := deploy(ctx, st, DeployInput{
		Target:           in.Target,
		ImageRef:         cur.ImageRef,
		CPUMillicores:    cpu,
		MemBytes:         mem,
		Env:              cur.Env,
		Healthcheck:      cur.Healthcheck,
		DrainSeconds:     cur.DrainSeconds,
		RestartMax:       cur.RestartMax,
		ProgressDeadline: cur.ProgressDeadline,
		Replicas:         replicas,
		CommitMessage:    fmt.Sprintf("resize v%d: cpu %dm→%dm, mem %d→%d MiB", cur.Version, cur.CpuMillicores, cpu, cur.MemBytes>>20, mem>>20),
		CreatedBy:        in.CreatedBy,
	})
	if err != nil {
		return ScaleResult{}, err
	}
	return ScaleResult{Version: out.Version}, nil
}

func orCurrent[T int32 | int64](v, current T) T {
	if v == 0 {
		return current
	}
	return v
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
