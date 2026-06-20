package cmd

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"conductor/internal/project"
	"conductor/internal/storage"
)

const downUsage = `conductor down [--yes]

Scale the service's replicas to zero. Stateful data (volumes) is preserved —
this stops compute, it does not delete the service. Requires project,
environment, and service.

Flags:
  --yes   skip the confirmation prompt`

func cmdDown(args []string) error {
	fs := newFlagSet("down", downUsage)
	var t Target
	addTargetFlags(fs, &t)
	yes := fs.Bool("yes", false, "skip confirmation")
	if err := fs.parse(args); err != nil {
		return err
	}
	resolve(&t, true)
	if err := t.require(true, true); err != nil {
		return usageErr(downUsage, err.Error())
	}

	if !*yes && !confirm(fmt.Sprintf("Scale %s to zero replicas? Volumes are preserved. [y/N]: ", t)) {
		fmt.Println("aborted")
		return nil
	}

	ctx := context.Background()
	store, err := storage.NewPostgresClient(ctx, databaseURL())
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = store.Close() }()

	if err := project.New(store).Down(ctx, t.Target); err != nil {
		return err
	}

	fmt.Printf("→ scaled down %s\n  replicas → 0 (volumes preserved)\n", t)
	return nil
}

// confirm prints prompt and returns true only when the user answers y/yes. A
// closed or empty stdin reads as "no", so a non-interactive `down` without --yes
// safely aborts rather than tearing compute down unattended.
func confirm(prompt string) bool {
	fmt.Print(prompt)
	s := bufio.NewScanner(os.Stdin)
	if !s.Scan() {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(s.Text())) {
	case "y", "yes":
		return true
	default:
		return false
	}
}
