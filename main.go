package main

import (
	"os"

	"conductor/cmd"
)

func main() {
	os.Exit(cmd.Run(os.Args[1:]))
}
