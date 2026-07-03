package engine

import (
	"context"
	"time"

	"conductor/internal/storage/db"

	"github.com/google/uuid"
)

type SensorStore interface {
	RecordHostHeartbeat(ctx context.Context, hostID uuid.UUID, observedAt time.Time, status string) error
	ListStaleHosts(ctx context.Context, lastHeartbeatBefore time.Time) ([]db.Host, error)
	MarkHostDown(ctx context.Context, hostID uuid.UUID) error
	RecordReplicaObservation(ctx context.Context, obs ReplicaObservation) error
	ListReplicasByHost(ctx context.Context, hostID uuid.UUID) ([]db.Replica, error)
	RecordVolumeObservedSize(ctx context.Context, volumeID uuid.UUID, observedBytes int64) error
}

type ReplicaObservation struct {
	ReplicaID      uuid.UUID
	Phase          string
	Healthy        bool
	RestartCount   int32
	LastExitReason string
}

type Sensor struct {
	store SensorStore
}

// run processes incoming host heartbeats and replica observations, marking
// stale hosts down so the Reconciler can reschedule their replicas. Blocking.
// TODO: implement the observation loop; parks on ctx until then so the
// supervisor doesn't treat the sensor as failed at startup.
func (s *Sensor) run(ctx context.Context) error {
	<-ctx.Done()
	return nil
}
