// Package deployspec parses and validates conductor.toml: the committed,
// per-repo build/deploy spec for a project's services. It is the "how to build
// and run" half of the model — the identity half (which project/env/service)
// lives in internal/link.
//
// The spec AUGMENTS services that already exist in the control-plane DB; it
// never owns their existence or replica counts (those are imperative DB writes
// via add/scale/down). The hard contract — enforced at load time by rejecting
// unknown keys — is build/deploy settings ONLY: no environment/service
// selection, no secret VALUES, no authoritative replica topology.
package deployspec

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// FileName is the committed spec file, discovered by walking up to the repo root.
const FileName = "conductor.toml"

// ErrNotFound is reported by callers that require a spec; Load itself treats a
// missing file as a valid state (ok=false), since the spec only augments the DB.
// Kept exported for symmetry with internal/link.
var ErrNotFound = fmt.Errorf("no %s found in this directory or any parent", FileName)

// Spec is the whole conductor.toml: a set of [services.NAME] blocks.
type Spec struct {
	Services map[string]Service `toml:"services"`
}

// Service is one [services.NAME] block: how to build and run that service.
// cpu/mem under [resources] are an INITIAL scaffold hint for the first deploy,
// not authoritative — `scale` owns live resourcing in the DB.
type Service struct {
	Builder      string `toml:"builder"`       // nixpacks | dockerfile | buildpacks
	Dockerfile   string `toml:"dockerfile"`    // path, when builder = dockerfile
	BuildCommand string `toml:"build_command"` // overrides the builder's default
	StartCommand string `toml:"start_command"` // process to run in the container

	Healthcheck *Healthcheck `toml:"healthcheck"`

	RestartPolicy     string `toml:"restart_policy"`      // never | on-failure | always
	RestartMaxRetries int    `toml:"restart_max_retries"` // cap for on-failure
	DrainSeconds      int    `toml:"drain_seconds"`       // graceful-shutdown window

	Resources *Resources `toml:"resources"`
}

type Healthcheck struct {
	Path           string `toml:"path"`
	TimeoutSeconds int    `toml:"timeout_seconds"`
}

// Resources is an initial scaffold for a service's first deploy. Authoritative
// sizing lives in the DB and is changed with `scale`.
type Resources struct {
	CPU    string `toml:"cpu"`
	Memory string `toml:"memory"`
}

var validBuilders = map[string]bool{"nixpacks": true, "dockerfile": true, "buildpacks": true}

var validRestartPolicies = map[string]bool{"never": true, "on-failure": true, "always": true}

// Find walks up from cwd looking for conductor.toml, returning its path.
// ok=false means none exists between cwd and the filesystem root.
func Find() (path string, ok bool, err error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", false, err
	}
	for {
		candidate := filepath.Join(cwd, FileName)
		if _, statErr := os.Stat(candidate); statErr == nil {
			return candidate, true, nil
		}
		parent := filepath.Dir(cwd)
		if parent == cwd {
			return "", false, nil
		}
		cwd = parent
	}
}

// Load finds, parses, and validates the nearest conductor.toml. ok=false (with a
// nil error) means no spec exists — a valid state, since the spec only augments
// the DB. A present-but-invalid spec is an error.
func Load() (s Spec, path string, ok bool, err error) {
	path, ok, err = Find()
	if err != nil || !ok {
		return Spec{}, "", ok, err
	}
	md, err := toml.DecodeFile(path, &s)
	if err != nil {
		return Spec{}, path, true, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := rejectUnknownKeys(path, md); err != nil {
		return Spec{}, path, true, err
	}
	if err := s.validate(path); err != nil {
		return Spec{}, path, true, err
	}
	return s, path, true, nil
}

// rejectUnknownKeys turns any key the schema does not model into an error. This
// is the enforcement arm of the spec contract: it stops replica counts, secret
// values, or selection pointers from being smuggled in and silently dropped.
func rejectUnknownKeys(path string, md toml.MetaData) error {
	undecoded := md.Undecoded()
	if len(undecoded) == 0 {
		return nil
	}
	keys := make([]string, len(undecoded))
	for i, k := range undecoded {
		keys[i] = k.String()
	}
	sort.Strings(keys)
	return fmt.Errorf("%s: unknown keys (build/deploy config only — no topology or secret values): %s",
		path, strings.Join(keys, ", "))
}

func (s Spec) validate(path string) error {
	for name, svc := range s.Services {
		if err := svc.validate(); err != nil {
			return fmt.Errorf("%s: [services.%s]: %w", path, name, err)
		}
	}
	return nil
}

func (s Service) validate() error {
	if s.Builder != "" && !validBuilders[s.Builder] {
		return fmt.Errorf("builder %q is not one of nixpacks, dockerfile, buildpacks", s.Builder)
	}
	if s.Builder == "dockerfile" && s.Dockerfile == "" {
		return fmt.Errorf(`builder "dockerfile" requires a dockerfile path`)
	}
	if s.RestartPolicy != "" && !validRestartPolicies[s.RestartPolicy] {
		return fmt.Errorf("restart_policy %q is not one of never, on-failure, always", s.RestartPolicy)
	}
	if s.RestartMaxRetries < 0 {
		return fmt.Errorf("restart_max_retries must be >= 0")
	}
	if s.DrainSeconds < 0 {
		return fmt.Errorf("drain_seconds must be >= 0")
	}
	if s.Healthcheck != nil && s.Healthcheck.TimeoutSeconds < 0 {
		return fmt.Errorf("healthcheck.timeout_seconds must be >= 0")
	}
	return nil
}
