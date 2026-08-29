-- +goose Up

-- hosts.revision was the CAS guard for placement, but it aborted on ANY
-- occupancy change: two same-pass placements onto one half-empty host
-- conflicted even when both fit. Its replacement is predicated reservation
-- (see ReserveReplicaOnHost in queries/reconcile.sql) — the reservation
-- UPDATE will carry the real invariant (capacity still sufficient) instead
-- of a version proxy, so only genuinely infeasible placements fail.
ALTER TABLE hosts DROP COLUMN revision;

-- volumes.revision was write-only: bumped on host bind, checked nowhere.
-- Volume safety lives in the lease upsert's own predicate (expired or same
-- replica), so the column guarded nothing.
ALTER TABLE volumes DROP COLUMN revision;

-- +goose Down
ALTER TABLE hosts ADD COLUMN revision bigint NOT NULL DEFAULT 0;
ALTER TABLE volumes ADD COLUMN revision bigint NOT NULL DEFAULT 0;
