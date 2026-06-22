package cmd

import (
	"conductor/cmd/control"
	"conductor/cmd/engine"
)

func Run(args []string) int {
	if len(args) > 0 && args[0] == "engine" {
		return engine.Run(args[1:])
	}
	return control.Run(args)
}
