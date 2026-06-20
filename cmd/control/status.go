package control

import (
	"context"
	"fmt"
	"os"

	"conductor/internal/status"
	"conductor/internal/storage"
)

const statusUsage = `conductor status [-e ENV] [-s SERVICE]

Show desired state (the active deploy commit and the replica count it targets)
next to observed state (the replicas the reconcile loop has actually placed and
found healthy), one row per service. Requires a project; -e/-s narrow the view
to a single environment and/or service.`

func cmdStatus(args []string) error {
	fs := newFlagSet("status", statusUsage)
	var t Target
	addTargetFlags(fs, &t)
	if err := fs.parse(args); err != nil {
		return err
	}
	resolveProject(&t)
	if err := t.require(false, false); err != nil {
		return usageErr(statusUsage, err.Error())
	}

	ctx := context.Background()
	store, err := storage.NewPostgresClient(ctx, databaseURL())
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = store.Close() }()

	rows, err := status.New(store).Fetch(ctx, t.Target)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Printf("no services match %s\n", status.Scope(t.Target))
		return nil
	}
	status.Render(os.Stdout, t.Project, rows)
	return nil
}
