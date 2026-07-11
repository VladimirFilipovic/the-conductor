-- +goose Up

-- served_revisions is the traffic switch for blue/green rollouts: per slot
-- (environment_service_id, region), the deployment whose replicas the router
-- may serve. Replica health/phase alone can't express this — new-revision
-- replicas pass health checks while still invisible to traffic, and the old
-- revision keeps serving until the pointer flips. The flip commits in the same
-- reconcile tx as the outgoing drain batch (all new healthy → shift → drain),
-- so the switch is atomic: no tick serves neither side or both.
CREATE TABLE served_revisions (
	environment_service_id uuid        NOT NULL REFERENCES environment_services ON DELETE CASCADE,
	region                 text        NOT NULL REFERENCES regions(name),
	deployment_id          uuid        NOT NULL REFERENCES deployments ON DELETE RESTRICT,
	updated_at             timestamptz NOT NULL DEFAULT now(),
	PRIMARY KEY (environment_service_id, region)
);

-- Reverse lookup for FK checks and "what serves this deployment" queries.
CREATE INDEX served_revisions_deployment_id_idx ON served_revisions (deployment_id);

-- +goose Down
DROP TABLE served_revisions;
