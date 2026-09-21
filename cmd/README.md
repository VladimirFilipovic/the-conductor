# cmd — Conductor CLI dispatcher

Dispatch is two levels. `cmd.Run(args)` (called from `main.go` as
`os.Exit(cmd.Run(os.Args[1:]))`) peels off the long-running server commands —
`engine`, `apiserver`, `agentsim`, each its own package — and hands everything
else to `control.Run`, which is the operator CLI proper.

`control.Run` switches on the first token and calls the matching subcommand.
Each subcommand lives in its own file under `control/` and follows the same
shape:

1. `newFlagSet(name, usage)` builds a silenced `flag.FlagSet`; the command
   registers the universal target flags (`addTargetFlags`, or the per-field
   `addProjectFlag`/`addEnvironmentFlag`/`addServiceFlag` when it overloads one
   of the names) plus any command-specific flags, then calls `fs.parse(args)`.
2. `resolve(&tgt, useLink)` fills any empty target field from the `CONDUCTOR_*`
   env vars and then, when `useLink` is true, from the folder-link file.
   `status` calls `resolveProject` instead — see below.
3. `tgt.require(environment, service)` validates the target — project is always
   required; environment/service are gated by the two bools. It reports
   everything missing at once rather than one field per run.
4. The command acts: it opens a Postgres client (`storage.NewPostgresClient`),
   constructs the relevant domain service (`project.New(store)`, `status.New(store)`),
   calls one of its methods, and renders the result. Read-only commands
   (`config`, `unlink`) skip the store.

`Target` (in `control/control.go`) is the resolved `(project, environment,
service)` triple, passed explicitly as an argument. It embeds
`internal/target.Target` — the pure value type the link layer persists and the
project layer consumes — and adds the CLI-side flag/env resolution. It is
distinct from `context.Context`, which the commands use only for
cancellation/deadlines when opening the store.

## Target resolution

Every command resolves its `(project, environment, service)` target from three
tiers, highest precedence first:

1. **Flags** — `--project/-p`, `--environment/-e`, `--service/-s`
2. **Env vars** — `CONDUCTOR_PROJECT`, `CONDUCTOR_ENVIRONMENT`, `CONDUCTOR_SERVICE`
3. **Folder link** — `.conductor/config.json`, discovered by walking up from cwd
   until a hit or the filesystem root, so the nearest link wins (see
   `internal/link`); written by `link`, `init`, `add --link`, and the
   `environment/service select` subcommands

| Need        | Flag                  | Env var                 |
|-------------|-----------------------|-------------------------|
| Project     | `--project`, `-p`     | `CONDUCTOR_PROJECT`     |
| Environment | `--environment`, `-e` | `CONDUCTOR_ENVIRONMENT` |
| Service     | `--service`, `-s`     | `CONDUCTOR_SERVICE`     |

Resolution is per-field, not all-or-nothing: an env var can supply the project
while the link still supplies the service.

Two commands opt out of the plain `resolve`:

- `link` resolves with the folder-link disabled (`useLink=false`) so an existing
  link's project can't silently satisfy a re-link.
- `status` uses `resolveProject`, which reads *only* the project from the link,
  so a link pointing at one service can't silently narrow a whole-project view —
  narrowing takes an explicit `-e/-s`.

## Commands

| File             | Command                                          | Requires (P/E/S) |
|------------------|--------------------------------------------------|------------------|
| `init.go`        | `init -n NAME`                                   | — (creates the project + links cwd) |
| `link.go`        | `link -p PROJECT [-e ENV]`                       | P |
| `link.go`        | `unlink`                                         | — |
| `config.go`      | `config`                                         | — (prints whatever resolves) |
| `add.go`         | `add (--service \| --database --engine T) --name N [--image I] [--repo U] [--stateful] [--link\|-l]` | P, E |
| `environment.go` | `environment [list \| create -n NAME \| select NAME]` | P |
| `service.go`     | `service [name]`                                 | P, E |
| `up.go`          | `up [-f PATH]`                                   | P, E, S |
| `rollback.go`    | `rollback [--to VERSION]`                        | P, E, S |
| `down.go`        | `down [--yes]`                                   | P, E, S |
| `scale.go`       | `scale <region=N ...>`                           | P, E, S |
| `volume.go`      | `volume <list \| add \| update \| revert \| rm>` | P, E, S |
| `status.go`      | `status`                                         | P |

Aliases: `deploy` → `up`; `env`/`envs`/`environments` → `environment`; inside
`volume`, `ls` → `list`, `resize` → `update`, `remove`/`delete` → `rm`.

`up` takes its spec path as a flag (`-f`), never a positional — stdlib `flag`
stops parsing at the first positional, which would silently drop any flags
after it, so a positional argument is rejected with a usage error.

`init` and `link` write the folder link into **cwd** (they don't search for an
existing one first), so subsequent commands in that directory can omit
`-p/-e/-s`. `environment select` / `service select` update the
environment/service pointers in the same file, and `add --link` points it at the
service it just created.

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
conductor rollback --to v2 -s web
conductor status

# Or skip the link entirely and drive everything from the environment.
export CONDUCTOR_PROJECT=rxlog-platform
export CONDUCTOR_ENVIRONMENT=production
export CONDUCTOR_SERVICE=web
conductor up
conductor status
```
