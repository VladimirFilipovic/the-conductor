package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"conductor/internal/storage/db"

	"github.com/google/uuid"
)

// volumeQuerier is the volume slice of Querier, backing `conductor volume`.
// Volumes key off the service, not the environment service — a volume outlives
// any single deployment.
type volumeQuerier interface {
	CreateVolume(ctx context.Context, serviceID uuid.UUID, name, region, mountPath string, sizeBytes int64) (db.Volume, error)
	ListVolumesByService(ctx context.Context, projectName, service string) ([]db.Volume, error)
	UpdateVolumeSize(ctx context.Context, serviceID uuid.UUID, mountPath string, sizeBytes int64) (db.Volume, error)
	DeleteVolume(ctx context.Context, serviceID uuid.UUID, mountPath string) (db.Volume, error)
}

func (q querier) CreateVolume(ctx context.Context, serviceID uuid.UUID, name, region, mountPath string, sizeBytes int64) (db.Volume, error) {
	v, err := q.queries.CreateVolume(ctx, db.CreateVolumeParams{
		ServiceID:        serviceID,
		Name:             name,
		Region:           region,
		MountPath:        mountPath,
		DesiredSizeBytes: sizeBytes,
	})
	if uniqueViolation(err) {
		return db.Volume{}, fmt.Errorf("volume at %q: %w", mountPath, ErrExists)
	}
	if err != nil {
		return db.Volume{}, err
	}
	return v, nil
}

func (q querier) ListVolumesByService(ctx context.Context, projectName, service string) ([]db.Volume, error) {
	return q.queries.ListVolumesByService(ctx, db.ListVolumesByServiceParams{ProjectName: projectName, Name: service})
}

func (q querier) UpdateVolumeSize(ctx context.Context, serviceID uuid.UUID, mountPath string, sizeBytes int64) (db.Volume, error) {
	v, err := q.queries.UpdateVolumeSize(ctx, db.UpdateVolumeSizeParams{
		DesiredSizeBytes: sizeBytes,
		ServiceID:        serviceID,
		MountPath:        mountPath,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return db.Volume{}, fmt.Errorf("volume at %q: %w", mountPath, ErrNotFound)
	}
	if err != nil {
		return db.Volume{}, err
	}
	return v, nil
}

func (q querier) DeleteVolume(ctx context.Context, serviceID uuid.UUID, mountPath string) (db.Volume, error) {
	v, err := q.queries.DeleteVolume(ctx, db.DeleteVolumeParams{ServiceID: serviceID, MountPath: mountPath})
	if errors.Is(err, sql.ErrNoRows) {
		return db.Volume{}, fmt.Errorf("volume at %q: %w", mountPath, ErrNotFound)
	}
	// A replica still pins this volume (replicas.volume_id FK); `down` the service
	// first so the reconcile loop reaps the replicas, then the disk can go.
	if fkViolation(err) {
		return db.Volume{}, fmt.Errorf("volume at %q is still attached to a replica: scale the service down first", mountPath)
	}
	if err != nil {
		return db.Volume{}, err
	}
	return v, nil
}
