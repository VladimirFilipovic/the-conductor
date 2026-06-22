// Package config is the single source for environment-derived settings shared by
// the control CLI and the engine: the Postgres DSN, log level, and the target-
// identity defaults. Both binaries call Load once at startup and read from the
// returned Config rather than touching os.Getenv directly.
package config

import (
	"fmt"
	"log/slog"
	"os"
)

// Environment variables read across the conductor binaries.
const (
	VarDatabaseURL = "CONDUCTOR_DATABASE_URL"
	VarLogLevel    = "LOG_LEVEL"
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

func orDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
