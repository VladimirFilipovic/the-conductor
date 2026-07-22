// Package spec renders the resolved view behind `conductor config`: the identity
// a directory is aimed at (from internal/link) plus the parsed config.toml
// build/deploy spec (from internal/deployspec). It is read-only — it loads and
// formats, never mutating the control plane or any files.
package spec

import (
	"fmt"
	"os"
	"strings"

	"conductor/internal/deployspec"
	"conductor/internal/link"
	"conductor/internal/target"
)

// Render returns the full `conductor config` report. A parse/validation error in
// the spec is returned; a missing link or missing spec is reported inline, not as
// an error.
func Render(id target.Target) (string, error) {
	var b strings.Builder

	b.WriteString("Identity\n")
	fmt.Fprintf(&b, "  project:     %s\n", dash(id.Project))
	fmt.Fprintf(&b, "  environment: %s\n", dash(id.Environment))
	fmt.Fprintf(&b, "  service:     %s\n", dash(id.Service))
	if _, dir, ok, err := link.Load(); err == nil && ok {
		fmt.Fprintf(&b, "  link file:   %s\n", link.Path(dir))
	} else {
		b.WriteString("  link file:   - (run `conductor link` to create one)\n")
	}

	// Same path `up` reads (current directory, no walk-up), so `config`
	// previews exactly what `up` would act on from here.
	path := deployspec.FileName
	b.WriteString("\nSpec\n")
	if _, err := os.Stat(path); err != nil {
		b.WriteString("  - (no config.toml in this directory)\n")
		return b.String(), nil
	}
	spec, err := deployspec.Load(path)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(&b, "  file: %s\n", path)
	// Resolve for the active environment so the reader sees what `up` would commit.
	bld, dep := spec.Resolve(id.Environment)
	writeBuild(&b, bld)
	writeDeploy(&b, dep)
	return b.String(), nil
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func writeBuild(b *strings.Builder, bld deployspec.Build) {
	b.WriteString("  [build]\n")
	writeField(b, "builder", orAuto(bld.Builder))
	writeField(b, "dockerfile", bld.Dockerfile)
	writeField(b, "build_command", bld.BuildCommand)
	writeField(b, "root", bld.Root)
	if len(bld.WatchPatterns) > 0 {
		writeField(b, "watch_patterns", strings.Join(bld.WatchPatterns, ", "))
	}
}

func writeDeploy(b *strings.Builder, dep deployspec.Deploy) {
	b.WriteString("  [deploy]\n")
	writeField(b, "start_command", dep.StartCommand)
	fmt.Fprintf(b, "    num_replicas: %d\n", dep.ReplicasOrDefault())
	writeField(b, "region", dep.RegionOrDefault())
	writeField(b, "healthcheck_path", dep.HealthcheckPath)
	if dep.HealthcheckTimeout != 0 {
		fmt.Fprintf(b, "    healthcheck_timeout: %d\n", dep.HealthcheckTimeout)
	}
	writeField(b, "restart_policy", dep.RestartPolicy)
	fmt.Fprintf(b, "    restart_max_retries: %d\n", dep.RestartMaxOrDefault())
	fmt.Fprintf(b, "    drain_seconds: %d\n", dep.DrainSecondsOrDefault())
	fmt.Fprintf(b, "    cpu_millicores: %d\n", dep.CPUMillicores())
	fmt.Fprintf(b, "    mem_bytes: %d\n", dep.MemBytes())
}

func writeField(b *strings.Builder, key, val string) {
	if val == "" {
		return
	}
	fmt.Fprintf(b, "    %s: %s\n", key, val)
}

func orAuto(builder string) string {
	if builder == "" {
		return "(autodetect)"
	}
	return builder
}
