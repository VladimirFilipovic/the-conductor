package control

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"

	"conductor/internal/project"
	"conductor/internal/storage"
	"conductor/internal/storage/db"
)

const volumeUsage = `conductor volume <subcommand> [flags]

Manage a service's persistent volumes. A volume follows the service across
restarts and reschedules. Its size is set at add (default 1 GiB) and stays
mutable — grow it later with update. Requires a project and service (volumes
are service-scoped, so no environment is needed).

Subcommands:
  list                List the service's volumes.
  add --mount PATH    Attach a new volume (default 1 GiB; pass --size GiB).
  update --size GiB   Grow the volume at --mount PATH (live, grow-only). The
                      engine applies it once the host has room; until then
                      list shows the volume attached with a pending grow, and
                      update back to the on-disk size withdraws it.
  rm --mount PATH     Detach and delete the volume at the mount path.`

// Sizes are entered in GiB on the CLI, stored as bytes in the control plane.
const gib = 1 << 30

func cmdVolume(args []string) error {
	sub, rest := splitSubcommand(args)
	if sub == "" {
		return usageErr(volumeUsage, "a subcommand is required")
	}

	fs := newFlagSet("volume "+sub, volumeUsage)
	var t Target
	addTargetFlags(fs, &t)
	mount := fs.String("mount", "", "mount path inside the container")
	size := fs.Int("size", 0, "volume size in GiB (add: initial, default 1; update: new size)")
	if err := fs.parse(rest); err != nil {
		return err
	}
	resolve(&t, true)
	// Volumes key off the service, a single project-scoped row shared across
	// environments — the environment pointer is irrelevant here.
	if err := t.require(false, true); err != nil {
		return usageErr(volumeUsage, err.Error())
	}

	ctx := context.Background()
	store, err := storage.NewPostgresClient(ctx, databaseURL())
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = store.Close() }()
	proj := project.New(store)

	switch sub {
	case "list", "ls":
		return listVolumesCmd(ctx, proj, t)
	case "add":
		if *mount == "" {
			return usageErr(volumeUsage, "volume add requires --mount")
		}
		if *size < 0 {
			return usageErr(volumeUsage, "volume add --size must be >= 0")
		}
		var sizeBytes int64
		if *size > 0 {
			sizeBytes = int64(*size) * gib
		}
		vol, err := proj.AddVolume(ctx, project.AddVolumeInput{Target: t.Target, MountPath: *mount, SizeBytes: sizeBytes})
		if err != nil {
			return err
		}
		fmt.Printf("added volume at %s on service %q in project %s  (%s, %s)\n", vol.MountPath, t.Service, t.Project, vol.Region, formatBytes(vol.DesiredSizeBytes))
		return nil
	case "update", "resize":
		if *size <= 0 {
			return usageErr(volumeUsage, "volume update requires --size > 0")
		}
		if *mount == "" {
			return usageErr(volumeUsage, "volume update requires --mount")
		}
		out, err := proj.ResizeVolume(ctx, t.Target, *mount, int64(*size)*gib)
		if err != nil {
			return err
		}
		vol := out.Volume
		fmt.Printf("resizing volume at %s on service %q in project %s → %s\n", vol.MountPath, t.Service, t.Project, formatBytes(vol.DesiredSizeBytes))
		switch {
		case !vol.HostID.Valid:
			fmt.Println("  not placed yet: the volume will be created at the new size")
		case vol.ObservedSizeBytes.Valid && vol.DesiredSizeBytes == vol.ObservedSizeBytes.Int64:
			fmt.Println("  matches what is on disk: pending grow withdrawn")
		case out.WaitingForSpace:
			fmt.Printf("  host is short %s of volume budget; the grow waits until space frees up (no action needed)\n", formatBytes(out.ShortfallBytes))
		default:
			fmt.Println("  host has room: the grow starts on the next reconcile tick")
		}
		return nil
	case "rm", "remove", "delete":
		if *mount == "" {
			return usageErr(volumeUsage, "volume rm requires --mount")
		}
		vol, err := proj.RemoveVolume(ctx, t.Target, *mount)
		if err != nil {
			return err
		}
		fmt.Printf("removed volume at %s on service %q in project %s\n", vol.MountPath, t.Service, t.Project)
		return nil
	}
	return usageErr(volumeUsage, "unknown volume subcommand "+sub)
}

func listVolumesCmd(ctx context.Context, proj *project.Service, t Target) error {
	vols, err := proj.ListVolumes(ctx, t.Target)
	if err != nil {
		return err
	}
	if len(vols) == 0 {
		fmt.Printf("no volumes on service %q in project %s\n", t.Service, t.Project)
		return nil
	}
	renderVolumes(os.Stdout, vols)
	return nil
}

func renderVolumes(w *os.File, vols []db.Volume) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "MOUNT\tNAME\tREGION\tSIZE\tON DISK\tSTATUS")
	for _, v := range vols {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			v.MountPath, v.Name, v.Region, formatBytes(v.DesiredSizeBytes), formatObserved(v), volumeStatusLabel(v))
	}
	_ = tw.Flush()
}

func formatObserved(v db.Volume) string {
	if !v.ObservedSizeBytes.Valid {
		return "-"
	}
	return formatBytes(v.ObservedSizeBytes.Int64)
}

// volumeStatusLabel surfaces the one state the status column can't: attached
// with desired above what's on disk is a grow the engine hasn't approved yet
// (the host is out of room), which the operator should be able to see without
// comparing two size columns.
func volumeStatusLabel(v db.Volume) string {
	if v.Status == "attached" && v.ObservedSizeBytes.Valid && v.DesiredSizeBytes > v.ObservedSizeBytes.Int64 {
		return "attached (grow waiting for host space)"
	}
	return v.Status
}

// formatBytes renders GiB, the unit the CLI accepts; one decimal when the
// value isn't whole (advisory shortfalls rarely are).
func formatBytes(b int64) string {
	if b%gib == 0 {
		return fmt.Sprintf("%dGiB", b/gib)
	}
	return fmt.Sprintf("%.1fGiB", float64(b)/float64(gib))
}
