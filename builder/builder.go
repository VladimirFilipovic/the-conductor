// Package builder is conductor's source-to-image extension point. `conductor up`
// commits desired state from a manifest; whenever a service ships a repo instead
// of a prebuilt image, up asks a Builder to turn that source into an image
// reference, and the returned ref becomes the deployment's image_ref.
//
// The library ships NoOp, which runs no build and synthesizes a deterministic
// local ref — enough to exercise the control plane with no build pipeline wired.
// Embedders implement Builder to shell out to nixpacks, a Dockerfile build,
// buildpacks, or their own system. This package is intentionally public (not
// under internal/) so it can be imported and implemented from outside the
// module.
package builder

import (
	"context"
	"fmt"
	"strings"
)

// Strategy names a build backend. It is a hint a Builder may honor or ignore;
// the values mirror a Railway-style build config so an embedder can map a
// Request straight onto railpack/nixpacks without a translation layer.
type Strategy string

const (
	StrategyAuto       Strategy = "" // let the backend detect (nixpacks-style autodetection)
	StrategyNixpacks   Strategy = "nixpacks"
	StrategyDockerfile Strategy = "dockerfile"
	StrategyBuildpacks Strategy = "buildpacks"
)

// Request is everything a backend needs to turn source into an image. It stays
// close to a Railway build config so implementations stay thin.
type Request struct {
	Project      string            // control-plane project the build belongs to
	Environment  string            // target environment
	Service      string            // service being built
	Repo         string            // source repo URL or local path
	Root         string            // sub-directory to build from (monorepos); "" = repo root
	Strategy     Strategy          // backend hint
	Dockerfile   string            // path, when Strategy == StrategyDockerfile
	BuildCommand string            // overrides the backend's default build command
	Env          map[string]string // variables available at build time (build args)
}

// Result is what a build produces. ImageRef is required — it is written
// verbatim as the deployment's image_ref. Digest is an optional immutable pin;
// Logs is free-form build output an embedder may surface to the user.
type Result struct {
	ImageRef string
	Digest   string
	Logs     string
}

// Builder turns source (Request) into an image (Result). Implementations must
// return a non-empty Result.ImageRef on success and should be safe for
// concurrent use (up may build several services in one invocation).
type Builder interface {
	Build(ctx context.Context, req Request) (Result, error)
}

// Resolve turns a service's source into the image ref to deploy. A prebuilt
// image (a service shipping an image, or a database's managed image) is used
// as-is; otherwise the repo in req is handed to b and the built ref is returned.
// A service with neither, or a builder that yields an empty ref, is an error.
// This is the build-vs-prebuilt decision lifted out of the CLI so `up` stays
// pure wiring.
func Resolve(ctx context.Context, b Builder, req Request, prebuilt string) (string, error) {
	if prebuilt != "" {
		return prebuilt, nil
	}
	if req.Repo == "" {
		return "", fmt.Errorf("service %q has no source (image or repo) to deploy", req.Service)
	}
	res, err := b.Build(ctx, req)
	if err != nil {
		return "", fmt.Errorf("build %s: %w", req.Repo, err)
	}
	if res.ImageRef == "" {
		return "", fmt.Errorf("builder returned an empty image ref for %s", req.Repo)
	}
	return res.ImageRef, nil
}

// NoOp is the default Builder: it performs no build and returns a deterministic,
// obviously-local image ref derived from the project and service. It lets up
// commit desired state for repo-based services before any real build pipeline
// exists; the conductor.local/ prefix makes clear the artifact was never pushed.
type NoOp struct{}

// Build returns a synthesized ref like "conductor.local/storefront/web:source".
func (NoOp) Build(_ context.Context, req Request) (Result, error) {
	return Result{ImageRef: fmt.Sprintf("conductor.local/%s/%s:source", slug(req.Project), slug(req.Service))}, nil
}

// slug keeps the synthesized ref a valid-ish image path: lowercased, with runs
// of anything outside [a-z0-9-] collapsed to a single dash.
func slug(s string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
			prevDash = r == '-'
			continue
		}
		if !prevDash {
			b.WriteByte('-')
			prevDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "service"
	}
	return out
}
