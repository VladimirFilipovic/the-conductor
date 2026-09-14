package project

import (
	"context"
	"fmt"
	"strings"

	"conductor/internal/config"
	"conductor/internal/storage/db"
	"conductor/internal/target"

	"github.com/google/uuid"
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
	id, err := serviceID(ctx, s.store, in.Target)
	if err != nil {
		return db.Volume{}, err
	}
	size := in.SizeBytes
	if size <= 0 {
		size = defaultVolumeSizeBytes
	}
	return s.store.CreateVolume(ctx, id, volumeName(in.MountPath), defaultVolumeRegion, in.MountPath, size)
}

// ListVolumes returns the service's volumes, ordered by mount path; the service
// is resolved first so an unknown one errs instead of an empty list.
func (s *Service) ListVolumes(ctx context.Context, t target.Target) ([]db.Volume, error) {
	if _, err := serviceID(ctx, s.store, t); err != nil {
		return nil, err
	}
	return s.store.ListVolumesByService(ctx, t.Project, t.Service)
}

// ResizeOutcome is ResizeVolume's answer: the patched row plus the engine's
// likely first verdict, so the caller can say up front whether the grow starts
// on the next tick or waits for host space.
type ResizeOutcome struct {
	Volume db.Volume
	// WaitingForSpace: the host's volume budget can't absorb the new size right
	// now. The request is kept — the engine re-checks every tick and approves
	// the grow the moment the host has room (another volume leaves, disk is
	// added); there is no failed state to clear.
	WaitingForSpace bool
	// ShortfallBytes is how much room the host is missing; 0 when it fits.
	ShortfallBytes int64
}

// ResizeVolume patches the desired size of the volume at mountPath; the reconcile
// loop grows the disk to match (§4b grow-only). The floor is what's ON DISK,
// not the previous desired: a desired below the observed size would never
// converge (the agent never shrinks) and would sit as permanent drift, but
// lowering a not-yet-applied grow back down to the disk is how an operator
// takes back a request the host can't hold.
//
// The space advisory is exactly that — advisory. It reuses the placer's
// DiskBudget with the default knobs, so an engine started with a non-default
// volume-budget flag may disagree at the margin; the engine's answer is the one
// that counts, this one just saves the operator a round-trip to `volume list`.
func (s *Service) ResizeVolume(ctx context.Context, t target.Target, mountPath string, sizeBytes int64) (ResizeOutcome, error) {
	id, err := serviceID(ctx, s.store, t)
	if err != nil {
		return ResizeOutcome{}, err
	}
	cur, err := s.store.GetVolume(ctx, id, mountPath)
	if err != nil {
		return ResizeOutcome{}, err
	}
	if sizeBytes == cur.DesiredSizeBytes {
		return ResizeOutcome{}, fmt.Errorf("%w: volume at %q is already %d bytes", ErrInvalid, mountPath, sizeBytes)
	}
	if cur.ObservedSizeBytes.Valid && sizeBytes < cur.ObservedSizeBytes.Int64 {
		return ResizeOutcome{}, fmt.Errorf("%w: volume at %q has %d bytes on disk and resize is grow-only; %d requested",
			ErrInvalid, mountPath, cur.ObservedSizeBytes.Int64, sizeBytes)
	}
	vol, err := s.store.UpdateVolumeSize(ctx, id, mountPath, sizeBytes)
	if err != nil {
		return ResizeOutcome{}, err
	}
	out := ResizeOutcome{Volume: vol}
	if !vol.HostID.Valid {
		return out, nil // not placed yet: the placer sizes it at creation
	}
	c, err := s.store.HostVolumeCommitment(ctx, vol.HostID.UUID)
	if err != nil {
		return out, err
	}
	// CommittedBytes already includes the size just written — the same
	// "desired is a reservation" view the engine's ledger takes.
	if budget := config.DefaultPlacement().DiskBudget(c.DiskBytes); c.CommittedBytes > budget {
		out.WaitingForSpace = true
		out.ShortfallBytes = c.CommittedBytes - budget
	}
	return out, nil
}

// RemoveVolume detaches and deletes the volume at mountPath. A volume still
// pinned by a replica cannot be deleted (FK) — scale the service down first.
func (s *Service) RemoveVolume(ctx context.Context, t target.Target, mountPath string) (db.Volume, error) {
	id, err := serviceID(ctx, s.store, t)
	if err != nil {
		return db.Volume{}, err
	}
	return s.store.DeleteVolume(ctx, id, mountPath)
}

// VolumeStore is the volume slice, backing `conductor volume`. Volumes key off
// the service, not the environment service — a volume outlives any single
// deployment.
type VolumeStore interface {
	CreateVolume(ctx context.Context, serviceID uuid.UUID, name, region, mountPath string, sizeBytes int64) (db.Volume, error)
	ListVolumesByService(ctx context.Context, projectName, service string) ([]db.Volume, error)
	GetVolume(ctx context.Context, serviceID uuid.UUID, mountPath string) (db.Volume, error)
	UpdateVolumeSize(ctx context.Context, serviceID uuid.UUID, mountPath string, sizeBytes int64) (db.Volume, error)
	HostVolumeCommitment(ctx context.Context, hostID uuid.UUID) (db.HostVolumeCommitmentRow, error)
	DeleteVolume(ctx context.Context, serviceID uuid.UUID, mountPath string) (db.Volume, error)
}

// serviceID resolves the project-domain half of every volume operation: volumes
// key off the service row, and resolving it up front turns an unknown service
// into storage.ErrNotFound instead of an empty list or a no-op write.
func serviceID(ctx context.Context, st ProjectStore, t target.Target) (uuid.UUID, error) {
	svc, err := st.GetService(ctx, t.Project, t.Service)
	return svc.ID, err
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
