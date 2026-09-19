// Package logbuf keeps the engine's most recent log lines in memory and hands
// them to live subscribers. It sits behind an io.Writer, so the slog handler
// tees into it beside stderr and the lines are byte-for-byte what a file tail
// used to show — the UI parses the same TextHandler output either way.
//
// Each line carries a monotonic id. That id is what a reader passes back to
// resume, which is what lets a reconnecting browser continue rather than start
// over, and what makes the buffer a replay log rather than a snapshot.
package logbuf

import (
	"strings"
	"sync"
)

// DefaultCapacity is the number of lines held for replay. At LOG_LEVEL=DEBUG
// the engine writes a few hundred lines a minute, so this is roughly the last
// ten minutes: enough that opening the Logs page shows real history, small
// enough to stay a couple of megabytes.
const DefaultCapacity = 5000

// subscriberBuffer is how far a reader may lag before it is dropped. A slow
// reader must never block Write: Write runs inline on the engine's reconcile
// tick, so backpressure from a stalled browser would be backpressure on
// reconciliation.
const subscriberBuffer = 256

// Line is one log record and the id a reader resumes from.
type Line struct {
	ID   uint64
	Text string
}

// Ring is a fixed-size buffer of recent lines plus the set of live
// subscribers. Safe for concurrent use: the slog handler writes from whichever
// goroutine logged, and each SSE connection reads from its own.
type Ring struct {
	mu     sync.Mutex
	lines  []Line
	start  int
	count  int
	nextID uint64

	subs   map[uint64]chan Line
	nextSu uint64
}

func New(capacity int) *Ring {
	if capacity < 1 {
		capacity = 1
	}
	return &Ring{
		lines:  make([]Line, capacity),
		nextID: 1,
		subs:   make(map[uint64]chan Line),
	}
}

// Write is the io.Writer the log handler tees into. One handler write is one
// record, but split on newlines anyway so a multi-line value can never land in
// the buffer as a single line the UI would fail to parse.
func (r *Ring) Write(p []byte) (int, error) {
	for raw := range strings.SplitSeq(string(p), "\n") {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		r.append(raw)
	}
	return len(p), nil
}

func (r *Ring) append(text string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	line := Line{ID: r.nextID, Text: text}
	r.nextID++

	r.lines[(r.start+r.count)%len(r.lines)] = line
	if r.count == len(r.lines) {
		r.start = (r.start + 1) % len(r.lines)
	} else {
		r.count++
	}

	for id, ch := range r.subs {
		select {
		case ch <- line:
		default:
			// Closing rather than skipping tells the reader it has a hole, so
			// it reconnects and replays from its last id instead of silently
			// serving a log with lines missing from the middle.
			delete(r.subs, id)
			close(ch)
		}
	}
}

// SubscribeSince registers a subscriber and takes its replay backlog in the
// same critical section, so no line can slip between the two and the caller
// never de-duplicates. since 0 means a fresh reader — it gets the last `last`
// lines; any other value means a resume and gets everything after that id.
// Lines older than the buffer are simply gone: a reader that was away longer
// than the ring is deep resumes from the oldest line still held.
//
// The returned cancel is idempotent and must be called; the channel is closed
// either by cancel or by the reader falling too far behind.
func (r *Ring) SubscribeSince(since uint64, last int) ([]Line, <-chan Line, func()) {
	r.mu.Lock()
	defer r.mu.Unlock()

	backlog := r.sinceLocked(since)
	if since == 0 {
		backlog = r.lastLocked(last)
	}

	id := r.nextSu
	r.nextSu++
	ch := make(chan Line, subscriberBuffer)
	r.subs[id] = ch

	return backlog, ch, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if c, ok := r.subs[id]; ok {
			delete(r.subs, id)
			close(c)
		}
	}
}

func (r *Ring) sinceLocked(id uint64) []Line {
	out := make([]Line, 0, r.count)
	for i := 0; i < r.count; i++ {
		if l := r.lines[(r.start+i)%len(r.lines)]; l.ID > id {
			out = append(out, l)
		}
	}
	return out
}

func (r *Ring) lastLocked(n int) []Line {
	if n < 0 {
		n = 0
	}
	if n > r.count {
		n = r.count
	}
	out := make([]Line, 0, n)
	for i := r.count - n; i < r.count; i++ {
		out = append(out, r.lines[(r.start+i)%len(r.lines)])
	}
	return out
}
