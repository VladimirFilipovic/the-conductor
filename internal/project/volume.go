package project

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"conductor/internal/config"
	"conductor/internal/domain"
	"conductor/internal/storage"
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

// AddVolume attaches a volume at the given mount path to the target's
// environment service. An unknown project/environment/service or an unbound
// service ⇒ storage.ErrNotFound; a second volume at the same mount path in the
// same environment ⇒ storage.ErrExists (the same mount in another environment
// is a different disk).
func (s *Service) AddVolume(ctx context.Context, in AddVolumeInput) (db.Volume, error) {
	id, err := environmentServiceID(ctx, s.store, in.Target)
	if err != nil {
		return db.Volume{}, err
	}
	size := in.SizeBytes
	if size <= 0 {
		size = defaultVolumeSizeBytes
	}
	return s.store.CreateVolume(ctx, id, volumeName(in.MountPath), defaultVolumeRegion, in.MountPath, size)
}

// ListVolumes returns the environment service's volumes, ordered by mount
// path; the binding is resolved first so an unknown one errs instead of an
// empty list.
func (s *Service) ListVolumes(ctx context.Context, t target.Target) ([]db.Volume, error) {
	if _, err := environmentServiceID(ctx, s.store, t); err != nil {
		return nil, err
	}
	return s.store.ListVolumesByEnvironmentService(ctx, t.Project, t.Environment, t.Service)
}

// ResizeOutcome is ResizeVolume's answer: the patched row plus the engine's
// likely first verdict, so the caller can say up front whether the grow starts
// on the next tick or waits for host space.
type ResizeOutcome struct {
	Volume db.Volume
	// WaitingForSpace: the host's volume budget can't absorb the grow delta
	// right now, so the engine will park the volume as resize_pending. The
	// request is kept — the engine re-checks every tick and approves the grow
	// the moment the host has room (another volume leaves, disk is added), and
	// `volume revert` takes it back meanwhile; there is no failed state.
	WaitingForSpace bool
	// ShortfallBytes is how much room the host is missing; 0 when it fits.
	ShortfallBytes int64
}

// ResizeVolume requests a grow of the volume at mountPath; the reconcile loop
// grows the disk to match (§4b grow-only). Only desired moves here — status is
// the engine's — and only one grow may be in flight: the volume must be
// unplaced, or attached and converged. A drifting volume the engine has not
// classified yet (the tick between update and resize_pending/resizing) is
// refused too, so the operator's second thought goes through revert, never a
// second update that would lose the revert target.
//
// The space advisory is exactly that — advisory. It reuses the placer's
// DiskBudget with the default knobs, so an engine started with a non-default
// volume-budget flag may disagree at the margin; the engine's answer is the one
// that counts, this one just saves the operator a round-trip to `volume list`.
func (s *Service) ResizeVolume(ctx context.Context, t target.Target, mountPath string, sizeBytes int64) (ResizeOutcome, error) {
	id, err := environmentServiceID(ctx, s.store, t)
	if err != nil {
		return ResizeOutcome{}, err
	}
	cur, err := s.store.GetVolume(ctx, id, mountPath)
	if err != nil {
		return ResizeOutcome{}, err
	}
	sz := storage.SizingOf(cur)
	if sz.Status != domain.VolumePending && sz.Status != domain.VolumeAttached {
		return ResizeOutcome{}, fmt.Errorf("%w: volume at %q is %s; revert or wait for it to attach",
			ErrInvalid, mountPath, sz.Status)
	}
	if sz.Drifting() {
		return ResizeOutcome{}, fmt.Errorf("%w: volume at %q already has a grow requested (%d → %d bytes); revert first",
			ErrInvalid, mountPath, sz.Observed, sz.Desired)
	}
	if sizeBytes == sz.Desired {
		return ResizeOutcome{}, fmt.Errorf("%w: volume at %q is already %d bytes", ErrInvalid, mountPath, sizeBytes)
	}
	if sizeBytes < sz.Desired {
		return ResizeOutcome{}, fmt.Errorf("%w: volume at %q is %d bytes and resize is grow-only; %d requested",
			ErrInvalid, mountPath, sz.Desired, sizeBytes)
	}
	vol, err := s.store.UpdateVolumeSize(ctx, id, mountPath, sizeBytes)
	if err != nil {
		return ResizeOutcome{}, err
	}
	out := ResizeOutcome{Volume: vol}
	if !vol.HostID.Valid {
		return out, nil // not placed yet: the placer sizes it at creation
	}
	host, err := s.store.GetHost(ctx, vol.HostID.UUID)
	if err != nil {
		return out, err
	}
	onHost, err := s.store.ListVolumesByHost(ctx, host.ID)
	if err != nil {
		return out, err
	}
	// The same view the engine's ledger takes: every volume charges its
	// Committed() bytes. The volume just updated still commits what's on disk
	// (nothing is approved yet), so its grow delta is added explicitly — that
	// delta is exactly the resize item the engine will try to fit.
	var committed int64
	for _, v := range onHost {
		committed += storage.SizingOf(v).Committed()
	}
	need := committed + storage.SizingOf(vol).GrowDelta()
	if budget := config.DefaultPlacement().DiskBudget(host.DiskBytes); need > budget {
		out.WaitingForSpace = true
		out.ShortfallBytes = need - budget
	}
	return out, nil
}

// RevertVolume takes back a grow the host could not hold: desired returns to
// the size before `volume update`, once. Only a resize_pending volume
// qualifies — resizing is already a promise to the agent, attached has
// nothing outstanding, and a drifting volume the engine has not classified
// yet needs one more tick. Status stays with the engine, which settles the
// volume back to attached on its next tick (observed >= desired now holds).
func (s *Service) RevertVolume(ctx context.Context, t target.Target, mountPath string) (db.Volume, error) {
	id, err := environmentServiceID(ctx, s.store, t)
	if err != nil {
		return db.Volume{}, err
	}
	cur, err := s.store.GetVolume(ctx, id, mountPath)
	if err != nil {
		return db.Volume{}, err
	}
	sz := storage.SizingOf(cur)
	switch {
	case sz.Status == domain.VolumeResizePending:
	case sz.Status == domain.VolumeAttached && sz.Drifting():
		return db.Volume{}, fmt.Errorf("%w: volume at %q has a grow the engine has not classified yet; retry in a moment",
			ErrInvalid, mountPath)
	default:
		return db.Volume{}, fmt.Errorf("%w: volume at %q is %s; nothing to revert", ErrInvalid, mountPath, sz.Status)
	}
	vol, err := s.store.RevertVolumeSize(ctx, id, mountPath)
	if errors.Is(err, storage.ErrNotFound) {
		// The engine moved the volume between the two reads (approved it, or
		// settled it) — the row exists, the state doesn't anymore.
		return db.Volume{}, fmt.Errorf("%w: volume at %q changed state while reverting; retry", ErrInvalid, mountPath)
	}
	return vol, err
}

// RemoveVolume detaches and deletes the volume at mountPath. A volume still
// pinned by a replica cannot be deleted (FK) — scale the service down first.
func (s *Service) RemoveVolume(ctx context.Context, t target.Target, mountPath string) (db.Volume, error) {
	id, err := environmentServiceID(ctx, s.store, t)
	if err != nil {
		return db.Volume{}, err
	}
	return s.store.DeleteVolume(ctx, id, mountPath)
}

// VolumeStore is the volume slice, backing `conductor volume`. Volumes key off
// the environment service: a volume outlives any single deployment, but data is
// what environments isolate, so one service bound into two environments owns
// two disks.
type VolumeStore interface {
	CreateVolume(ctx context.Context, envServiceID uuid.UUID, name, region, mountPath string, sizeBytes int64) (db.Volume, error)
	ListVolumesByEnvironmentService(ctx context.Context, projectName, environment, service string) ([]db.Volume, error)
	GetVolume(ctx context.Context, envServiceID uuid.UUID, mountPath string) (db.Volume, error)
	UpdateVolumeSize(ctx context.Context, envServiceID uuid.UUID, mountPath string, sizeBytes int64) (db.Volume, error)
	RevertVolumeSize(ctx context.Context, envServiceID uuid.UUID, mountPath string) (db.Volume, error)
	// GetHost + ListVolumesByHost feed the resize advisory: the host's disk
	// and what its volumes already commit.
	GetHost(ctx context.Context, hostID uuid.UUID) (db.Host, error)
	ListVolumesByHost(ctx context.Context, hostID uuid.UUID) ([]db.Volume, error)
	DeleteVolume(ctx context.Context, envServiceID uuid.UUID, mountPath string) (db.Volume, error)
}

// environmentServiceID resolves the project-domain half of every volume
// operation: volumes key off the environment_services row, the same one
// Deploy/Scale target, and resolving it up front turns an unknown environment,
// service or binding into storage.ErrNotFound instead of an empty list or a
// no-op write.
func environmentServiceID(ctx context.Context, st DeploymentStore, t target.Target) (uuid.UUID, error) {
	row, err := st.GetEnvironmentService(ctx, t.Project, t.Environment, t.Service)
	return row.ID, err
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
