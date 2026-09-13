package engine

// Watchdog unit tests: the staleness cutoff arithmetic, the sweep's mark-down
// fan-out, and the apiserver-keyed startup grace. The stale-filter SQL itself
// is covered by the e2e loop test (e2e_test.go) through the in-memory store.

import (
	"context"
	"testing"
	"time"

	"conductor/internal/storage/db"

	"github.com/google/uuid"
)

type fakeWatchdogStore struct {
	dead []db.Host
	// apiserverStarted is what OldestLiveApiserverStart reports; zero = no live
	// apiserver, so the sweep must never render a death verdict.
	apiserverStarted time.Time

	notReadyCutoffs []time.Time
	deadCutoffs     []time.Time
	downed          []uuid.UUID
}

func (f *fakeWatchdogStore) MarkStaleHostsNotReady(_ context.Context, before time.Time) (int64, error) {
	f.notReadyCutoffs = append(f.notReadyCutoffs, before)
	return 0, nil
}

func (f *fakeWatchdogStore) ListDeadHosts(_ context.Context, before time.Time) ([]db.Host, error) {
	f.deadCutoffs = append(f.deadCutoffs, before)
	return f.dead, nil
}

func (f *fakeWatchdogStore) MarkHostDown(_ context.Context, hostID uuid.UUID, _ time.Time) error {
	f.downed = append(f.downed, hostID)
	return nil
}

func (f *fakeWatchdogStore) OldestLiveApiserverStart(context.Context, time.Time) (time.Time, bool, error) {
	return f.apiserverStarted, !f.apiserverStarted.IsZero(), nil
}

func newTestWatchdog(store WatchdogStore, now time.Time) *Watchdog {
	return &Watchdog{store: store, now: func() time.Time { return now }}
}

// The sweep demotes briefly-silent hosts at the notready cutoff and marks
// every dead-listed host down at the (longer) death cutoff.
func TestSweepMarksStaleHostsDown(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	store := &fakeWatchdogStore{dead: []db.Host{{ID: pinnedID(1)}, {ID: pinnedID(2)}}, apiserverStarted: now.Add(-time.Hour)}

	if err := newTestWatchdog(store, now).sweepStaleHosts(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	wantNotReady := now.Add(-hostNotReadyAfter)
	if len(store.notReadyCutoffs) != 1 || !store.notReadyCutoffs[0].Equal(wantNotReady) {
		t.Fatalf("notready cutoff = %v, want %v", store.notReadyCutoffs, wantNotReady)
	}
	wantDead := now.Add(-hostDeadAfter)
	if len(store.deadCutoffs) != 1 || !store.deadCutoffs[0].Equal(wantDead) {
		t.Fatalf("dead cutoff = %v, want %v", store.deadCutoffs, wantDead)
	}
	if len(store.downed) != 2 || store.downed[0] != pinnedID(1) || store.downed[1] != pinnedID(2) {
		t.Fatalf("downed = %v, want both dead hosts", store.downed)
	}
}

// The death pass is only fair once some apiserver has been continuously live
// for a full death window — after a control-plane outage every heartbeat is
// stale by the plane's own absence, and agents get that window to reconnect.
// Demotion to notready still runs (reversible, harmless). With no live
// apiserver at all, nobody could have heartbeated: never fair.
func TestSweepStartupGraceSkipsDeathPass(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	store := &fakeWatchdogStore{dead: []db.Host{{ID: pinnedID(1)}}}
	s := newTestWatchdog(store, now)

	sweep := func(what string) {
		t.Helper()
		if err := s.sweepStaleHosts(context.Background()); err != nil {
			t.Fatalf("sweep (%s): %v", what, err)
		}
	}

	sweep("no live apiserver")
	if len(store.notReadyCutoffs) != 1 {
		t.Fatal("notready demotion skipped during startup grace")
	}
	if len(store.deadCutoffs) != 0 || len(store.downed) != 0 {
		t.Fatalf("death pass ran with no live apiserver: cutoffs=%v downed=%v", store.deadCutoffs, store.downed)
	}

	store.apiserverStarted = now.Add(-hostDeadAfter / 2)
	sweep("young apiserver")
	if len(store.downed) != 0 {
		t.Fatalf("death pass ran inside the apiserver's grace window: downed=%v", store.downed)
	}

	store.apiserverStarted = now.Add(-hostDeadAfter)
	sweep("apiserver past grace")
	if len(store.downed) != 1 {
		t.Fatal("death pass still skipped after the grace window elapsed")
	}
}
