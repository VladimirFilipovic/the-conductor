#!/bin/sh
# Shared entrypoint for the engine and apiserver containers (same image).
# engine: migrate the schema and load the host fleet, then run — the reconcile
# loop can't place anything onto an empty hosts table, so the seed is part of
# bringing the engine up, not an optional dev step. Both steps are idempotent
# and neither ever drops anything: wiping is an explicit operator action
# (`make stack-fresh`, which removes the database volume), never something a
# container start infers on its own.
# any other mode (apiserver): wait until the engine has seeded, then run. Only
# one container migrates, so goose never races itself.
set -eu

DSN="${CONDUCTOR_DATABASE_URL:?CONDUCTOR_DATABASE_URL must be set}"
# No args (the engine service sets no command) means engine; the mode is then
# also what gets exec'd, so the default has to land in "$@" itself.
if [ "$#" -eq 0 ]; then
	set -- engine
fi
MODE="$1"

psql_q() { psql "$DSN" -v ON_ERROR_STOP=1 -tAq -c "$1"; }

if [ "$MODE" = "engine" ]; then
	# The postgres depends_on healthcheck gates start, but retry briefly to
	# cover the gap before the socket accepts.
	tries=0
	until psql_q 'SELECT 1' >/dev/null 2>&1; do
		tries=$((tries + 1))
		if [ "$tries" -ge 30 ]; then
			echo "entrypoint: database unreachable after $tries attempts" >&2
			exit 1
		fi
		sleep 2
	done

	echo "entrypoint: running migrations"
	goose -dir ./db/migrations postgres "$DSN" up

	echo "entrypoint: seeding hosts"
	psql "$DSN" -v ON_ERROR_STOP=1 -q -f ./db/seeds/hosts.sql
else
	echo "entrypoint: waiting for the engine to migrate and seed"
	tries=0
	until [ "$(psql "$DSN" -tAc 'SELECT count(*) FROM hosts' 2>/dev/null || echo 0)" -gt 0 ]; do
		tries=$((tries + 1))
		if [ "$tries" -ge 60 ]; then
			echo "entrypoint: schema not ready after $tries attempts" >&2
			exit 1
		fi
		sleep 2
	done
fi

echo "entrypoint: starting conductor $*"
exec conductor "$@"
