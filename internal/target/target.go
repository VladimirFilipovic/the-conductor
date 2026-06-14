// Package target defines the (project, environment, service) identity triple
// that every layer resolves before talking to the control plane. It is a pure
// value type with no behavior — the CLI layer hangs flag/env resolution off it,
// the link layer persists it, and the project layer takes it as input.
package target

// Target is the identity triple a command is aimed at. The json tags match the
// folder-link wire format (internal/link aliases this type), so changing them
// changes the on-disk .conductor/config.json.
type Target struct {
	Project     string `json:"project"`
	Environment string `json:"environment,omitempty"`
	Service     string `json:"service,omitempty"`
}
