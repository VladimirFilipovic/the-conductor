package cmd

const volumeUsage = `conductor volume <subcommand> [flags]

Manage a service's persistent volumes. A volume follows the service across
restarts and reschedules. Volume size is a mutable property you patch (like
scale) — there is no create-time --size on "add". Requires project,
environment, and service.

Subcommands:
  list                       list the service's volumes
  add --mount PATH           attach a new volume at the given mount path
  update --size GB [--mount PATH]  resize a volume (live)
  rm --mount PATH            detach and delete a volume`

func cmdVolume(args []string) int {
	rest, ctx := extractTarget(args)
	if len(rest) == 0 {
		return usageErr(volumeUsage, "a subcommand is required")
	}
	sub, subArgs := rest[0], rest[1:]

	fs := newFlagSet("volume "+sub, volumeUsage)
	mount := fs.String("mount", "", "mount path inside the container")
	size := fs.Int("size", 0, "volume size in GB (update only)")
	if code := parse(fs, subArgs); code != contParse {
		return code
	}
	if err := ctx.require(true, true, true); err != nil {
		return usageErr(volumeUsage, err.Error())
	}

	switch sub {
	case "list", "ls":
		return engineTODO("list volumes", ctx, "")
	case "add":
		if *mount == "" {
			return usageErr(volumeUsage, "volume add requires --mount")
		}
		return engineTODO("add volume", ctx, "mount "+*mount)
	case "update", "resize":
		if *size <= 0 {
			return usageErr(volumeUsage, "volume update requires --size > 0")
		}
		detail := "size → " + itoa(*size) + "GB"
		if *mount != "" {
			detail = "mount " + *mount + "  " + detail
		}
		return engineTODO("update volume", ctx, detail)
	case "rm", "remove", "delete":
		if *mount == "" {
			return usageErr(volumeUsage, "volume rm requires --mount")
		}
		return engineTODO("remove volume", ctx, "mount "+*mount)
	}
	return usageErr(volumeUsage, "unknown volume subcommand "+sub)
}
