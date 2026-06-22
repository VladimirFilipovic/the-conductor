package control

import (
	"fmt"

	"conductor/internal/spec"
)

const configUsage = `conductor config

Print the resolved identity (from flags/env/link file) and the parsed
config.toml spec, merged for the active environment. Read-only — touches neither
the control plane nor any files. Useful for checking what 'conductor up' would
deploy.`

func cmdConfig(args []string) error {
	fs := newFlagSet("config", configUsage)
	var t Target
	addTargetFlags(fs, &t)
	if err := fs.parse(args); err != nil {
		return err
	}
	resolve(&t, true)

	report, err := spec.Render(t.Target)
	if err != nil {
		return err
	}
	fmt.Print(report)
	return nil
}
