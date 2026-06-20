// Package deployspec parses config.toml: the committed, single-service build and
// deploy spec `conductor up` reads. Its shape borrows from Railway's railway.toml
// for the fields Railway actually exposes (numReplicas, region, healthcheck,
// restart policy, start command); the cpu/memory sizing fields are a Kubernetes-
// style addition — Railway itself has no per-service CPU/mem knob (it autoscales
// to a plan cap and bills by usage). It holds build/deploy SETTINGS only — never
// identity (which project/environment/service this deploys to comes from the
// folder link or -p/-e/-s) and never the service's source (the repo/image is
// recorded in the control plane by `add`). Per-environment overrides live in
// [environments.NAME.*] blocks, merged over the base for the active environment.
package deployspec

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// FileName is the conventional spec path `conductor up` reads when none is given.
const FileName = "config.toml"

// Spec is the whole config.toml for one service: base [build]/[deploy] plus any
// per-environment overrides.
type Spec struct {
	Build        Build                  `toml:"build"`
	Deploy       Deploy                 `toml:"deploy"`
	Environments map[string]EnvOverride `toml:"environments"`
}

// Build is how the service's source is turned into an image (the build.Builder
// seam consumes these). Empty builder means autodetect.
type Build struct {
	Builder       string   `toml:"builder"`        // nixpacks | dockerfile | buildpacks; "" = autodetect
	Dockerfile    string   `toml:"dockerfile"`     // path, required when builder = dockerfile
	BuildCommand  string   `toml:"build_command"`  // overrides the builder's default
	Root          string   `toml:"root"`           // sub-directory to build from (monorepos)
	WatchPatterns []string `toml:"watch_patterns"` // paths whose changes trigger a rebuild
}

// Deploy is the committed runtime intent: image sizing, health, restart, and the
// replica count/region the reconcile loop converges to. num_replicas + region
// mirror Railway's numReplicas (Railway picks the region; here the fleet is
// region-keyed, so a single region is named, defaulting to DefaultRegion).
type Deploy struct {
	StartCommand string `toml:"start_command"`
	// Pointer so an explicit num_replicas = 0 (commit a deploy that runs zero
	// replicas) is distinguishable from the field being omitted (nil → default).
	NumReplicas        *int   `toml:"num_replicas"`
	Region             string `toml:"region"`
	HealthcheckPath    string `toml:"healthcheck_path"`
	HealthcheckTimeout int    `toml:"healthcheck_timeout"`
	RestartPolicy      string `toml:"restart_policy"`      // never | on-failure | always
	RestartMaxRetries  int    `toml:"restart_max_retries"` // cap for on-failure
	DrainSeconds       int    `toml:"drain_seconds"`       // graceful-shutdown window
	CPU                string `toml:"cpu"`                 // millicores ("500m") or cores ("1", "1.5")
	Memory             string `toml:"memory"`              // binary units: "512Mi", "2Gi", or bytes
}

// EnvOverride is an [environments.NAME] block: a partial Build/Deploy whose set
// fields win over the base when that environment is the deploy target.
type EnvOverride struct {
	Build  Build  `toml:"build"`
	Deploy Deploy `toml:"deploy"`
}

// DefaultRegion is where replicas land when [deploy].region is omitted. It is one
// of the seeded fleet regions (db/seeds/hosts.sql) so a deploy has hosts to land on.
const DefaultRegion = "us-east-1"

const (
	DefaultNumReplicas  = 1
	DefaultDrainSeconds = 30
	DefaultRestartMax   = 5

	// MaxReplicas caps num_replicas (and `scale`) so a typo can't request an
	// absurd fleet; the reconcile loop would otherwise try to place every one.
	MaxReplicas = 50
)

var validBuilders = map[string]bool{"nixpacks": true, "dockerfile": true, "buildpacks": true}

var validRestartPolicies = map[string]bool{"never": true, "on-failure": true, "always": true}

// Load reads, parses, and validates the spec at path. Unknown keys are rejected
// so a typo or a smuggled-in setting fails loudly rather than being dropped.
func Load(path string) (Spec, error) {
	var s Spec
	md, err := toml.DecodeFile(path, &s)
	if err != nil {
		return Spec{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := rejectUnknownKeys(path, md); err != nil {
		return Spec{}, err
	}
	if err := s.validate(path); err != nil {
		return Spec{}, err
	}
	return s, nil
}

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
	return fmt.Errorf("%s: unknown keys: %s", path, strings.Join(keys, ", "))
}

func (s Spec) validate(path string) error {
	if err := s.Build.validate(); err != nil {
		return fmt.Errorf("%s: [build]: %w", path, err)
	}
	if err := s.Deploy.validate(); err != nil {
		return fmt.Errorf("%s: [deploy]: %w", path, err)
	}
	for name, ov := range s.Environments {
		if err := ov.Build.validate(); err != nil {
			return fmt.Errorf("%s: [environments.%s.build]: %w", path, name, err)
		}
		if err := ov.Deploy.validate(); err != nil {
			return fmt.Errorf("%s: [environments.%s.deploy]: %w", path, name, err)
		}
	}
	return nil
}

func (b Build) validate() error {
	if b.Builder != "" && !validBuilders[b.Builder] {
		return fmt.Errorf("builder %q is not one of nixpacks, dockerfile, buildpacks", b.Builder)
	}
	if b.Builder == "dockerfile" && b.Dockerfile == "" {
		return fmt.Errorf(`builder "dockerfile" requires a dockerfile path`)
	}
	return nil
}

func (d Deploy) validate() error {
	if d.RestartPolicy != "" && !validRestartPolicies[d.RestartPolicy] {
		return fmt.Errorf("restart_policy %q is not one of never, on-failure, always", d.RestartPolicy)
	}
	if d.NumReplicas != nil && (*d.NumReplicas < 0 || *d.NumReplicas > MaxReplicas) {
		return fmt.Errorf("num_replicas must be between 0 and %d", MaxReplicas)
	}
	if d.RestartMaxRetries < 0 {
		return fmt.Errorf("restart_max_retries must be >= 0")
	}
	if d.DrainSeconds < 0 {
		return fmt.Errorf("drain_seconds must be >= 0")
	}
	if d.HealthcheckTimeout < 0 {
		return fmt.Errorf("healthcheck_timeout must be >= 0")
	}
	if _, err := parseCPU(d.CPU); err != nil {
		return err
	}
	if _, err := parseMem(d.Memory); err != nil {
		return err
	}
	return nil
}

// Resolve merges the [environments.NAME] override for env over the base spec and
// returns the effective Build/Deploy to deploy. A field set in the override wins;
// an unset field keeps the base value.
func (s Spec) Resolve(env string) (Build, Deploy) {
	build, deploy := s.Build, s.Deploy
	if ov, ok := s.Environments[env]; ok {
		build = build.merge(ov.Build)
		deploy = deploy.merge(ov.Deploy)
	}
	return build, deploy
}

func (b Build) merge(ov Build) Build {
	out := b
	out.Builder = orStr(ov.Builder, out.Builder)
	out.Dockerfile = orStr(ov.Dockerfile, out.Dockerfile)
	out.BuildCommand = orStr(ov.BuildCommand, out.BuildCommand)
	out.Root = orStr(ov.Root, out.Root)
	if ov.WatchPatterns != nil {
		out.WatchPatterns = ov.WatchPatterns
	}
	return out
}

// HealthcheckJSON renders the deploy's health check as the deployments.healthcheck
// jsonb payload, or nil when no path is set (the engine treats absent as "none").
func (d Deploy) HealthcheckJSON() json.RawMessage {
	if d.HealthcheckPath == "" {
		return nil
	}
	return json.RawMessage(fmt.Appendf(nil, `{"path":%q,"timeout_s":%d}`, d.HealthcheckPath, d.HealthcheckTimeout))
}

func (d Deploy) merge(ov Deploy) Deploy {
	out := d
	out.StartCommand = orStr(ov.StartCommand, out.StartCommand)
	if ov.NumReplicas != nil {
		out.NumReplicas = ov.NumReplicas
	}
	out.Region = orStr(ov.Region, out.Region)
	out.HealthcheckPath = orStr(ov.HealthcheckPath, out.HealthcheckPath)
	out.HealthcheckTimeout = orInt(ov.HealthcheckTimeout, out.HealthcheckTimeout)
	out.RestartPolicy = orStr(ov.RestartPolicy, out.RestartPolicy)
	out.RestartMaxRetries = orInt(ov.RestartMaxRetries, out.RestartMaxRetries)
	out.DrainSeconds = orInt(ov.DrainSeconds, out.DrainSeconds)
	out.CPU = orStr(ov.CPU, out.CPU)
	out.Memory = orStr(ov.Memory, out.Memory)
	return out
}

// Region returns the deploy's target region, or DefaultRegion when unset.
func (d Deploy) RegionOrDefault() string { return orStr(d.Region, DefaultRegion) }

// ReplicasOrDefault returns the desired replica count, defaulting to
// DefaultNumReplicas only when num_replicas was omitted (nil). An explicit 0 is
// honored — it commits a deploy with zero running replicas (like `down`).
func (d Deploy) ReplicasOrDefault() int32 {
	if d.NumReplicas == nil {
		return DefaultNumReplicas
	}
	return int32(*d.NumReplicas)
}

func (d Deploy) DrainSecondsOrDefault() int32 {
	return orDefaultInt32(int32(d.DrainSeconds), DefaultDrainSeconds)
}

func (d Deploy) RestartMaxOrDefault() int32 {
	return orDefaultInt32(int32(d.RestartMaxRetries), DefaultRestartMax)
}

const (
	defaultCPUMillicores int32 = 250
	defaultMemBytes      int64 = 256 << 20 // 256Mi
)

// CPUMillicores parses [deploy].cpu, defaulting to 250m. See parseCPU.
func (d Deploy) CPUMillicores() int32 {
	v, _ := parseCPU(d.CPU) // validated at Load time
	return v
}

// MemBytes parses [deploy].memory, defaulting to 256Mi. See parseMem.
func (d Deploy) MemBytes() int64 {
	v, _ := parseMem(d.Memory) // validated at Load time
	return v
}

// parseCPU turns a cpu string into millicores. A bare number is cores ("1" →
// 1000m, "1.5" → 1500m); an "m" suffix is millicores ("500m" → 500). Empty
// defaults to 250m. Result must be > 0 (DB CHECK).
func parseCPU(raw string) (int32, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultCPUMillicores, nil
	}
	if milli, ok := strings.CutSuffix(raw, "m"); ok {
		n, err := strconv.Atoi(strings.TrimSpace(milli))
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("cpu %q is not a positive millicore value", raw)
		}
		return int32(n), nil
	}
	milli, ok := coresToMillicores(raw)
	if !ok || milli <= 0 {
		return 0, fmt.Errorf("cpu %q is not a positive core count", raw)
	}
	return milli, nil
}

// coresToMillicores converts a decimal core count ("1", "1.5", "0.25") to
// millicores with exact integer math. Floats are wrong here: 0.29*1000 is
// 289.999…, which truncates to 289m, and sub-millicore values truncate to 0m —
// both would slip past validation and only fail the DB's cpu_millicores > 0
// CHECK at deploy. Precision finer than a millicore (a non-zero 4th fractional
// digit) is rejected rather than silently dropped.
func coresToMillicores(raw string) (millicores int32, ok bool) {
	whole, frac, hasFrac := strings.Cut(raw, ".")
	w := 0
	if whole != "" {
		var err error
		if w, err = strconv.Atoi(whole); err != nil || w < 0 {
			return 0, false
		}
	}
	milli := w * 1000
	if hasFrac {
		frac = strings.TrimRight(frac, "0")
		if len(frac) > 3 {
			return 0, false
		}
		if frac != "" {
			f, err := strconv.Atoi(frac + strings.Repeat("0", 3-len(frac)))
			if err != nil || f < 0 {
				return 0, false
			}
			milli += f
		}
	}
	return int32(milli), true
}

var memUnits = []struct {
	suffix string
	mult   int64
}{
	{"Gi", 1 << 30},
	{"Mi", 1 << 20},
	{"Ki", 1 << 10},
}

// parseMem turns a memory string into bytes. Binary units Ki/Mi/Gi or a bare
// byte count are accepted. Empty defaults to 256Mi. Result must be > 0.
func parseMem(raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultMemBytes, nil
	}
	for _, u := range memUnits {
		if num, ok := strings.CutSuffix(raw, u.suffix); ok {
			n, err := strconv.ParseInt(strings.TrimSpace(num), 10, 64)
			if err != nil || n <= 0 {
				return 0, fmt.Errorf("memory %q is not a positive %s value", raw, u.suffix)
			}
			return n * u.mult, nil
		}
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("memory %q: want bytes or a Ki/Mi/Gi value", raw)
	}
	return n, nil
}

func orStr(v, fallback string) string {
	if v != "" {
		return v
	}
	return fallback
}

func orInt(v, fallback int) int {
	if v != 0 {
		return v
	}
	return fallback
}

func orDefaultInt32(v, fallback int32) int32 {
	if v == 0 {
		return fallback
	}
	return v
}
