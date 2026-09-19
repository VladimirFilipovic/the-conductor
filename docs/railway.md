# Deploying the stack on Railway

The compose stack maps onto one Railway project: managed Postgres plus four
services on the project's private network. Only chaos-ui gets a public domain;
everything else is reachable at `<service>.railway.internal` and nowhere else.

The one thing that does not transfer is the shared log volume — Railway has no
volume two services can mount. The engine serves its recent and live log lines
over SSE instead (`CONDUCTOR_LOGS_ADDR`), and chaos-ui relays that stream to
the browser.

```
                    ┌──────────┐
                    │ postgres │  managed, TLS, :5432
                    └────┬─────┘
            DATABASE_URL │
                ┌────────┴────────┐
                ▼                 ▼
          ┌──────────┐      ┌───────────┐  gRPC :7443   ┌──────────┐
          │  engine  │      │ apiserver │ ◄──────────── │ agentsim │
          │ SSE :7090│      │ HTTP :7080│               │  :7780   │
          └────▲─────┘      └─────▲─────┘               └────▲─────┘
               │                  │                          │
               └──────────┬───────┴──────────────────────────┘
                     ┌────┴─────┐
                     │ chaos-ui │  public domain, :3000
                     └──────────┘
                           ▲
                        browser
```

Every arrow is a dial direction, and each points at a service that knows
nothing about its caller. The log stream is a pull for that reason: a push
service would make the engine the only component dialling out to a consumer,
and would put a blocking failure mode in the log path.

## Services

Three services build from the repo root, so each needs its own config file
(Railway's per-service **Config-as-code path** setting) — the default
`railway.json` can only describe one of them.

| Service     | Root directory | Config path                    | Start command                           |
| ----------- | -------------- | ------------------------------ | --------------------------------------- |
| `engine`    | repo root      | `deploy/railway/engine.json`   | `/usr/local/bin/engine-entrypoint.sh`   |
| `apiserver` | repo root      | `deploy/railway/apiserver.json`| `/usr/local/bin/apiserver-entrypoint.sh`|
| `agentsim`  | repo root      | `deploy/railway/agentsim.json` | image default                           |
| `chaos-ui`  | `chaos-ui/`    | — (its Dockerfile is detected) | image default                           |

`Dockerfile.engine` declares `CMD`, not `ENTRYPOINT`, so the start command in
each config names a script directly rather than smuggling a mode through argv.

## Variables

| Service     | Variable                   | Value                                        |
| ----------- | -------------------------- | -------------------------------------------- |
| `engine`    | `CONDUCTOR_DATABASE_URL`   | `${{Postgres.DATABASE_URL}}`                  |
|             | `CONDUCTOR_LOGS_ADDR`      | `:7090`                                       |
|             | `LOG_LEVEL`                | `DEBUG`                                       |
| `apiserver` | `CONDUCTOR_DATABASE_URL`   | `${{Postgres.DATABASE_URL}}`                  |
|             | `LOG_LEVEL`                | `DEBUG`                                       |
| `agentsim`  | `CONDUCTOR_AGENTAPI_ADDR`  | `apiserver.railway.internal:7443`             |
|             | `CONDUCTOR_CONTROL_ADDR`   | `:7780`                                       |
| `chaos-ui`  | `CONTROL_PLANE_URL`        | `http://apiserver.railway.internal:7080`      |
|             | `AGENTSIM_URL`             | `http://agentsim.railway.internal:7780`       |
|             | `ENGINE_LOG_URL`           | `http://engine.railway.internal:7090`         |

The UI reads all three of its URLs server-side (none is `NEXT_PUBLIC_`), so
nothing is baked into the browser bundle and the private services stay private.

Go listeners are all bare `:port`, which binds the IPv6 wildcard — what
Railway's IPv6-only private network needs. chaos-ui keeps `HOSTNAME=0.0.0.0`
because it is reached only from the public edge.

## Click-through

1. New project → **Deploy from GitHub repo** → `the-conductor`. Delete the
   service it auto-creates.
2. Add **Postgres** from the template.
3. Add four services from the same repo. Set `chaos-ui/` as the root directory
   on the UI one; point the other three at their config file.
4. Set the variables above, using the `${{Postgres.DATABASE_URL}}` reference on
   engine and apiserver.
5. Generate a domain on chaos-ui with **target port 3000**. Nothing else gets a
   domain.
6. Deploy. Watch the engine log for `entrypoint: seeding hosts`; the apiserver
   then stops waiting, agentsim registers the fleet, and the UI draws the
   topology with the log stream live beside it.

Boot order needs no dependency graph: the engine polls Postgres before
migrating, the apiserver polls until `hosts` is non-empty, and `ON_FAILURE`
restarts cover a service that gives up first. Only the engine runs goose, so
migrations never race — which is also why it stays at **one replica**.

## Postgres over TLS

Railway's Postgres template (`railwayapp-templates/postgres-ssl`) generates a
self-signed certificate at init, so the server does serve TLS and the DSN's
default `sslmode=prefer` negotiates it.

- **Inside the project**, leave `${{Postgres.DATABASE_URL}}` alone. The private
  network is WireGuard-encrypted underneath anyway, and anyone who can reach it
  already holds the DSN.
- **From your laptop**, through Railway's public TCP proxy, append
  `?sslmode=require`. `prefer` means "continue in plaintext if the server
  declines" — a downgrade the other end decides, which over the open internet
  is a downgrade an interceptor can force. `require` pins encryption and skips
  certificate validation, which is what a self-signed certificate needs.

```bash
export CONDUCTOR_DATABASE_URL='postgres://postgres:PASS@HOST.proxy.rlwy.net:PORT/railway?sslmode=require'
./build/conductor up -p demo -e production -s web --image nginx:1.27 --replicas 3
```

`verify-full` is stricter and needs the service's CA exported first — worth it
if the CLI ever touches anything real, overkill for a demo stack.

## If something misbehaves

- **apiserver restarts in a loop.** Its entrypoint gives up after 120s of an
  unseeded schema. Check the engine finished `running migrations` / `seeding
  hosts`; if the engine is failing, the apiserver is only reporting it.
- **The healthcheck fails on a service with two ports.** Railway probes the
  service's target port. The apiserver listens on 7080 (HTTP) and 7443 (gRPC);
  set the target port to 7080, or drop `healthcheckPath` from its config.
- **Log lines arrive in bursts rather than continuously.** Something between
  the engine and the browser is buffering. The engine sends a heartbeat comment
  every 20s and both hops set `X-Accel-Buffering: no`; if bursts persist, the
  Next relay is the hop to instrument first.
- **The Logs page says "stream disconnected".** `ENGINE_LOG_URL` is wrong or
  the engine is down. EventSource retries on its own and resumes from its last
  id, so a recovered engine needs no page reload.
