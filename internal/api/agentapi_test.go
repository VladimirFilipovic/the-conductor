package api

// AgentAPI downlink tests: the per-host mailbox and the push machinery driven
// directly, without a gRPC stream — Session is only the glue around them.

import (
	"context"
	"sync"
	"testing"
	"time"

	"conductor/internal/storage/db"
	"conductor/proto/agentpb"

	"github.com/google/uuid"
)

// fakeAgentStore answers ListReplicasByHost from a script: each call takes the
// next entry, optionally blocking on its gate first so tests can interleave
// concurrent reads deterministically.
type fakeAgentStore struct {
	mu      sync.Mutex
	script  []scriptedRead
	entered chan struct{} // one send per call, as it begins
}

type scriptedRead struct {
	rows []db.Replica
	gate <-chan struct{} // nil = return immediately
}

func (f *fakeAgentStore) ListReplicasByHost(context.Context, uuid.UUID) ([]db.Replica, error) {
	f.mu.Lock()
	next := f.script[0]
	f.script = f.script[1:]
	f.mu.Unlock()
	if f.entered != nil {
		f.entered <- struct{}{}
	}
	if next.gate != nil {
		<-next.gate
	}
	return next.rows, nil
}

func (f *fakeAgentStore) ListAgentHosts(context.Context) ([]db.Host, error) { return nil, nil }

func newTestAgentAPI(store *fakeAgentStore) *AgentAPI {
	return NewAgentAPI("", nil, store, nil)
}

func replicaRows(phases ...string) []db.Replica {
	rows := make([]db.Replica, len(phases))
	for i, p := range phases {
		rows[i] = db.Replica{ID: pinnedID(byte(i + 1)), Phase: p}
	}
	return rows
}

// receive drains one message or fails; a nil return means the mailbox was
// empty within the wait.
func receive(t *testing.T, s *agentSession) *agentpb.ServerMessage {
	t.Helper()
	select {
	case m := <-s.send:
		return m
	case <-time.After(50 * time.Millisecond):
		return nil
	}
}

func TestMailboxKeepsLatestOnly(t *testing.T) {
	s := newAgentSession(pinnedID(1))
	for i := 1; i <= 3; i++ {
		s.push(&agentpb.ServerMessage{State: &agentpb.HostState{Replicas: make([]*agentpb.Replica, i)}})
	}
	m := receive(t, s)
	if m == nil || len(m.State.Replicas) != 3 {
		t.Fatalf("got %v, want the last of three pushes", m)
	}
	if receive(t, s) != nil {
		t.Error("mailbox held more than one message")
	}
}

// A notification that changed nothing the agent sees must not wake the stream;
// the periodic resync (force) always does.
func TestPushStateSkipsUnchanged(t *testing.T) {
	store := &fakeAgentStore{script: []scriptedRead{
		{rows: replicaRows("active")},
		{rows: replicaRows("active")},
		{rows: replicaRows("active", "starting")},
		{rows: replicaRows("active", "starting")},
	}}
	api := newTestAgentAPI(store)
	s := newAgentSession(pinnedID(1))
	api.register(s)
	ctx := context.Background()

	api.pushState(ctx, s.hostID, true)
	if m := receive(t, s); m == nil || len(m.State.Replicas) != 1 {
		t.Fatalf("connect push = %v, want 1 replica", m)
	}
	api.pushState(ctx, s.hostID, false)
	if receive(t, s) != nil {
		t.Error("unchanged state woke the stream")
	}
	api.pushState(ctx, s.hostID, false)
	if m := receive(t, s); m == nil || len(m.State.Replicas) != 2 {
		t.Fatalf("changed state push = %v, want 2 replicas", m)
	}
	api.pushState(ctx, s.hostID, true)
	if receive(t, s) == nil {
		t.Error("forced resync did not push")
	}
}

// Two overlapping triggers for one host must leave the agent on the NEWER
// snapshot: the slow first read may not land after the fast second one.
func TestPushStateSerializesPerHost(t *testing.T) {
	release := make(chan struct{})
	store := &fakeAgentStore{
		entered: make(chan struct{}, 2),
		script: []scriptedRead{
			{rows: replicaRows("active"), gate: release}, // older state, slow read
			{rows: replicaRows("active", "starting")},    // newer state, fast read
		},
	}
	api := newTestAgentAPI(store)
	s := newAgentSession(pinnedID(1))
	api.register(s)
	ctx := context.Background()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); api.pushState(ctx, s.hostID, false) }()
	<-store.entered // first read is in flight and blocked
	go func() { defer wg.Done(); api.pushState(ctx, s.hostID, false) }()
	// Give the second trigger time to reach the store if nothing held it back.
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()

	m := receive(t, s)
	if m == nil || len(m.State.Replicas) != 2 {
		t.Fatalf("final state = %v, want the newer 2-replica snapshot", m)
	}
}

// A reconnect replaces the session; the old stream's teardown must not evict it.
func TestUnregisterKeepsReplacementSession(t *testing.T) {
	api := newTestAgentAPI(&fakeAgentStore{})
	old := newAgentSession(pinnedID(1))
	api.register(old)
	replacement := newAgentSession(pinnedID(1))
	api.register(replacement)

	api.unregister(old)
	if got := api.session(pinnedID(1)); got != replacement {
		t.Fatalf("session after old teardown = %v, want the replacement", got)
	}
	api.unregister(replacement)
	if api.session(pinnedID(1)) != nil {
		t.Error("replacement teardown left a session behind")
	}
}
