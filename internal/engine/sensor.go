package engine

import (
	"context"
	"errors"
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

// tick processes incoming host heartbeats and replica observations, marking
// stale hosts down so the Reconciler can reschedule their replicas.
func (s *Sensor) tick(ctx context.Context) error {
	return errors.New("not implemented")
}
