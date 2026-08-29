package engine

// Supervisor policy tests: the restart budget, the stable-run reset, and clean
// shutdown. The run function and clock are fakes — each simulated run reports
// how long it "lived" by advancing the clock.

import (
	"context"
	"errors"
	"testing"
	"time"
)

// scriptedRuns returns a supervisor clock and a run fn: run i advances the
// clock by lifespans[i] and crashes; running out of script fails the test.
func scriptedRuns(t *testing.T, lifespans []time.Duration) (supervisor, func(context.Context) error, *int) {
	t.Helper()
	now := time.Unix(1_000_000, 0)
	calls := 0
	s := supervisor{
		maxRestarts: 3,
		stableAfter: time.Minute,
		now:         func() time.Time { return now },
	}
	fn := func(context.Context) error {
		if calls >= len(lifespans) {
			t.Fatal("supervisor restarted more often than scripted")
		}
		now = now.Add(lifespans[calls])
		calls++
		return errors.New("boom")
	}
	return s, fn, &calls
}

// Four instant crashes in a row (the original run + 3 restarts) exhaust the
// budget.
func TestSupervisorGivesUpAfterBudget(t *testing.T) {
	s, fn, calls := scriptedRuns(t, []time.Duration{0, 0, 0, 0})

	err := s.run(context.Background(), fn)
	if err == nil {
		t.Fatal("supervisor never gave up")
	}
	if *calls != 4 {
		t.Fatalf("runs = %d, want 4 (original + 3 restarts)", *calls)
	}
}

// A run that stayed up past stableAfter earns the budget back: its crash is
// fresh, not chronic.
func TestSupervisorStableRunResetsBudget(t *testing.T) {
	// Two quick crashes, a stable 2-minute run, then quick crashes again:
	// without the reset the run after the stable one would already be the
	// last (4 runs total); the reset buys a fresh budget → 6 runs.
	s, fn, calls := scriptedRuns(t, []time.Duration{0, 0, 2 * time.Minute, 0, 0, 0})

	err := s.run(context.Background(), fn)
	if err == nil {
		t.Fatal("supervisor never gave up")
	}
	if *calls != 6 {
		t.Fatalf("runs = %d, want 6 (budget reset after the stable run)", *calls)
	}
}

// A run that returns because ctx ended is a shutdown, not a crash — no restart,
// no error, regardless of what the run returned.
func TestSupervisorCleanShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	s := supervisor{maxRestarts: 3, stableAfter: time.Minute, now: time.Now}

	err := s.run(ctx, func(ctx context.Context) error {
		calls++
		cancel()
		return errors.New("interrupted mid-pass")
	})
	if err != nil {
		t.Fatalf("shutdown returned error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("runs = %d, want 1 (no restart after shutdown)", calls)
	}
}
