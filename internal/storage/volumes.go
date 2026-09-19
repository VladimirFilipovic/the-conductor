package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"conductor/internal/domain"
	"conductor/internal/storage/db"

	"github.com/google/uuid"
)

// volumeQuerier is the volume slice of Querier, backing `conductor volume`.
// Volumes key off the environment service: a volume outlives any single
// deployment, but not the binding — data is what environments isolate.
type volumeQuerier interface {
	CreateVolume(ctx context.Context, envServiceID uuid.UUID, name, region, mountPath string, sizeBytes int64) (db.Volume, error)
	ListVolumesByEnvironmentService(ctx context.Context, projectName, environment, service string) ([]db.Volume, error)
	GetVolume(ctx context.Context, envServiceID uuid.UUID, mountPath string) (db.Volume, error)
	// UpdateVolumeSize writes desired and remembers the size it replaced; the
	// status is the engine's to move.
	UpdateVolumeSize(ctx context.Context, envServiceID uuid.UUID, mountPath string, sizeBytes int64) (db.Volume, error)
	// RevertVolumeSize restores the pre-update desired size of a resize_pending
	// volume, once. ErrNotFound when the volume is not resize_pending or has
	// nothing to revert to.
	RevertVolumeSize(ctx context.Context, envServiceID uuid.UUID, mountPath string) (db.Volume, error)
	// GetHost is the host a placed volume sits on — the resize advisory's
	// disk size input.
	GetHost(ctx context.Context, hostID uuid.UUID) (db.Host, error)
	DeleteVolume(ctx context.Context, envServiceID uuid.UUID, mountPath string) (db.Volume, error)
}

func (q querier) CreateVolume(ctx context.Context, envServiceID uuid.UUID, name, region, mountPath string, sizeBytes int64) (db.Volume, error) {
	v, err := q.queries.CreateVolume(ctx, db.CreateVolumeParams{
		EnvironmentServiceID: envServiceID,
		Name:                 name,
		Region:               region,
		MountPath:            mountPath,
		DesiredSizeBytes:     sizeBytes,
	})
	if uniqueViolation(err) {
		return db.Volume{}, fmt.Errorf("volume at %q: %w", mountPath, ErrExists)
	}
	if err != nil {
		return db.Volume{}, err
	}
	return v, nil
}

func (q querier) ListVolumesByEnvironmentService(ctx context.Context, projectName, environment, service string) ([]db.Volume, error) {
	return q.queries.ListVolumesByEnvironmentService(ctx, db.ListVolumesByEnvironmentServiceParams{
		ProjectName: projectName,
		Environment: environment,
		Service:     service,
	})
}

func (q querier) GetVolume(ctx context.Context, envServiceID uuid.UUID, mountPath string) (db.Volume, error) {
	v, err := q.queries.GetVolume(ctx, db.GetVolumeParams{EnvironmentServiceID: envServiceID, MountPath: mountPath})
	if errors.Is(err, sql.ErrNoRows) {
		return db.Volume{}, fmt.Errorf("volume at %q: %w", mountPath, ErrNotFound)
	}
	return v, err
}

func (q querier) GetHost(ctx context.Context, hostID uuid.UUID) (db.Host, error) {
	h, err := q.queries.GetHost(ctx, hostID)
	if errors.Is(err, sql.ErrNoRows) {
		return db.Host{}, fmt.Errorf("host %s: %w", hostID, ErrNotFound)
	}
	return h, err
}

func (q querier) UpdateVolumeSize(ctx context.Context, envServiceID uuid.UUID, mountPath string, sizeBytes int64) (db.Volume, error) {
	v, err := q.queries.UpdateVolumeSize(ctx, db.UpdateVolumeSizeParams{
		DesiredSizeBytes:     sizeBytes,
		EnvironmentServiceID: envServiceID,
		MountPath:            mountPath,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return db.Volume{}, fmt.Errorf("volume at %q: %w", mountPath, ErrNotFound)
	}
	if err != nil {
		return db.Volume{}, err
	}
	return v, nil
}

func (q querier) RevertVolumeSize(ctx context.Context, envServiceID uuid.UUID, mountPath string) (db.Volume, error) {
	v, err := q.queries.RevertVolumeSize(ctx, db.RevertVolumeSizeParams{EnvironmentServiceID: envServiceID, MountPath: mountPath})
	if errors.Is(err, sql.ErrNoRows) {
		return db.Volume{}, fmt.Errorf("volume at %q: %w", mountPath, ErrNotFound)
	}
	return v, err
}

// SizingOf unpacks a volume row into the domain's sizing view once, so the
// engine, the agent downlink and the CLI all read drift, settle and committed
// bytes off the same predicates instead of each handling the NULL observed
// column.
func SizingOf(v db.Volume) domain.VolumeSizing {
	return domain.VolumeSizing{
		Status:   domain.VolumeStatus(v.Status),
		Desired:  v.DesiredSizeBytes,
		Observed: v.ObservedSizeBytes.Int64,
	}
}

func (q querier) DeleteVolume(ctx context.Context, envServiceID uuid.UUID, mountPath string) (db.Volume, error) {
	v, err := q.queries.DeleteVolume(ctx, db.DeleteVolumeParams{EnvironmentServiceID: envServiceID, MountPath: mountPath})
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
