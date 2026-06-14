-- Dev fleet for the scheduler/reconcile loop to place onto. NOT a migration:
-- this is data, not schema, so it lives outside db/migrations (which goose would
-- otherwise re-run on migrate-fresh). Apply with `make seed`.
--
-- The fleet is intentionally heterogeneous and multi-region so bin-packing under
-- CPU/RAM/disk/region constraints (orchestration-engine.md) has real choices to
-- make: a mix of small/medium/large boxes per region, plus one cordoned host so
-- placement has to skip an otherwise-viable candidate.
--
-- Idempotent: re-running upserts by hostname and refreshes the heartbeat, so a
-- re-seed keeps the fleet looking alive without churning host ids (replicas
-- reference host_id, so stable ids matter).

INSERT INTO hosts (region, hostname, cpu_millicores, mem_bytes, disk_bytes, labels, status, last_heartbeat)
VALUES
	-- us-east-1: full size ladder — somewhere for everything to land.
	('us-east-1', 'ue1-small-1',  2000,  pg_size_bytes('4GB'),  pg_size_bytes('80GB'),  '{"class":"small","arch":"amd64"}',  'ready', now()),
	('us-east-1', 'ue1-medium-1', 4000,  pg_size_bytes('16GB'), pg_size_bytes('200GB'), '{"class":"medium","arch":"amd64"}', 'ready', now()),
	('us-east-1', 'ue1-large-1',  8000,  pg_size_bytes('32GB'), pg_size_bytes('500GB'), '{"class":"large","arch":"amd64"}',  'ready', now()),

	-- us-west-2: bigger boxes only — forces region-aware placement when a service
	-- pins us-west and asks for a small footprint.
	('us-west-2', 'uw2-medium-1', 4000,  pg_size_bytes('16GB'), pg_size_bytes('200GB'), '{"class":"medium","arch":"amd64"}', 'ready', now()),
	('us-west-2', 'uw2-large-1',  8000,  pg_size_bytes('32GB'), pg_size_bytes('500GB'), '{"class":"large","arch":"arm64"}',  'ready', now()),

	-- eu-west-1: small region, and one host cordoned (drained for maintenance) so
	-- the scheduler must avoid an otherwise-fitting candidate.
	('eu-west-1', 'ew1-small-1',  2000,  pg_size_bytes('4GB'),  pg_size_bytes('80GB'),  '{"class":"small","arch":"arm64"}',  'ready',    now()),
	('eu-west-1', 'ew1-medium-1', 4000,  pg_size_bytes('16GB'), pg_size_bytes('200GB'), '{"class":"medium","arch":"arm64"}', 'cordoned', now())
ON CONFLICT (hostname) DO UPDATE SET
	region         = EXCLUDED.region,
	cpu_millicores = EXCLUDED.cpu_millicores,
	mem_bytes      = EXCLUDED.mem_bytes,
	disk_bytes     = EXCLUDED.disk_bytes,
	labels         = EXCLUDED.labels,
	status         = EXCLUDED.status,
	last_heartbeat = now();
