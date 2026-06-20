package control

import (
	"context"
	"fmt"

	"conductor/internal/project"
	"conductor/internal/storage"
)

const rollbackUsage = `conductor rollback [--to vN] [-p P -e E -s S]

Re-point the service at an EARLIER deployment version — no rebuild. The target
version's image and settings are reused verbatim (rollback never reads
config.toml), and the reconcile loop converges to it. Defaults to the version
just before the current one; --to picks a specific past version.

This is the analog of a fresh 'conductor up' in reverse: up appends a new
version, rollback re-selects an existing one. Requires project, environment, and
service.

Flags:
  --to vN   roll back to a specific version (e.g. --to v2 or --to 2)`

func cmdRollback(args []string) error {
	fs := newFlagSet("rollback", rollbackUsage)
	var t Target
	addTargetFlags(fs, &t)
	to := fs.String("to", "", "roll back to a specific version (e.g. v2)")
	if err := fs.parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return usageErr(rollbackUsage, "unexpected argument: "+fs.Arg(0))
	}
	resolve(&t, true)
	if err := t.require(true, true); err != nil {
		return usageErr(rollbackUsage, err.Error())
	}

	version, err := project.ParseVersion(*to)
	if err != nil {
		return usageErr(rollbackUsage, "--to "+err.Error())
	}

	ctx := context.Background()
	store, err := storage.NewPostgresClient(ctx, databaseURL())
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = store.Close() }()

	res, err := project.New(store).Rollback(ctx, project.RollbackInput{Target: t.Target, ToVersion: version})
	if err != nil {
		return err
	}

	fmt.Printf("→ rolled back %s  v%d → v%d\n", t, res.From, res.To)
	return nil
}
