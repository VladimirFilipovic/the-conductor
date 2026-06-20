package cmd

import "conductor/cmd/control"

// Run dispatches the CLI. The `engine` command starts the long-running
// orchestration engine; every other command is a short-lived control-plane
// client handled by conductor/cmd/control. This package is the stable entry
// point main.go depends on.
func Run(args []string) int {
	if len(args) > 0 && args[0] == "engine" {
		return runEngine(args[1:])
	}
	return control.Run(args)
}
