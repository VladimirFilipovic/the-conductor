# cmd — Conductor CLI dispatcher

`Run(args)` (called from `main.go` as `os.Exit(cmd.Run(os.Args[1:]))`) parses
argv, switches on the first token, and calls the matching subcommand. Each
subcommand lives in its own file and follows the same shape:

1. `newFlagSet(name, usage)` builds a silenced `flag.FlagSet`; the command
   registers the universal target flags (`addTargetFlags`, or the per-field
   `addProjectFlag`/`addEnvironmentFlag`/`addServiceFlag` when it overloads one
   of the names) plus any command-specific flags, then calls `fs.parse(args)`.
2. `resolve(&tgt, useLink)` fills any empty target field from the `CONDUCTOR_*`
   env vars and then, when `useLink` is true, from the folder-link file.
3. `tgt.require(environment, service)` validates the target — project is always
   required; environment/service are gated by the two bools.
4. The command acts: it opens a Postgres client (`storage.NewPostgresClient`),
   constructs the relevant domain service (`project.New(store)`, `status.New(store)`),
   calls one of its methods, and renders the result. Read-only commands
   (`config`, `unlink`) skip the store.

`Target` (in `cmd.go`) is the resolved `(project, environment, service)` triple,
passed explicitly as an argument. It is distinct from `context.Context`, which
the commands use only for cancellation/deadlines when opening the store.

## Target resolution

Every command resolves its `(project, environment, service)` target from three
tiers, highest precedence first:

1. **Flags** — `--project/-p`, `--environment/-e`, `--service/-s`
2. **Env vars** — `CONDUCTOR_PROJECT`, `CONDUCTOR_ENVIRONMENT`, `CONDUCTOR_SERVICE`
3. **Folder link** — `.conductor/config.json`, discovered by walking up from cwd
   (see `internal/link`); written by `link`, `init`, and the
   `environment/service select` subcommands

| Need        | Flag                  | Env var                 |
|-------------|-----------------------|-------------------------|
| Project     | `--project`, `-p`     | `CONDUCTOR_PROJECT`     |
| Environment | `--environment`, `-e` | `CONDUCTOR_ENVIRONMENT` |
| Service     | `--service`, `-s`     | `CONDUCTOR_SERVICE`     |

`link` resolves with the folder-link disabled (`useLink=false`) so an existing
link's project can't silently satisfy a re-link.

## Commands

| File             | Command                                          | Requires (P/E/S) |
|------------------|--------------------------------------------------|------------------|
| `init.go`        | `init -n NAME`                                   | — (creates the project + links dir) |
| `link.go`        | `link -p PROJECT [-e ENV]`                       | P |
| `link.go`        | `unlink`                                         | — |
| `config.go`      | `config`                                         | — (prints whatever resolves) |
| `add.go`         | `add (--service \| --database --engine T) --name N [--image I] [--repo U]` | P, E |
| `environment.go` | `environment [list \| create -n NAME \| select NAME]` | P |
| `service.go`     | `service [name]`                                 | P, E |
| `up.go`          | `up [path] [--ci] [--detach]`                    | P, E, S |
| `down.go`        | `down [--yes]`                                   | P, E, S |
| `scale.go`       | `scale <region=N ...>`                           | P, E, S |
| `volume.go`      | `volume <list \| add \| update \| rm>`           | P, E, S |
| `status.go`      | `status`                                         | P |

`init` and `link` write the folder link, so subsequent commands in that
directory can omit `-p/-e/-s`. `environment select` / `service select` update
the environment/service pointers in the same file.

## Examples

```bash
# Create a project (+ default "production" env) and link this directory to it.
conductor init -n rxlog-platform

# From here -p/-e default to the link; add a code service and a database.
conductor add --service --name web --repo https://github.com/acme/web
conductor add --database --engine postgres --name pg

# Deploy, scale, inspect — service comes from the link or -s.
conductor up -s web
conductor scale us-east-1=3 -s web
conductor volume add --mount /var/lib/postgresql/data -s pg
conductor status

# Or skip the link entirely and drive everything from the environment.
export CONDUCTOR_PROJECT=rxlog-platform
export CONDUCTOR_ENVIRONMENT=production
export CONDUCTOR_SERVICE=web
conductor up
conductor status
```
