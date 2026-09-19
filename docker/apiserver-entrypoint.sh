#!/bin/sh
# The apiserver container's boot: wait until the engine has migrated and
# seeded, then serve. Nothing here touches the schema — a second writer would
# race goose, and an apiserver that came up against an empty fleet would serve
# an empty topology to the UI.
set -eu

. /usr/local/bin/wait-for-db.sh

echo "entrypoint: waiting for the engine to migrate and seed"
wait_until 60 'schema not ready' hosts_seeded

echo "entrypoint: starting conductor apiserver"
exec conductor apiserver "$@"
