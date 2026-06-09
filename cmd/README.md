# cmd — Conductor CLI dispatcher

`Run(args)` (called from `main.go` as `os.Exit(cmd.Run(os.Args[1:]))`) parses
argv, switches on the first token, and calls the matching subcommand. Each
subcommand lives in its own file and follows the same shape:

1. `extractTarget(args)` pulls the universal `--project/-p`, `--environment/-e`,
   `--service/-s` flags out of the arg list (from any position) and merges them
   with the `CONDUCTOR_*` environment variables into a `Context`.
2. A per-command `flag.FlagSet` parses the command-specific flags from what's
   left.
3. `ctx.require(...)` validates that the needed parts of the target are present.
4. `engineTODO(...)` is the seam where a real implementation would talk to the
   orchestration engine. The interface layer just echoes the resolved target.

## No `link`

Unlike Railway, there is **no `link` command** and no on-disk link file. Every
command resolves its `(project, environment, service)` target purely from flags
or environment variables. Flags always win over env vars.

| Need | Flag | Env var |
|---|---|---|
| Project   | `--project`, `-p`     | `CONDUCTOR_PROJECT`     |
| Environment | `--environment`, `-e` | `CONDUCTOR_ENVIRONMENT` |
| Service   | `--service`, `-s`     | `CONDUCTOR_SERVICE`     |
| Auth token | —                    | `CONDUCTOR_TOKEN`       |

## Commands

| File | Command | Requires (P/E/S) |
|---|---|---|
| `init.go`        | `init [name]`             | — (creates the project) |
| `add.go`         | `add --service/--database`| P, E |
| `environment.go` | `environment [new] [name]`| P |
| `service.go`     | `service [name]`          | P, E |
| `up.go`          | `up [path]`               | P, E, S |
| `down.go`        | `down`                    | P, E, S |
| `scale.go`       | `scale <region=N ...>`    | P, E, S |
| `volume.go`      | `volume <sub>`            | P, E, S |
| `status.go`      | `status`                  | P |

## Examples

```bash
conductor init rxlog-platform
conductor add --database postgres -p rxlog-platform -e production
conductor up -p rxlog-platform -e production -s web
conductor scale us-west1=3 -s web -p rxlog-platform -e production
conductor volume add --mount /var/lib/postgresql/data -s postgres -p rxlog-platform -e production

# or drive everything from the environment
export CONDUCTOR_PROJECT=rxlog-platform
export CONDUCTOR_ENVIRONMENT=production
export CONDUCTOR_SERVICE=web
conductor up
conductor status
```
