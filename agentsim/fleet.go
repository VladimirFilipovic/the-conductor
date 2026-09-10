package agentsim

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"

	"conductor/proto/agentpb"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// hostDiscoveryInterval paces re-listing the fleet, so hosts added after
// startup get an agent without restarting the fleet.
const hostDiscoveryInterval = 30 * time.Second

// Fleet is the simulated host-agent fleet: it discovers hosts through the gateway's
// ListHosts (agents never touch the database), spawns one Agent per host, and
// serves the chaos control API the CLI (and later chaos-ui) drives.
type Fleet struct {
	GatewayAddr string
	ControlAddr string
	Tick        time.Duration

	client agentpb.AgentGatewayClient

	mu     sync.Mutex
	agents map[uuid.UUID]*Agent
}

func NewFleet(gatewayAddr, controlAddr string, tick time.Duration) *Fleet {
	return &Fleet{
		GatewayAddr: gatewayAddr,
		ControlAddr: controlAddr,
		Tick:        tick,
		agents:      map[uuid.UUID]*Agent{},
	}
}

// Run blocks until ctx ends: dial the gateway (h2c — trusted dev network, no
// TLS), keep the agent set in sync with the fleet, serve the control API.
func (f *Fleet) Run(ctx context.Context) error {
	conn, err := grpc.NewClient(f.GatewayAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("agentsim: dial gateway: %w", err)
	}
	defer conn.Close()
	f.client = agentpb.NewAgentGatewayClient(conn)

	f.syncAgents(ctx)
	go f.discoveryLoop(ctx)

	srv := &http.Server{Addr: f.ControlAddr, Handler: f.controlMux()}
	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
	}()
	slog.Info("agentsim -> control API serving", "addr", f.ControlAddr, "gateway", f.GatewayAddr)

	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		return fmt.Errorf("agentsim: control api: %w", err)
	}
	f.stopAll()
	return nil
}

func (f *Fleet) discoveryLoop(ctx context.Context) {
	t := time.NewTicker(hostDiscoveryInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			f.syncAgents(ctx)
		}
	}
}

// syncAgents spawns an agent for every schedulable host that lacks one. A
// failed listing is just logged: the engine may not be up yet, and the next
// discovery tick retries — same level-triggered posture as everything else.
func (f *Fleet) syncAgents(ctx context.Context) {
	resp, err := f.client.ListHosts(ctx, &agentpb.ListHostsRequest{})
	if err != nil {
		slog.Warn("agentsim: host discovery failed, retrying next tick", "err", err)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, h := range resp.Hosts {
		hostID, err := uuid.Parse(h.Id)
		if err != nil || f.agents[hostID] != nil {
			continue
		}
		a := NewAgent(f.client, hostID, h.Hostname, h.Region, f.Tick)
		a.Start(ctx)
		f.agents[hostID] = a
		slog.Info("agentsim -> agent spawned", "host", hostID, "hostname", h.Hostname, "region", h.Region)
	}
}

func (f *Fleet) stopAll() {
	f.mu.Lock()
	agents := make([]*Agent, 0, len(f.agents))
	for _, a := range f.agents {
		agents = append(agents, a)
	}
	f.mu.Unlock()
	for _, a := range agents {
		a.Stop()
	}
}

func (f *Fleet) agentByHost(hostID string) *Agent {
	id, err := uuid.Parse(hostID)
	if err != nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.agents[id]
}

// agentByReplica finds the agent currently running a replica, so chaos
// commands can target a replica without the caller knowing its host.
func (f *Fleet) agentByReplica(replicaID string) *Agent {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.agents {
		if slices.ContainsFunc(a.Status().Containers, func(c ContainerStatus) bool {
			return c.ReplicaID == replicaID
		}) {
			return a
		}
	}
	return nil
}

func (f *Fleet) statuses() []AgentStatus {
	f.mu.Lock()
	agents := make([]*Agent, 0, len(f.agents))
	for _, a := range f.agents {
		agents = append(agents, a)
	}
	f.mu.Unlock()
	out := make([]AgentStatus, len(agents))
	for i, a := range agents {
		out[i] = a.Status()
	}
	slices.SortFunc(out, func(x, y AgentStatus) int {
		if x.Hostname < y.Hostname {
			return -1
		}
		if x.Hostname > y.Hostname {
			return 1
		}
		return 0
	})
	return out
}
