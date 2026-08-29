// Package config is the single source for environment-derived settings shared by
// the control CLI and the engine: the Postgres DSN, log level, and the target-
// identity defaults. Both binaries call Load once at startup and read from the
// returned Config rather than touching os.Getenv directly.
package config

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
)

// Environment variables read across the conductor binaries.
const (
	VarDatabaseURL = "CONDUCTOR_DATABASE_URL"
	VarLogLevel    = "LOG_LEVEL"
	VarLogFile     = "CONDUCTOR_LOG_FILE"
	VarProject     = "CONDUCTOR_PROJECT"
	VarEnvironment = "CONDUCTOR_ENVIRONMENT"
	VarService     = "CONDUCTOR_SERVICE"
	varUser        = "USER"
)

// DefaultDatabaseURL targets the local docker-compose Postgres (see
// docker-compose.yml) so both binaries run out of the box without exporting
// CONDUCTOR_DATABASE_URL. The Makefile sets the same value for `make` recipes.
const DefaultDatabaseURL = "postgres://conductor:conductor@localhost:5432/conductor?sslmode=disable"

// Config is the process-wide resolved environment. Loaded once at startup; the
// environment is not consulted again after Load returns.
type Config struct {
	// DatabaseURL is the control-plane Postgres DSN (CONDUCTOR_DATABASE_URL,
	// falling back to the local-dev database).
	DatabaseURL string
	// LogLevel is the engine's slog level (LOG_LEVEL), defaulting to Info.
	LogLevel slog.Level
	// LogFile, when set (CONDUCTOR_LOG_FILE), additionally appends engine logs
	// to this path; empty means stderr only.
	LogFile string

	// Target-identity defaults; an explicit -p/-e/-s flag overrides each.
	Project     string
	Environment string
	Service     string

	// User stamps a deployment's created_by (the OS $USER).
	User string
}

// Load resolves configuration from the environment, applying the local-dev DSN
// default and defaulting the log level to Info. A malformed LOG_LEVEL is the
// only hard error.
func Load() (Config, error) {
	cfg := Config{
		DatabaseURL: orDefault(VarDatabaseURL, DefaultDatabaseURL),
		LogLevel:    slog.LevelInfo,
		LogFile:     os.Getenv(VarLogFile),
		Project:     os.Getenv(VarProject),
		Environment: os.Getenv(VarEnvironment),
		Service:     os.Getenv(VarService),
		User:        os.Getenv(varUser),
	}
	if l := os.Getenv(VarLogLevel); l != "" {
		if err := cfg.LogLevel.UnmarshalText([]byte(l)); err != nil {
			return Config{}, fmt.Errorf("invalid %s %q: %w", VarLogLevel, l, err)
		}
	}
	return cfg, nil
}

// Placement holds the bin-packing knobs (docs/bin-pack.md). Flag-derived
// rather than env-derived: these are engine server flags, tuned per fleet.
type Placement struct {
	// Headroom is the per-host reserve as a fraction of capacity; 0 disables.
	// Sizing intent: ≈ largest host per region spread across N hosts (1/N).
	Headroom float64
	// AntiAffinity spreads same-slot replicas across hosts (hard constraint
	// with a soft fallback); off = pure bin packing.
	AntiAffinity bool
	// ScarcityWeights weight the packing score by per-region fleet fullness;
	// off fixes every dimension's weight at 1.0.
	ScarcityWeights bool
	// VolumeBudget is the fraction of host disk packable by volumes; the rest
	// is ephemeral (images, logs, OS).
	VolumeBudget float64
	// DiskReserve is the fraction of the volume budget held for grow-only
	// resizes; new volumes can't use it.
	DiskReserve float64
}

func DefaultPlacement() Placement {
	return Placement{
		Headroom:        0.1,
		AntiAffinity:    true,
		ScarcityWeights: true,
		VolumeBudget:    0.8,
		DiskReserve:     0.2,
	}
}

// PlacementFlags binds the placement server flags onto fs, returning the
// destination struct pre-loaded with defaults.
func PlacementFlags(fs *flag.FlagSet) *Placement {
	p := DefaultPlacement()
	fs.Float64Var(&p.Headroom, "placement.headroom", p.Headroom,
		"per-host reserve as a fraction of capacity; 0 disables")
	fs.BoolVar(&p.AntiAffinity, "placement.anti-affinity", p.AntiAffinity,
		"spread same-slot replicas across hosts; off = pure bin packing")
	fs.BoolVar(&p.ScarcityWeights, "placement.scarcity-weights", p.ScarcityWeights,
		"weight the packing score by per-region resource scarcity")
	fs.Float64Var(&p.VolumeBudget, "placement.volume-budget", p.VolumeBudget,
		"fraction of host disk packable by volumes; the rest is ephemeral")
	fs.Float64Var(&p.DiskReserve, "placement.disk-reserve", p.DiskReserve,
		"fraction of the volume budget held for grow-only resizes")
	return &p
}

func orDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
