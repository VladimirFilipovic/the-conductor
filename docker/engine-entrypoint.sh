#!/bin/sh
# Migrate the schema and load the host fleet, then hand off to the engine. The
# reconcile loop can't place anything onto an empty hosts table, so the seed is
# part of bringing the engine up, not an optional dev step.
set -eu

DSN="${CONDUCTOR_DATABASE_URL:?CONDUCTOR_DATABASE_URL must be set}"

# goose speaks the same DSN the engine uses. The postgres depends_on healthcheck
# gates start, but retry briefly to cover the gap before the socket accepts.
echo "engine-entrypoint: running migrations"
tries=0
until goose -dir ./db/migrations postgres "$DSN" up; do
	tries=$((tries + 1))
	if [ "$tries" -ge 30 ]; then
		echo "engine-entrypoint: migrations failed after $tries attempts" >&2
		exit 1
	fi
	echo "engine-entrypoint: migrate retry $tries"
	sleep 2
done

echo "engine-entrypoint: seeding hosts"
psql "$DSN" -v ON_ERROR_STOP=1 -f ./db/seeds/hosts.sql

echo "engine-entrypoint: starting engine"
exec conductor engine
