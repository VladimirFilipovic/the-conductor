#!/bin/sh
# Sourced by both container entrypoints. Boot order is a poll, not a dependency
# graph: compose's healthcheck gates the socket but not the schema, and Railway
# has no depends_on at all, so each container waits for the condition it
# actually needs and the supervisor restarts it if that never arrives.

DSN="${CONDUCTOR_DATABASE_URL:?CONDUCTOR_DATABASE_URL must be set}"

# wait_until <tries> <what> <command...>: retry every 2s until the command
# succeeds; give up loudly rather than starting against a database that isn't
# there.
wait_until() {
	tries=$1
	what=$2
	shift 2
	attempt=0
	until "$@" >/dev/null 2>&1; do
		attempt=$((attempt + 1))
		if [ "$attempt" -ge "$tries" ]; then
			echo "entrypoint: $what after $attempt attempts" >&2
			exit 1
		fi
		sleep 2
	done
}

db_reachable() {
	psql "$DSN" -v ON_ERROR_STOP=1 -tAq -c 'SELECT 1'
}

hosts_seeded() {
	[ "$(psql "$DSN" -tAc 'SELECT count(*) FROM hosts' 2>/dev/null || echo 0)" -gt 0 ]
}
