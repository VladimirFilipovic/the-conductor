package project

import (
	"context"
	"strings"

	"conductor/internal/storage/db"
	"conductor/internal/target"
)

const (
	// `volume add` takes no --region, so a new volume lands in a region the
	// seed fleet actually has hosts in; the reconcile loop places it from there.
	defaultVolumeRegion = "us-east-1"

	// Just a starting point — size stays mutable via `volume update --size`.
	defaultVolumeSizeBytes = 1 << 30 // 1 GiB
)

// AddVolumeInput: the mount path is the volume's user-facing identity;
// SizeBytes 0 ⇒ defaultVolumeSizeBytes, mutable afterward via ResizeVolume.
type AddVolumeInput struct {
	target.Target
	MountPath string
	SizeBytes int64
}

// AddVolume attaches a volume at the given mount path. An unknown service ⇒
// storage.ErrNotFound; a second volume at the same mount path ⇒ storage.ErrExists.
func (s *Service) AddVolume(ctx context.Context, in AddVolumeInput) (db.Volume, error) {
	svc, err := s.store.GetService(ctx, in.Project, in.Service)
	if err != nil {
		return db.Volume{}, err
	}
	size := in.SizeBytes
	if size <= 0 {
		size = defaultVolumeSizeBytes
	}
	return s.store.CreateVolume(ctx, svc.ID, volumeName(in.MountPath), defaultVolumeRegion, in.MountPath, size)
}

// ListVolumes returns the service's volumes, ordered by mount path; the service
// is resolved first so an unknown one errs instead of an empty list.
func (s *Service) ListVolumes(ctx context.Context, t target.Target) ([]db.Volume, error) {
	if _, err := s.store.GetService(ctx, t.Project, t.Service); err != nil {
		return nil, err
	}
	return s.store.ListVolumesByService(ctx, t.Project, t.Service)
}

// ResizeVolume patches the desired size of the volume at mountPath; the reconcile
// loop grows the disk to match (§4b grow-only).
func (s *Service) ResizeVolume(ctx context.Context, t target.Target, mountPath string, sizeBytes int64) (db.Volume, error) {
	svc, err := s.store.GetService(ctx, t.Project, t.Service)
	if err != nil {
		return db.Volume{}, err
	}
	return s.store.UpdateVolumeSize(ctx, svc.ID, mountPath, sizeBytes)
}

// RemoveVolume detaches and deletes the volume at mountPath. A volume still
// pinned by a replica cannot be deleted (FK) — scale the service down first.
func (s *Service) RemoveVolume(ctx context.Context, t target.Target, mountPath string) (db.Volume, error) {
	svc, err := s.store.GetService(ctx, t.Project, t.Service)
	if err != nil {
		return db.Volume{}, err
	}
	return s.store.DeleteVolume(ctx, svc.ID, mountPath)
}

// volumeName derives the engine's stable internal id from the mount path
// ("/var/lib/pg" → "var-lib-pg", "/" → "root"); the mount path stays the CLI's handle.
func volumeName(mountPath string) string {
	trimmed := strings.Trim(mountPath, "/")
	if trimmed == "" {
		return "root"
	}
	return strings.ReplaceAll(trimmed, "/", "-")
}
