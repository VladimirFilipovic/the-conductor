package cmd

import "conductor/conductor"

// runEngine starts the orchestration engine. It is a distinct mode from the
// control commands (which are short-lived client invocations), so Run routes
// it here rather than through cmd/control's dispatcher.
func runEngine(_ []string) int {
	conductor.Run()
	return 0
}
