package engine

import (
	"context"
	"time"

	"conductor/internal/storage"
	"conductor/internal/storage/db"

	"github.com/google/uuid"
)

// SensorStore is the observe side of the control loop. The Sensor ingests
// reality reported by host agents — heartbeats, per-replica phase/health,
// volume usage — and flags hosts that have gone silent. It only records
// observed facts; it never decides placement, so it carries no scheduling
// writes. Splitting it from OrchestratorStore keeps the read/observe path
// unable to mutate desired state by accident.
type SensorStore interface {
	// Fleet liveness. Agents heartbeat per host; ListStaleHosts surfaces those
	// whose last_heartbeat predates the cutoff so the Sensor can mark them down
	// and let the Orchestrator reschedule their replicas.
	RecordHostHeartbeat(ctx context.Context, hostID uuid.UUID, observedAt time.Time, status string) error
	ListStaleHosts(ctx context.Context, lastHeartbeatBefore time.Time) ([]db.Host, error)
	MarkHostDown(ctx context.Context, hostID uuid.UUID) error

	// Per-replica status as reported by the host agent. This is what self-heal
	// reconciles against: a crashed or unhealthy replica diverges from desired.
	RecordReplicaObservation(ctx context.Context, obs ReplicaObservation) error
	ListReplicasByHost(ctx context.Context, hostID uuid.UUID) ([]db.Replica, error)

	// Observed volume growth, later compared against desired_size_bytes to drive
	// resize/alerting.
	RecordVolumeObservedSize(ctx context.Context, volumeID uuid.UUID, observedBytes int64) error
}

// OrchestratorStore is the decide/act side. It reads desired state (the current
// deployment of each service and its per-region replica counts) and observed
// state (existing replicas, host capacity), bin-packs new replicas onto hosts,
// and drives each through its rollout lifecycle. Stateful services additionally
// go through volume placement and a single-writer lease.
type OrchestratorStore interface {
	// Desired state: the current deployment of every service and the replica
	// count it wants per region — the target the loop reconciles toward.
	ListCurrentDeployments(ctx context.Context) ([]db.Deployment, error)
	ListDeploymentRegions(ctx context.Context, deploymentID uuid.UUID) ([]db.DeploymentRegion, error)

	// Placement inputs: schedulable hosts in a region, plus the replicas already
	// pinned to a deployment or host, so the loop can bin-pack under CPU/RAM
	// headroom and compare desired-vs-actual replica counts. The service's
	// regional volume is read here too, before a stateful placement is committed.
	ListSchedulableHosts(ctx context.Context, region string) ([]db.Host, error)
	ListReplicasByDeployment(ctx context.Context, deploymentID uuid.UUID) ([]db.Replica, error)
	ListReplicasByHost(ctx context.Context, hostID uuid.UUID) ([]db.Replica, error)
	GetServiceVolume(ctx context.Context, serviceID uuid.UUID, region string) (db.Volume, error)

	// Single-row lifecycle advance (start -> health-gate -> shift -> drain),
	// guarded by CAS on replicas.revision so a stale read can't overwrite a phase
	// that already moved.
	SetReplicaPhase(ctx context.Context, replicaID uuid.UUID, phase string, expectRevision int64) error

	// Multi-row invariants — schedule+reserve and the stateful single-writer
	// lease — commit together. The loop reads + bin-packs above, then hands the
	// resulting writes to one short transaction. See storage.ReconcileTx.
	WithReconcileTx(ctx context.Context, fn func(storage.ReconcileTx) error) error
}

// ReplicaObservation is one host agent's report of a replica's runtime status,
// folded into the replicas row by the Sensor.
type ReplicaObservation struct {
	ReplicaID      uuid.UUID
	Phase          string
	Healthy        bool
	RestartCount   int32
	LastExitReason string
}
