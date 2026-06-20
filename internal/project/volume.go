package project

import (
	"context"
	"strings"

	"conductor/internal/storage/db"
	"conductor/internal/target"
)

const (
	// defaultVolumeRegion is where a freshly added volume's disk is requested.
	// `volume add` takes no --region, so it lands in a region the seed fleet
	// actually has hosts in; the reconcile loop places it from there.
	defaultVolumeRegion = "us-east-1"

	// defaultVolumeSizeBytes is the size a volume is created at. Size is a
	// mutable desired property, so this is just a starting point the user grows
	// with `volume update --size`.
	defaultVolumeSizeBytes = 1 << 30 // 1 GiB
)

// AddVolumeInput names the service to attach a volume to and the container mount
// path that is the volume's user-facing identity, and an optional initial size
// (0 ⇒ defaultVolumeSizeBytes). Size stays mutable afterward via ResizeVolume.
type AddVolumeInput struct {
	target.Target
	MountPath string
	SizeBytes int64
}

// AddVolume attaches a new volume to the service at the given mount path, sized
// at SizeBytes (or the default when unset). An unknown service surfaces as
// storage.ErrNotFound; a second volume at the same mount path as storage.ErrExists.
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

// ListVolumes returns the service's volumes, ordered by mount path. The service
// is resolved first so an unknown one surfaces as storage.ErrNotFound rather
// than an empty list.
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

// volumeName derives the engine's stable internal id from the user-facing mount
// path (e.g. "/var/lib/pg" → "var-lib-pg", "/" → "root"), keeping the two
// identities distinct while the mount path stays the CLI's handle.
func volumeName(mountPath string) string {
	trimmed := strings.Trim(mountPath, "/")
	if trimmed == "" {
		return "root"
	}
	return strings.ReplaceAll(trimmed, "/", "-")
}
