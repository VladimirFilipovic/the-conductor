-- +goose Up

-- hosts.status carried two owners in one column: the agent/watchdog wrote
-- ready/notready, the operator wrote cordoned/draining, and each write had to
-- CASE around the other's values. Split by owner. host_healthy is observed
-- state (heartbeat → true, silence → false); status is operator intent
-- (open/cordoned/draining). Schedulable = host_healthy AND status = 'open',
-- so a draining host that goes silent keeps its drain instead of losing it
-- to a 'notready' overwrite.
ALTER TABLE hosts
	ADD COLUMN host_healthy     bool NOT NULL DEFAULT true,
	ADD COLUMN drain_started_at timestamptz;

-- The old CHECK has to go before the backfill: 'open' is not in it.
ALTER TABLE hosts DROP CONSTRAINT hosts_status_check;
UPDATE hosts SET host_healthy = (status <> 'notready');
UPDATE hosts SET status = 'open' WHERE status IN ('ready', 'notready');
UPDATE hosts SET drain_started_at = now() WHERE status = 'draining';

ALTER TABLE hosts ALTER COLUMN status SET DEFAULT 'open';
ALTER TABLE hosts ADD CONSTRAINT hosts_status_check
	CHECK (status IN ('open', 'cordoned', 'draining'));
-- drain_started_at exists only while a drain is in flight: it is the stalled-
-- drain clock, and a stale one on a cordoned host would read as a live drain.
ALTER TABLE hosts ADD CONSTRAINT hosts_drain_started_check
	CHECK ((status = 'draining') = (drain_started_at IS NOT NULL));

-- +goose Down
ALTER TABLE hosts DROP CONSTRAINT hosts_drain_started_check;
ALTER TABLE hosts DROP CONSTRAINT hosts_status_check;
UPDATE hosts SET status = CASE WHEN host_healthy THEN 'ready' ELSE 'notready' END
WHERE status = 'open';
ALTER TABLE hosts ALTER COLUMN status SET DEFAULT 'ready';
ALTER TABLE hosts ADD CONSTRAINT hosts_status_check
	CHECK (status IN ('ready', 'notready', 'draining', 'cordoned'));
ALTER TABLE hosts DROP COLUMN drain_started_at, DROP COLUMN host_healthy;
