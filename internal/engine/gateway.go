package engine

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

	"conductor/internal/storage"
	"conductor/internal/storage/db"
	"conductor/proto/agentpb"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/protobuf/proto"
)

// gatewayResyncInterval paces the periodic full-state push to every connected
// agent. It is the safety net under the NOTIFY fast path: a dropped
// notification (LISTEN reconnect gap) costs at most one interval of latency,
// never a stuck agent.
const gatewayResyncInterval = 60 * time.Second

// GatewayStore is the read side the gateway serves agents from.
type GatewayStore interface {
	ListReplicasByHost(ctx context.Context, hostID uuid.UUID) ([]db.Replica, error)
	// ListAgentHosts is agent discovery: every host, scheduling status
	// ignored — a notready host's agent must still enroll and heartbeat,
	// or a demoted host could never heal back to ready.
	ListAgentHosts(ctx context.Context) ([]db.Host, error)
}

// Gateway is the agent-facing gRPC boundary: one bidi Session stream per host.
// Uplink (heartbeats, observations) funnels into the Sensor — the gateway adds
// no rules of its own. Downlink pushes the host's FULL replica state, never
// deltas: on connect, on a replicas_changed notification, and on the periodic
// resync, so any single message is sufficient and a lost one self-heals.
type Gateway struct {
	agentpb.UnimplementedAgentGatewayServer
	addr   string
	sensor *Sensor
	store  GatewayStore
	// listen blocks delivering change notifications; injectable so tests feed
	// notifications from a channel instead of a live LISTEN connection.
	// onReconnect fires each time the underlying LISTEN is (re)established.
	listen func(ctx context.Context, onChange func(hostID uuid.UUID), onReconnect func()) error

	mu       sync.Mutex
	sessions map[uuid.UUID]*agentSession
	// cache holds a hash of the last state read per host, so a NOTIFY that
	// changed nothing the agent cares about (the trigger already filters most)
	// doesn't wake the stream. A digest, not the message: comparison is a
	// fixed-size compare and memory stays flat however many replicas a host
	// runs. Refreshed on every hostState read; resync bypasses it.
	cache map[uuid.UUID][sha256.Size]byte
}

func NewGateway(addr string, sensor *Sensor, store GatewayStore, listen func(context.Context, func(uuid.UUID), func()) error) *Gateway {
	return &Gateway{
		addr:     addr,
		sensor:   sensor,
		store:    store,
		listen:   listen,
		sessions: map[uuid.UUID]*agentSession{},
		cache:    map[uuid.UUID][sha256.Size]byte{},
	}
}

// agentSession is one connected host's downlink. send is a latest-wins
// mailbox of size 1: states supersede each other, so a slow agent never
// backpressures the gateway and always wakes to the freshest picture.
type agentSession struct {
	hostID uuid.UUID
	send   chan *agentpb.ServerMessage
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

// gatewayListenRetryInterval paces bind retries so a failed listen burns time
// like the engine/sensor loops burn ticks — the supervisor has no pacing of
// its own and relies on that (see Run's no-hot-loop note).
const gatewayListenRetryInterval = 5 * time.Second

// run serves gRPC until ctx ends. Blocking; runs under the supervisor next to
// the engine and sensor loops, restarting with them.
func (g *Gateway) run(ctx context.Context) error {
	lis, err := g.listenWithRetry(ctx)
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
	agentpb.RegisterAgentGatewayServer(srv, g)
	slog.Info("gateway -> serving", "addr", g.addr)

	go func() {
		<-ctx.Done()
		srv.Stop() // Session streams are infinite; GracefulStop would hang forever
	}()
	go g.resyncLoop(ctx)
	go func() {
		onChange := func(hostID uuid.UUID) { g.notify(ctx, hostID) }
		onReconnect := func() { g.ForcePushAll(ctx) }
		if err := g.listen(ctx, onChange, onReconnect); err != nil {
			slog.Error("gateway: listener stopped", "err", err)
		}
	}()

	if err := srv.Serve(lis); err != nil && ctx.Err() == nil {
		return fmt.Errorf("gateway: serve: %w", err)
	}
	slog.Info("gateway -> done")
	return nil
}

// listenWithRetry binds g.addr, retrying transient failures (typically
// EADDRINUSE from a predecessor still releasing the port) on a paced loop.
// A nil, nil return means ctx ended before a bind succeeded.
func (g *Gateway) listenWithRetry(ctx context.Context) (net.Listener, error) {
	timer := time.NewTimer(gatewayListenRetryInterval)
	defer timer.Stop()

	failures := 0
	for {
		lis, err := net.Listen("tcp", g.addr)
		if err == nil {
			return lis, nil
		}
		failures++
		if failures >= maxConsecutiveFailures {
			return nil, fmt.Errorf("gateway: listen %s: %d consecutive failures: %w", g.addr, failures, err)
		}
		slog.Warn("gateway: listen failed, retrying", "addr", g.addr, "err", err, "consecutive", failures)
		select {
		case <-ctx.Done():
			return nil, nil
		case <-timer.C:
		}
		timer.Reset(gatewayListenRetryInterval)
	}
}

func (g *Gateway) resyncLoop(ctx context.Context) {
	t := time.NewTicker(gatewayResyncInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		for _, hostID := range g.connectedHosts() {
			g.pushState(ctx, hostID, true)
		}
	}
}

// ForcePushAll resends the full state to every connected agent. It is the
// LISTEN reconnect hook: notifications fired while no connection was
// listening are gone, so every host must be treated as changed. Pushes run
// off the caller's goroutine so a slow store read never stalls the listener
// that just came back.
func (g *Gateway) ForcePushAll(ctx context.Context) {
	hosts := g.connectedHosts()
	slog.Info("gateway -> force push", "sessions", len(hosts))
	go func() {
		for _, hostID := range hosts {
			g.pushState(ctx, hostID, true)
		}
	}()
}

func (g *Gateway) connectedHosts() []uuid.UUID {
	g.mu.Lock()
	defer g.mu.Unlock()
	hosts := make([]uuid.UUID, 0, len(g.sessions))
	for id := range g.sessions {
		hosts = append(hosts, id)
	}
	return hosts
}

// notify is the NOTIFY fast path: re-read the host and push only if the state
// the agent sees actually changed.
func (g *Gateway) notify(ctx context.Context, hostID uuid.UUID) {
	g.pushState(ctx, hostID, false)
}

func (g *Gateway) pushState(ctx context.Context, hostID uuid.UUID, force bool) {
	g.mu.Lock()
	s := g.sessions[hostID]
	g.mu.Unlock()
	if s == nil {
		return
	}
	state, changed, err := g.hostState(ctx, hostID)
	if err != nil {
		slog.Warn("gateway: host state read failed", "host", hostID, "err", err)
		return
	}
	if changed || force {
		s.push(&agentpb.ServerMessage{State: state})
	}
}

// hostState reads the host's replicas, refreshes the cache, and reports
// whether the state differs from the last one read.
func (g *Gateway) hostState(ctx context.Context, hostID uuid.UUID) (*agentpb.HostState, bool, error) {
	rows, err := g.store.ListReplicasByHost(ctx, hostID)
	if err != nil {
		return nil, false, err
	}
	replicas := make([]*agentpb.Replica, len(rows))
	for i, r := range rows {
		replicas[i] = &agentpb.Replica{Id: r.ID.String(), Phase: r.Phase}
	}
	// Stable order so equality means equality, not iteration luck.
	slices.SortFunc(replicas, func(a, b *agentpb.Replica) int { return strings.Compare(a.Id, b.Id) })
	state := &agentpb.HostState{Replicas: replicas}
	sum, err := stateHash(state)
	if err != nil {
		return nil, false, err
	}

	g.mu.Lock()
	changed := g.cache[hostID] != sum
	g.cache[hostID] = sum
	g.mu.Unlock()
	return state, changed, nil
}

// stateHash digests the wire encoding. Deterministic marshal pins map order;
// repeated fields keep the order hostState already sorted them into.
func stateHash(state *agentpb.HostState) ([sha256.Size]byte, error) {
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(state)
	if err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("gateway: marshal host state: %w", err)
	}
	return sha256.Sum256(b), nil
}

func (g *Gateway) register(s *agentSession) {
	g.mu.Lock()
	g.sessions[s.hostID] = s
	g.mu.Unlock()
}

func (g *Gateway) unregister(s *agentSession) {
	g.mu.Lock()
	// Only drop the entry if it is still ours — a reconnect may have already
	// replaced the session, and the old stream must not evict the new one.
	if g.sessions[s.hostID] == s {
		delete(g.sessions, s.hostID)
		delete(g.cache, s.hostID)
	}
	g.mu.Unlock()
}

// Session handles one agent's lifetime: Hello binds the stream to a host,
// then uplink messages flow into the Sensor while the downlink goroutine
// drains the session mailbox. Bad uplink messages are logged and dropped, not
// fatal — a sim agent racing a reap is normal, not a protocol violation.
func (g *Gateway) Session(stream agentpb.AgentGateway_SessionServer) error {
	ctx := stream.Context()
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	hello := first.GetHello()
	if hello == nil {
		return fmt.Errorf("gateway: first message must be hello")
	}
	hostID, err := uuid.Parse(hello.HostId)
	if err != nil {
		return fmt.Errorf("gateway: hello with bad host id %q", hello.HostId)
	}

	s := &agentSession{hostID: hostID, send: make(chan *agentpb.ServerMessage, 1)}
	g.register(s)
	defer g.unregister(s)
	slog.Info("gateway -> agent connected", "host", hostID)
	defer slog.Info("gateway -> agent disconnected", "host", hostID)

	// Full snapshot on (re)connect — the level-triggered opening move.
	g.pushState(ctx, hostID, true)

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
		switch m := msg.Msg.(type) {
		case *agentpb.AgentMessage_Heartbeat:
			err = g.sensor.RecordHeartbeat(ctx, hostID, m.Heartbeat.Status)
		case *agentpb.AgentMessage_Observation:
			o := m.Observation
			replicaID, perr := uuid.Parse(o.ReplicaId)
			if perr != nil {
				err = perr
				break
			}
			err = g.sensor.ObserveReplica(ctx, storage.ReplicaObservation{
				ReplicaID:      replicaID,
				Phase:          o.Phase,
				Healthy:        o.Healthy,
				RestartCount:   o.RestartCount,
				LastExitReason: o.LastExitReason,
			})
		}
		if err != nil {
			slog.Warn("gateway: uplink message dropped", "host", hostID, "err", err)
		}
	}
}

func (g *Gateway) ListHosts(ctx context.Context, _ *agentpb.ListHostsRequest) (*agentpb.ListHostsResponse, error) {
	hosts, err := g.store.ListAgentHosts(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*agentpb.Host, len(hosts))
	for i, h := range hosts {
		out[i] = &agentpb.Host{Id: h.ID.String(), Region: h.Region, Hostname: h.Hostname}
	}
	return &agentpb.ListHostsResponse{Hosts: out}, nil
}
