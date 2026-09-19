package logbuf

import (
	"fmt"
	"testing"
	"time"
)

func write(t *testing.T, r *Ring, text string) {
	t.Helper()
	if _, err := r.Write([]byte(text + "\n")); err != nil {
		t.Fatalf("write %q: %v", text, err)
	}
}

func ids(lines []Line) []uint64 {
	out := make([]uint64, 0, len(lines))
	for _, l := range lines {
		out = append(out, l.ID)
	}
	return out
}

func sameIDs(got []Line, want ...uint64) bool {
	g := ids(got)
	if len(g) != len(want) {
		return false
	}
	for i := range g {
		if g[i] != want[i] {
			return false
		}
	}
	return true
}

func TestFreshReaderGetsBacklogThenLiveLines(t *testing.T) {
	r := New(10)
	write(t, r, "one")
	write(t, r, "two")

	backlog, live, cancel := r.SubscribeSince(0, 10)
	defer cancel()

	if !sameIDs(backlog, 1, 2) {
		t.Fatalf("backlog ids = %v, want [1 2]", ids(backlog))
	}
	if backlog[0].Text != "one" {
		t.Fatalf("backlog[0].Text = %q, want %q", backlog[0].Text, "one")
	}

	write(t, r, "three")
	select {
	case l := <-live:
		if l.ID != 3 || l.Text != "three" {
			t.Fatalf("live line = %+v, want id 3 %q", l, "three")
		}
	case <-time.After(time.Second):
		t.Fatal("no live line delivered")
	}
}

func TestOldestLinesFallOutOfTheRing(t *testing.T) {
	r := New(3)
	for i := 1; i <= 5; i++ {
		write(t, r, fmt.Sprintf("line %d", i))
	}

	backlog, _, cancel := r.SubscribeSince(0, 10)
	defer cancel()

	if !sameIDs(backlog, 3, 4, 5) {
		t.Fatalf("backlog ids = %v, want [3 4 5]", ids(backlog))
	}
}

func TestResumeReplaysOnlyAfterTheGivenID(t *testing.T) {
	r := New(10)
	for i := 1; i <= 4; i++ {
		write(t, r, fmt.Sprintf("line %d", i))
	}

	backlog, _, cancel := r.SubscribeSince(2, 10)
	defer cancel()

	if !sameIDs(backlog, 3, 4) {
		t.Fatalf("backlog ids = %v, want [3 4]", ids(backlog))
	}
}

func TestFreshReaderBacklogIsCappedAtTail(t *testing.T) {
	r := New(10)
	for i := 1; i <= 6; i++ {
		write(t, r, fmt.Sprintf("line %d", i))
	}

	backlog, _, cancel := r.SubscribeSince(0, 2)
	defer cancel()

	if !sameIDs(backlog, 5, 6) {
		t.Fatalf("backlog ids = %v, want [5 6]", ids(backlog))
	}
}

// A reader that stops draining must be dropped, never block the writer: Write
// runs inline on the engine's reconcile tick.
func TestSlowSubscriberIsDroppedNotBlocked(t *testing.T) {
	r := New(subscriberBuffer * 4)
	_, live, cancel := r.SubscribeSince(0, 0)
	defer cancel()

	done := make(chan struct{})
	go func() {
		for i := range subscriberBuffer * 2 {
			_, _ = r.Write(fmt.Appendf(nil, "line %d\n", i))
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("writer blocked on a subscriber that stopped reading")
	}

	// The first subscriberBuffer lines are already queued; the drop shows up
	// as a closed channel on the read past them.
	for range subscriberBuffer + 1 {
		if _, ok := <-live; !ok {
			return
		}
	}
	t.Fatal("subscriber was never dropped")
}

func TestWriteSplitsLinesAndSkipsBlanks(t *testing.T) {
	r := New(10)
	if _, err := r.Write([]byte("first\n\nsecond\n")); err != nil {
		t.Fatalf("write: %v", err)
	}

	backlog, _, cancel := r.SubscribeSince(0, 10)
	defer cancel()

	if !sameIDs(backlog, 1, 2) {
		t.Fatalf("backlog ids = %v, want [1 2]", ids(backlog))
	}
	if backlog[1].Text != "second" {
		t.Fatalf("backlog[1].Text = %q, want %q", backlog[1].Text, "second")
	}
}

func TestCancelIsIdempotent(t *testing.T) {
	r := New(10)
	_, live, cancel := r.SubscribeSince(0, 0)
	cancel()
	cancel()

	if _, ok := <-live; ok {
		t.Fatal("channel still open after cancel")
	}
}
