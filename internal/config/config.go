// Package config renders the resolved view behind `conductor config`: the
// identity a directory is aimed at (from internal/link) plus the parsed
// conductor.toml spec (from internal/deployspec). It is read-only — it loads and
// formats, never mutating the control plane or any files.
package config

import (
	"fmt"
	"sort"
	"strings"

	"conductor/internal/deployspec"
	"conductor/internal/link"
	"conductor/internal/target"
)

// Render returns the full `conductor config` report. A parse/validation error
// in the spec is returned; a missing link or missing spec is reported inline,
// not as an error. The identity is the resolved (project, environment, service)
// the caller already worked out from flags/env/link.
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

	spec, path, ok, err := deployspec.Load()
	if err != nil {
		return "", err
	}
	b.WriteString("\nSpec\n")
	if !ok {
		b.WriteString("  - (no conductor.toml found)\n")
		return b.String(), nil
	}
	fmt.Fprintf(&b, "  file: %s\n", path)
	writeServices(&b, spec, id.Service)
	noteIfServiceUnspecified(&b, spec, id.Service)
	return b.String(), nil
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// writeServices marks the selected service so the reader sees which block `up`
// would target.
func writeServices(b *strings.Builder, spec deployspec.Spec, selected string) {
	names := make([]string, 0, len(spec.Services))
	for name := range spec.Services {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		marker := ""
		if name == selected {
			marker = "  (selected)"
		}
		fmt.Fprintf(b, "  [services.%s]%s\n", name, marker)
		writeServiceBody(b, spec.Services[name])
	}
}

func writeServiceBody(b *strings.Builder, s deployspec.Service) {
	writeField(b, "builder", s.Builder)
	writeField(b, "dockerfile", s.Dockerfile)
	writeField(b, "build_command", s.BuildCommand)
	writeField(b, "start_command", s.StartCommand)
	writeField(b, "restart_policy", s.RestartPolicy)
	if s.RestartMaxRetries != 0 {
		fmt.Fprintf(b, "    restart_max_retries: %d\n", s.RestartMaxRetries)
	}
	if s.DrainSeconds != 0 {
		fmt.Fprintf(b, "    drain_seconds: %d\n", s.DrainSeconds)
	}
	if s.Healthcheck != nil {
		writeField(b, "healthcheck.path", s.Healthcheck.Path)
		if s.Healthcheck.TimeoutSeconds != 0 {
			fmt.Fprintf(b, "    healthcheck.timeout_seconds: %d\n", s.Healthcheck.TimeoutSeconds)
		}
	}
	if s.Resources != nil {
		writeField(b, "resources.cpu", s.Resources.CPU)
		writeField(b, "resources.memory", s.Resources.Memory)
	}
}

func writeField(b *strings.Builder, key, val string) {
	if val == "" {
		return
	}
	fmt.Fprintf(b, "    %s: %s\n", key, val)
}

// noteIfServiceUnspecified flags a selected service that has no [services.NAME]
// block. This is legal under Camp B — the service lives in the DB and just has
// no build/deploy overrides — but the note catches the far more common cause: a
// typo in the link's service pointer.
func noteIfServiceUnspecified(b *strings.Builder, spec deployspec.Spec, selected string) {
	if selected == "" {
		return
	}
	if _, ok := spec.Services[selected]; ok {
		return
	}
	fmt.Fprintf(b, "  note: selected service %q has no [services.%s] block — it will deploy with defaults (or check for a typo)\n", selected, selected)
}
