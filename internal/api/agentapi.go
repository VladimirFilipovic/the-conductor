package api

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	"conductor/internal/domain"
	"conductor/internal/storage"
	"conductor/internal/storage/db"
	"conductor/proto/agentpb"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/protobuf/proto"
)

const (
	// agentResyncInterval paces the periodic full-state push to every connected
	// agent. It is the safety net under the NOTIFY fast path: a dropped
	// notification (LISTEN reconnect gap) costs at most one interval of
	// latency, never a stuck agent.
	agentResyncInterval = 60 * time.Second
	// agentListenRetryInterval paces bind retries (typically EADDRINUSE from a
	// predecessor still releasing the port); agentListenMaxFailures bounds them
	// so a permanently taken port fails the process instead of spinning.
	agentListenRetryInterval = 5 * time.Second
	agentListenMaxFailures   = 5
)

// AgentAPIStore is the read side the AgentAPI serves agents from.
type AgentAPIStore interface {
	ListReplicasByHost(ctx context.Context, hostID uuid.UUID) ([]db.Replica, error)
	ListVolumesByHost(ctx context.Context, hostID uuid.UUID) ([]db.Volume, error)
	// ListAgentHosts is agent discovery: every host, scheduling status
	// ignored — a notready host's agent must still enroll and heartbeat,
	// or a demoted host could never heal back to ready.
	ListAgentHosts(ctx context.Context) ([]db.Host, error)
}

// AgentAPI is the agent-facing gRPC boundary: one bidi Session stream per
// host. Uplink (heartbeats, observations) funnels into ObservedState — this
// layer adds no rules of its own. Downlink pushes the host's FULL state
// (replicas and volumes), never deltas: on connect, on a replicas_changed notification, and on
// the periodic resync, so any single message is sufficient and a lost one
// self-heals.
type AgentAPI struct {
	agentpb.UnimplementedAgentAPIServer
	addr     string
	observed *ObservedState
	store    AgentAPIStore
	// listen blocks delivering change notifications; injectable so tests feed
	// notifications from a channel instead of a live LISTEN connection.
	// onReconnect fires each time the underlying LISTEN is (re)established.
	listen func(ctx context.Context, onChange func(hostID uuid.UUID), onReconnect func()) error

	mu       sync.Mutex
	sessions map[uuid.UUID]*agentSession
}

func NewAgentAPI(addr string, observed *ObservedState, store AgentAPIStore, listen func(context.Context, func(uuid.UUID), func()) error) *AgentAPI {
	return &AgentAPI{
		addr:     addr,
		observed: observed,
		store:    store,
		listen:   listen,
		sessions: map[uuid.UUID]*agentSession{},
	}
}

// agentSession is one connected host's downlink. send is a latest-wins
// mailbox of size 1: states supersede each other, so a slow agent never
// backpressures the server and always wakes to the freshest picture.
type agentSession struct {
	hostID uuid.UUID
	send   chan *agentpb.ServerMessage

	// pushMu serializes read→compare→push per host. Without it two overlapping
	// notifications could read state A then B, push B, then push the older A —
	// leaving the agent on a stale picture until the next resync.
	pushMu sync.Mutex
	// lastHash digests the last state pushed, so a NOTIFY that changed nothing
	// the agent cares about doesn't wake the stream. A digest, not the message:
	// comparison is a fixed-size compare and memory stays flat however many
	// replicas a host runs.
	lastHash [sha256.Size]byte
}

func newAgentSession(hostID uuid.UUID) *agentSession {
	return &agentSession{hostID: hostID, send: make(chan *agentpb.ServerMessage, 1)}
}

func (s *agentSession) push(m *agentpb.ServerMessage) {
	for {
		select {
		case s.send <- m:
			return
		default:
			select {
			case <-s.send: // drop the stale state, retry with the fresh one
			default:
			}
		}
	}
}

// Run serves gRPC until ctx ends. Blocking; Server runs it next to the
// OperatorAPI HTTP server.
func (a *AgentAPI) Run(ctx context.Context) error {
	lis, err := a.listenWithRetry(ctx)
	if err != nil {
		return err
	}
	if lis == nil {
		return nil // ctx ended while waiting to bind
	}
	srv := grpc.NewServer(
		// Server-side pings keep NAT entries fresh and detect dead pipes; the
		// enforcement floor stops a misbehaving client from ping-flooding.
		grpc.KeepaliveParams(keepalive.ServerParameters{Time: 30 * time.Second, Timeout: 10 * time.Second}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 10 * time.Second, PermitWithoutStream: true}),
	)
	agentpb.RegisterAgentAPIServer(srv, a)
	slog.Info("agentapi -> serving", "addr", a.addr)

	go func() {
		<-ctx.Done()
		srv.Stop() // Session streams are infinite; GracefulStop would hang forever
	}()
	go a.resyncLoop(ctx)
	go func() {
		onChange := func(hostID uuid.UUID) { a.pushState(ctx, hostID, false) }
		onReconnect := func() { a.pushAll(ctx) }
		if err := a.listen(ctx, onChange, onReconnect); err != nil {
			slog.Error("agentapi: listener stopped", "err", err)
		}
	}()

	if err := srv.Serve(lis); err != nil && ctx.Err() == nil {
		return fmt.Errorf("agentapi: serve: %w", err)
	}
	slog.Info("agentapi -> done")
	return nil
}

// listenWithRetry binds a.addr, retrying transient failures on a paced loop.
// A nil, nil return means ctx ended before a bind succeeded.
func (a *AgentAPI) listenWithRetry(ctx context.Context) (net.Listener, error) {
	timer := time.NewTimer(agentListenRetryInterval)
	defer timer.Stop()

	failures := 0
	for {
		lis, err := net.Listen("tcp", a.addr)
		if err == nil {
			return lis, nil
		}
		failures++
		if failures >= agentListenMaxFailures {
			return nil, fmt.Errorf("agentapi: listen %s: %d consecutive failures: %w", a.addr, failures, err)
		}
		slog.Warn("agentapi: listen failed, retrying", "addr", a.addr, "err", err, "consecutive", failures)
		select {
		case <-ctx.Done():
			return nil, nil
		case <-timer.C:
		}
		timer.Reset(agentListenRetryInterval)
	}
}

func (a *AgentAPI) resyncLoop(ctx context.Context) {
	t := time.NewTicker(agentResyncInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		for _, hostID := range a.connectedHosts() {
			a.pushState(ctx, hostID, true)
		}
	}
}

// pushAll resends the full state to every connected agent. It is the LISTEN
// reconnect hook: notifications fired while no connection was listening are
// gone, so every host must be treated as changed. Pushes run off the caller's
// goroutine so a slow store read never stalls the listener that just came back.
func (a *AgentAPI) pushAll(ctx context.Context) {
	hosts := a.connectedHosts()
	slog.Info("agentapi -> force push", "sessions", len(hosts))
	go func() {
		for _, hostID := range hosts {
			a.pushState(ctx, hostID, true)
		}
	}()
}

func (a *AgentAPI) connectedHosts() []uuid.UUID {
	a.mu.Lock()
	defer a.mu.Unlock()
	hosts := make([]uuid.UUID, 0, len(a.sessions))
	for id := range a.sessions {
		hosts = append(hosts, id)
	}
	return hosts
}

func (a *AgentAPI) session(hostID uuid.UUID) *agentSession {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sessions[hostID]
}

// pushState re-reads the host and pushes when the state the agent sees
// actually changed (or unconditionally when force). The whole sequence runs
// under the session's pushMu, so concurrent triggers for one host are applied
// in read order and the agent always ends on the newest snapshot.
func (a *AgentAPI) pushState(ctx context.Context, hostID uuid.UUID, force bool) {
	s := a.session(hostID)
	if s == nil {
		return
	}
	s.pushMu.Lock()
	defer s.pushMu.Unlock()

	state, sum, err := a.hostState(ctx, hostID)
	if err != nil {
		slog.Warn("agentapi: host state read failed", "host", hostID, "err", err)
		return
	}
	if !force && sum == s.lastHash {
		return
	}
	s.lastHash = sum
	s.push(&agentpb.ServerMessage{State: state})
}

// hostState reads the host's replicas and volumes and digests the wire
// encoding. Deterministic marshal pins map order; repeated fields keep the
// order sorted here, so equality means equality, not iteration luck.
func (a *AgentAPI) hostState(ctx context.Context, hostID uuid.UUID) (*agentpb.HostState, [sha256.Size]byte, error) {
	rows, err := a.store.ListReplicasByHost(ctx, hostID)
	if err != nil {
		return nil, [sha256.Size]byte{}, err
	}
	replicas := make([]*agentpb.Replica, len(rows))
	for i, r := range rows {
		replicas[i] = &agentpb.Replica{Id: r.ID.String(), Phase: r.Phase}
	}
	slices.SortFunc(replicas, func(x, y *agentpb.Replica) int { return strings.Compare(x.Id, y.Id) })

	vols, err := a.store.ListVolumesByHost(ctx, hostID)
	if err != nil {
		return nil, [sha256.Size]byte{}, err
	}
	volumes := make([]*agentpb.Volume, len(vols))
	for i, v := range vols {
		volumes[i] = &agentpb.Volume{Id: v.ID.String(), MountPath: v.MountPath, SizeBytes: volumeTargetSize(v)}
	}
	slices.SortFunc(volumes, func(x, y *agentpb.Volume) int { return strings.Compare(x.Id, y.Id) })
	state := &agentpb.HostState{Replicas: replicas, Volumes: volumes}

	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(state)
	if err != nil {
		return nil, [sha256.Size]byte{}, fmt.Errorf("agentapi: marshal host state: %w", err)
	}
	return state, sha256.Sum256(b), nil
}

// volumeTargetSize is the size the agent should have on disk. The engine is
// the capacity gate for a grow: a bumped desired size stays invisible to the
// agent until the reconciler flips the volume to resizing, so an agent never
// grows a disk the host can't hold. Outside a resize the agent is told what it
// already has; a never-observed volume (fresh placement) takes desired as its
// first size.
func volumeTargetSize(v db.Volume) int64 {
	if domain.VolumeStatus(v.Status) == domain.VolumeResizing || !v.ObservedSizeBytes.Valid {
		return v.DesiredSizeBytes
	}
	return v.ObservedSizeBytes.Int64
}

func (a *AgentAPI) register(s *agentSession) {
	a.mu.Lock()
	a.sessions[s.hostID] = s
	a.mu.Unlock()
}

func (a *AgentAPI) unregister(s *agentSession) {
	a.mu.Lock()
	// Only drop the entry if it is still ours — a reconnect may have already
	// replaced the session, and the old stream must not evict the new one.
	if a.sessions[s.hostID] == s {
		delete(a.sessions, s.hostID)
	}
	a.mu.Unlock()
}

// Session handles one agent's lifetime: Hello binds the stream to a host,
// then uplink messages flow into ObservedState while the downlink goroutine
// drains the session mailbox. Bad uplink messages are logged and dropped, not
// fatal — a sim agent racing a reap is normal, not a protocol violation.
func (a *AgentAPI) Session(stream agentpb.AgentAPI_SessionServer) error {
	ctx := stream.Context()
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	hello := first.GetHello()
	if hello == nil {
		return fmt.Errorf("agentapi: first message must be hello")
	}
	hostID, err := uuid.Parse(hello.HostId)
	if err != nil {
		return fmt.Errorf("agentapi: hello with bad host id %q", hello.HostId)
	}

	s := newAgentSession(hostID)
	a.register(s)
	defer a.unregister(s)
	slog.Info("agentapi -> agent connected", "host", hostID)
	defer slog.Info("agentapi -> agent disconnected", "host", hostID)

	// Full snapshot on (re)connect — the level-triggered opening move.
	a.pushState(ctx, hostID, true)

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case m := <-s.send:
				if err := stream.Send(m); err != nil {
					return // broken stream surfaces on Recv below
				}
			}
		}
	}()

	for {
		msg, err := stream.Recv()
		if err != nil {
			return nil // agent hung up (or server stopping); reconnect is its move
		}
		if err := a.applyUplink(ctx, hostID, msg); err != nil {
			slog.Warn("agentapi: uplink message dropped", "host", hostID, "err", err)
		}
	}
}

// applyUplink translates one agent message into an ObservedState call.
func (a *AgentAPI) applyUplink(ctx context.Context, hostID uuid.UUID, msg *agentpb.AgentMessage) error {
	switch m := msg.Msg.(type) {
	case *agentpb.AgentMessage_Heartbeat:
		return a.observed.RecordHeartbeat(ctx, hostID, m.Heartbeat.Status)
	case *agentpb.AgentMessage_Observation:
		o := m.Observation
		replicaID, err := uuid.Parse(o.ReplicaId)
		if err != nil {
			return fmt.Errorf("agentapi: observation with bad replica id %q", o.ReplicaId)
		}
		return a.observed.ObserveReplica(ctx, storage.ReplicaObservation{
			ReplicaID:      replicaID,
			Phase:          o.Phase,
			Healthy:        o.Healthy,
			RestartCount:   o.RestartCount,
			LastExitReason: o.LastExitReason,
		})
	case *agentpb.AgentMessage_VolumeObservation:
		o := m.VolumeObservation
		volumeID, err := uuid.Parse(o.VolumeId)
		if err != nil {
			return fmt.Errorf("agentapi: volume observation with bad volume id %q", o.VolumeId)
		}
		return a.observed.ObserveVolumeSize(ctx, volumeID, o.ObservedSizeBytes)
	default:
		return fmt.Errorf("agentapi: unexpected uplink message %T", msg.Msg)
	}
}

func (a *AgentAPI) ListHosts(ctx context.Context, _ *agentpb.ListHostsRequest) (*agentpb.ListHostsResponse, error) {
	hosts, err := a.store.ListAgentHosts(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*agentpb.Host, len(hosts))
	for i, h := range hosts {
		out[i] = &agentpb.Host{Id: h.ID.String(), Region: h.Region, Hostname: h.Hostname}
	}
	return &agentpb.ListHostsResponse{Hosts: out}, nil
}
