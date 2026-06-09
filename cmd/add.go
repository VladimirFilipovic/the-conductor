package cmd

const addUsage = `conductor add [--service NAME | --database TYPE] [--image IMG] [--repo URL]

Add a service or a database to the project. A --database service is stateful
and is provisioned with a managed image and a volume; a --service is built from
this repo (or the given --image). Requires a project and environment.

Flags:
  --service NAME   add a code service with this name
  --database TYPE  add a managed database (e.g. postgres, redis, mysql)
  --image IMG      base image for a --service (skip the build step)
  --repo URL       source repo for a --service (default: current directory)`

func cmdAdd(args []string) error {
	rest, ctx := extractTarget(args)
	fs := newFlagSet("add", addUsage)
	service := fs.String("service", "", "name of a code service to add")
	database := fs.String("database", "", "type of a managed database to add")
	image := fs.String("image", "", "base image for a code service")
	repo := fs.String("repo", "", "source repo for a code service")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if err := ctx.require(true, true, false); err != nil {
		return usageErr(addUsage, err.Error())
	}
	if (*service == "") == (*database == "") {
		return usageErr(addUsage, "exactly one of --service or --database is required")
	}

	if *database != "" {
		return engineTODO("add database "+*database, ctx, "stateful — provisions a volume")
	}
	detail := "code service"
	if *image != "" {
		detail = "image " + *image
	} else if *repo != "" {
		detail = "repo " + *repo
	}
	ctx.Service = *service
	return engineTODO("add service "+*service, ctx, detail)
}
