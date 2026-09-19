#!/bin/sh
# The engine container's boot: migrate the schema, load the host fleet, then
# run the reconcile loop. The seed belongs here rather than in a dev-only step
# because the loop can't place anything onto an empty hosts table. Both steps
# are idempotent and neither ever drops anything: wiping is an explicit
# operator action (`make stack-fresh`), never something a container start
# infers on its own.
#
# This is the only script that runs goose, so migrations never race themselves
# — which is also why the engine stays at a single replica.
set -eu

. /usr/local/bin/wait-for-db.sh

wait_until 30 'database unreachable' db_reachable

echo "entrypoint: running migrations"
goose -dir ./db/migrations postgres "$DSN" up

echo "entrypoint: seeding hosts"
psql "$DSN" -v ON_ERROR_STOP=1 -q -f ./db/seeds/hosts.sql

echo "entrypoint: starting conductor engine"
exec conductor engine "$@"
