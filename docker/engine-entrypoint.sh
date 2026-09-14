#!/bin/sh
# Shared entrypoint for the engine and apiserver containers (same image).
# engine: rebuild the schema and load the host fleet, then run — the reconcile
# loop can't place anything onto an empty hosts table, so the seed is part of
# bringing the engine up, not an optional dev step.
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

# Written after the first successful rebuild. It lives in the container's own
# writable layer rather than in a volume, which is exactly the distinction we
# need: a restart (crash loop, `docker restart`, restart: unless-stopped) finds
# it and leaves the data alone, while a recreated container (stack-down then
# stack-up, a rebuilt image, --force-recreate) does not and starts clean.
INIT_MARKER=/var/lib/conductor/schema-initialized

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

	# Dev-only schema policy: there is no production database, so a new
	# container always builds the schema from scratch instead of migrating onto
	# whatever the previous run left behind. DROP SCHEMA rather than
	# `goose reset`, so the wipe never depends on a migration's Down staying in
	# sync with the schema its Up produced.
	if [ ! -f "$INIT_MARKER" ]; then
		echo "entrypoint: new container; dropping and rebuilding the schema"
		psql_q 'DROP SCHEMA public CASCADE; CREATE SCHEMA public;' >/dev/null
	fi

	echo "entrypoint: running migrations"
	goose -dir ./db/migrations postgres "$DSN" up

	echo "entrypoint: seeding hosts"
	psql "$DSN" -v ON_ERROR_STOP=1 -q -f ./db/seeds/hosts.sql

	mkdir -p "$(dirname "$INIT_MARKER")"
	touch "$INIT_MARKER"
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
