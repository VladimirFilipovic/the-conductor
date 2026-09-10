package engine

// Gateway tests over a real gRPC stack (bufconn): the Session protocol
// (hello → full state, observations → Sensor, notify → push) and the full
// loop — a REAL agentsim.Agent converging a deployment through the wire,
// then chaos flowing the same way.

import (
	"context"
	"net"
	"testing"
	"time"

	"conductor/agentsim"
	"conductor/internal/domain"
	"conductor/internal/storage/db"
	"conductor/proto/agentpb"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// serveGateway hosts g on an in-memory listener and returns a connected client.
func serveGateway(t *testing.T, g *Gateway) agentpb.AgentGatewayClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	agentpb.RegisterAgentGatewayServer(srv, g)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial bufconn: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return agentpb.NewAgentGatewayClient(conn)
}

func recvState(t *testing.T, stream agentpb.AgentGateway_SessionClient) *agentpb.HostState {
	t.Helper()
	type result struct {
		msg *agentpb.ServerMessage
		err error
	}
	ch := make(chan result, 1)
	go func() {
		m, err := stream.Recv()
		ch <- result{m, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("recv: %v", r.err)
		}
		return r.msg.GetState()
	case <-time.After(5 * time.Second):
		t.Fatal("no downlink message within 5s")
		return nil
	}
}

// waitFor polls cond until it holds or the deadline hits.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestGatewaySessionProtocol(t *testing.T) {
	ms := newMemStore()
	hostID := uuid.New()
	ms.hosts = append(ms.hosts, db.Host{ID: hostID, Region: "eu-west-1", Hostname: "h1", Status: "ready"})
	dep := &memDeployment{id: uuid.New(), environmentServiceID: uuid.New(), region: "eu-west-1", isCurrent: true, status: "rolling_out"}
	ms.deployments = append(ms.deployments, dep)
	replica := &memReplica{id: uuid.New(), deploymentID: dep.id, region: "eu-west-1", hostID: hostID, phase: domain.ReplicaPhaseScheduling}
	ms.replicas = append(ms.replicas, replica)

	g := NewGateway("", NewSensor(ms), ms, nil)
	client := serveGateway(t, g)

	ctx := t.Context()
	stream, err := client.Session(ctx)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	if err := stream.Send(&agentpb.AgentMessage{Msg: &agentpb.AgentMessage_Hello{
		Hello: &agentpb.Hello{HostId: hostID.String()},
	}}); err != nil {
		t.Fatalf("hello: %v", err)
	}

	// Connect → full snapshot with the scheduling replica.
	state := recvState(t, stream)
	if len(state.Replicas) != 1 || state.Replicas[0].Id != replica.id.String() || state.Replicas[0].Phase != "scheduling" {
		t.Fatalf("initial state = %v, want the scheduling replica", state.Replicas)
	}

	// Uplink observation lands in the store through the Sensor.
	err = stream.Send(&agentpb.AgentMessage{Msg: &agentpb.AgentMessage_Observation{
		Observation: &agentpb.ReplicaObservation{ReplicaId: replica.id.String(), Phase: "active", Healthy: true},
	}})
	if err != nil {
		t.Fatalf("observation: %v", err)
	}
	waitFor(t, "observation to commit", func() bool {
		var ok bool
		ms.withLock(func() { ok = replica.phase == domain.ReplicaPhaseActive && replica.healthy })
		return ok
	})

	// Uplink heartbeat stamps the host.
	err = stream.Send(&agentpb.AgentMessage{Msg: &agentpb.AgentMessage_Heartbeat{
		Heartbeat: &agentpb.Heartbeat{Status: "ready"},
	}})
	if err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	waitFor(t, "heartbeat to commit", func() bool {
		var ok bool
		ms.withLock(func() { ok = ms.hosts[0].LastHeartbeat.Valid })
		return ok
	})

	// A change + notify pushes the fresh full state.
	ms.withLock(func() { replica.phase = domain.ReplicaPhaseDraining })
	g.notify(ctx, hostID)
	state = recvState(t, stream)
	if len(state.Replicas) != 1 || state.Replicas[0].Phase != "draining" {
		t.Fatalf("post-notify state = %v, want draining replica", state.Replicas)
	}

	// A notify that changed nothing the agent sees must not push (cache hit):
	// prove it by a second, real change arriving as the very next message.
	g.notify(ctx, hostID)
	ms.withLock(func() { replica.phase = domain.ReplicaPhaseReaped }) // drops off ListReplicasByHost
	g.notify(ctx, hostID)
	state = recvState(t, stream)
	if len(state.Replicas) != 0 {
		t.Fatalf("post-reap state = %v, want empty (dedupe pushed a stale state?)", state.Replicas)
	}
}

// TestGatewayFullLoopWithAgent closes the whole circle over real gRPC: the
// engine schedules, the gateway pushes state, an agentsim agent fakes the
// containers healthy, the deployment completes — then a chaos crash-loop
// flows back through the same wire until crashLooping fails the deployment.
func TestGatewayFullLoopWithAgent(t *testing.T) {
	l := newLoop(t)
	region := "eu-west-1"
	slot := replicaSlot{uuid.New(), region}
	hostID := l.addHost(region, 2000, 1<<30, 1<<30)
	v1 := l.deploy(slot, uuid.New(), 1, false)

	g := NewGateway("", l.sensor, l.ms, nil)
	client := serveGateway(t, g)

	ctx := t.Context()
	agent := agentsim.NewAgent(client, hostID, "h1", region, 10*time.Millisecond)
	agent.Start(ctx)
	defer agent.Stop()

	// Drive engine ticks; the trigger's job (NOTIFY on change) is played by a
	// forced push after each tick.
	converge := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			l.tick()
			g.pushState(ctx, hostID, true)
			if cond() {
				return
			}
			time.Sleep(30 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", what)
	}

	converge("deployment active through the real wire", func() bool {
		var ok bool
		l.ms.withLock(func() { ok = l.ms.deployment(v1).status == string(domain.DeploymentActive) })
		return ok
	})

	var replicaID string
	l.ms.withLock(func() {
		r := l.ms.replicas[0]
		if r.hostID != hostID || !r.healthy {
			t.Errorf("replica not converged on host: host=%s healthy=%v", r.hostID, r.healthy)
		}
		replicaID = r.id.String()
	})

	// Chaos through the agent: crash-loop until restart_count breaches
	// RestartMax and the reconciler fails the deployment.
	if !agent.CrashLoop(replicaID) {
		t.Fatalf("agent does not run replica %s", replicaID)
	}
	converge("crash-loop to fail the deployment", func() bool {
		var ok bool
		l.ms.withLock(func() { ok = l.ms.deployment(v1).status == string(domain.DeploymentFailed) })
		return ok
	})
}

// TestGatewayForcePushAll: the LISTEN reconnect hook resends the full state to
// every connected session even though nothing changed (cache bypass).
func TestGatewayForcePushAll(t *testing.T) {
	ms := newMemStore()
	g := NewGateway("", NewSensor(ms), ms, nil)
	client := serveGateway(t, g)
	ctx := t.Context()

	streams := make([]agentpb.AgentGateway_SessionClient, 3)
	for i := range streams {
		hostID := uuid.New()
		ms.hosts = append(ms.hosts, db.Host{ID: hostID, Region: "eu-west-1", Hostname: "h", Status: "ready"})
		stream, err := client.Session(ctx)
		if err != nil {
			t.Fatalf("session: %v", err)
		}
		if err := stream.Send(&agentpb.AgentMessage{Msg: &agentpb.AgentMessage_Hello{
			Hello: &agentpb.Hello{HostId: hostID.String()},
		}}); err != nil {
			t.Fatalf("hello: %v", err)
		}
		recvState(t, stream) // connect snapshot
		streams[i] = stream
	}
	waitFor(t, "all sessions registered", func() bool { return len(g.connectedHosts()) == len(streams) })

	g.ForcePushAll(ctx)
	for i, stream := range streams {
		if state := recvState(t, stream); state == nil {
			t.Fatalf("session %d: force push carried no state", i)
		}
	}
}
