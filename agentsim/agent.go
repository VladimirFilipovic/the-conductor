// Package agentsim is the simulated host-agent fleet: one Agent per host row,
// each holding a real gRPC Session to the apiserver's AgentAPI and playing a tiny
// per-host reconciler — desired state arrives on the downlink, fake containers
// and disks converge toward it, observations and heartbeats ride the uplink. Every agent
// is also a chaos point: its knobs make it lie or go silent so failure paths
// can be exercised through the REAL transport instead of SQL edits behind the
// engine's back.
package agentsim

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"conductor/proto/agentpb"

	"github.com/google/uuid"
)

const reconnectDelay = time.Second

// container is one fake workload. phase holds domain replica-phase strings
// ("starting", "health_check", "active", "failed") — the agent reports them
// verbatim.
type container struct {
	phase        string
	healthy      bool
	restartCount int32
	exitReason   string
	chaos        chaosMode
}

type chaosMode string

const (
	chaosNone        chaosMode = ""
	chaosCrashLoop   chaosMode = "crash_loop"
	chaosStallHealth chaosMode = "stall_health"
	chaosStallResize chaosMode = "stall_resize"
)

// disk is one fake volume on the host. sizeBytes is what's "on disk" and is
// what the agent reports; targetBytes is the size the last downlink asked
// for. A grow lands on the next tick (a real resize2fs takes a moment), a
// shrink is ignored — the control plane is grow-only and the agent must not
// destroy data because a message said so.
type disk struct {
	mountPath   string
	sizeBytes   int64
	targetBytes int64
	chaos       chaosMode
}

// Agent simulates one host's agent. Lifecycle: Start spawns the session loop,
// Stop tears it down. All chaos knobs are safe to call concurrently.
type Agent struct {
	HostID   uuid.UUID
	Hostname string
	Region   string

	client agentpb.AgentAPIClient
	tick   time.Duration

	mu         sync.Mutex
	hostDown   bool
	containers map[string]*container
	disks      map[string]*disk

	cancel context.CancelFunc
	done   chan struct{}
}

func NewAgent(client agentpb.AgentAPIClient, hostID uuid.UUID, hostname, region string, tick time.Duration) *Agent {
	return &Agent{
		HostID:     hostID,
		Hostname:   hostname,
		Region:     region,
		client:     client,
		tick:       tick,
		containers: map[string]*container{},
		disks:      map[string]*disk{},
	}
}

func (a *Agent) Start(ctx context.Context) {
	ctx, a.cancel = context.WithCancel(ctx)
	a.done = make(chan struct{})
	go func() {
		defer close(a.done)
		a.run(ctx)
	}()
}

func (a *Agent) Stop() {
	if a.cancel != nil {
		a.cancel()
		<-a.done
	}
}

// run is the reconnect loop: each attempt opens a fresh Session, and the
// AgentAPI's first downlink message is a full snapshot, so a dropped stream
// loses nothing.
func (a *Agent) run(ctx context.Context) {
	for ctx.Err() == nil {
		if err := a.session(ctx); err != nil && ctx.Err() == nil {
			slog.Debug("agentsim: session ended, reconnecting", "host", a.HostID, "err", err)
		}
		select {
		case <-time.After(reconnectDelay):
		case <-ctx.Done():
		}
	}
}

func (a *Agent) session(ctx context.Context) error {
	stream, err := a.client.Session(ctx)
	if err != nil {
		return err
	}
	if err := stream.Send(&agentpb.AgentMessage{Msg: &agentpb.AgentMessage_Hello{
		Hello: &agentpb.Hello{HostId: a.HostID.String()},
	}}); err != nil {
		return err
	}

	// Downlink drains on its own goroutine; errors surface through recvErr and
	// end the session so the outer loop reconnects.
	recvErr := make(chan error, 1)
	go func() {
		for {
			msg, err := stream.Recv()
			if err != nil {
				recvErr <- err
				return
			}
			if s := msg.GetState(); s != nil {
				a.applyState(s)
			}
		}
	}()

	t := time.NewTicker(a.tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-recvErr:
			return err
		case <-t.C:
			a.step()
			if err := a.report(stream); err != nil {
				return err
			}
		}
	}
}

// applyState converges the local container set toward the host's desired
// state: scheduling starts a container, draining stops it, absence removes it,
// and any other phase on an unknown replica is adopted as-is (the agent
// restarted mid-life and picks the workload back up).
func (a *Agent) applyState(state *agentpb.HostState) {
	a.mu.Lock()
	defer a.mu.Unlock()

	seen := map[string]bool{}
	for _, r := range state.Replicas {
		seen[r.Id] = true
		if c := a.containers[r.Id]; c != nil {
			if r.Phase == "draining" {
				// Graceful stop: the container vanishes; the reconciler reaps the
				// row on the drain window, no observation needed (the guard drops
				// reports about draining replicas anyway).
				delete(a.containers, r.Id)
			}
			continue
		}
		switch r.Phase {
		case "scheduling":
			a.containers[r.Id] = &container{phase: "starting"}
		case "draining", "failed", "reaped":
			// Nothing to run: retiring or terminal.
		default:
			a.containers[r.Id] = &container{phase: r.Phase, healthy: r.Phase == "active"}
		}
	}
	for id := range a.containers {
		if !seen[id] {
			delete(a.containers, id) // reaped or moved away
		}
	}

	// Disks: first sight creates the volume at the requested size (the
	// control plane hands out desired for a never-observed volume); a larger
	// size later is a grow the engine already approved; absence is deletion.
	seenDisks := map[string]bool{}
	for _, v := range state.Volumes {
		seenDisks[v.Id] = true
		d := a.disks[v.Id]
		if d == nil {
			a.disks[v.Id] = &disk{mountPath: v.MountPath, sizeBytes: v.SizeBytes, targetBytes: v.SizeBytes}
			continue
		}
		d.targetBytes = v.SizeBytes
	}
	for id := range a.disks {
		if !seenDisks[id] {
			delete(a.disks, id)
		}
	}
}

// step advances every container one tick: the happy path walks
// starting → health_check → active/healthy; chaos modes bend it. Disks grow
// to their target in one tick unless a stalled resize pins them.
func (a *Agent) step() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, d := range a.disks {
		if d.chaos != chaosStallResize && d.targetBytes > d.sizeBytes {
			d.sizeBytes = d.targetBytes
		}
	}
	for _, c := range a.containers {
		switch c.chaos {
		case chaosCrashLoop:
			// The container dies on every tick and the (simulated) restart policy
			// brings it straight back: restart_count climbs until the reconciler's
			// crashLooping rule trips.
			c.phase = "starting"
			c.healthy = false
			c.restartCount++
			c.exitReason = "chaos: crash loop"
		case chaosStallHealth:
			// Comes up but never passes probes — the progress-deadline path.
			if c.phase == "starting" || c.phase == "active" {
				c.phase = "health_check"
			}
			c.healthy = false
		default:
			switch c.phase {
			case "starting":
				c.phase = "health_check"
			case "health_check":
				c.phase = "active"
				c.healthy = true
			}
		}
	}
}

// report sends the tick's heartbeat plus one observation per container and
// one per disk. A downed host sends NOTHING — silence is exactly how a dead host looks to the
// sensor; the stream deliberately stays open (connection is not liveness).
func (a *Agent) report(stream agentpb.AgentAPI_SessionClient) error {
	a.mu.Lock()
	if a.hostDown {
		a.mu.Unlock()
		return nil
	}
	msgs := []*agentpb.AgentMessage{{Msg: &agentpb.AgentMessage_Heartbeat{
		Heartbeat: &agentpb.Heartbeat{},
	}}}
	for id, c := range a.containers {
		msgs = append(msgs, &agentpb.AgentMessage{Msg: &agentpb.AgentMessage_Observation{
			Observation: &agentpb.ReplicaObservation{
				ReplicaId:      id,
				Phase:          c.phase,
				Healthy:        c.healthy,
				RestartCount:   c.restartCount,
				LastExitReason: c.exitReason,
			},
		}})
	}
	for id, d := range a.disks {
		msgs = append(msgs, &agentpb.AgentMessage{Msg: &agentpb.AgentMessage_VolumeObservation{
			VolumeObservation: &agentpb.VolumeObservation{VolumeId: id, ObservedSizeBytes: d.sizeBytes},
		}})
	}
	a.mu.Unlock()

	for _, m := range msgs {
		if err := stream.Send(m); err != nil {
			return err
		}
	}
	return nil
}

// --- chaos knobs -------------------------------------------------------------

// KillHost silences the agent: no heartbeats, no observations. The gRPC stream
// stays open on purpose — proving liveness comes from heartbeats, not from a
// connection existing.
func (a *Agent) KillHost() {
	a.mu.Lock()
	a.hostDown = true
	a.mu.Unlock()
}

// RecoverHost resumes reporting. The next heartbeat marks the host healthy
// again and the placer starts considering it.
func (a *Agent) RecoverHost() {
	a.mu.Lock()
	a.hostDown = false
	a.mu.Unlock()
}

// CrashReplica kills one container terminally: it reports failed once (the
// observation guard then owns the row) and stays down until the reconciler
// destroys it.
func (a *Agent) CrashReplica(replicaID string) bool {
	return a.withContainer(replicaID, func(c *container) {
		c.phase = "failed"
		c.healthy = false
		c.exitReason = "chaos: killed"
		c.chaos = chaosNone
	})
}

// CrashLoop puts one container into die-restart-die: restart_count climbs
// every tick until crashLooping fails the deployment (or Heal is called).
func (a *Agent) CrashLoop(replicaID string) bool {
	return a.withContainer(replicaID, func(c *container) { c.chaos = chaosCrashLoop })
}

// StallHealth pins one container short of healthy — the stalled-rollout /
// progress-deadline path.
func (a *Agent) StallHealth(replicaID string) bool {
	return a.withContainer(replicaID, func(c *container) { c.chaos = chaosStallHealth })
}

// Heal clears any chaos mode; the container resumes its normal walk to
// active/healthy.
func (a *Agent) Heal(replicaID string) bool {
	return a.withContainer(replicaID, func(c *container) { c.chaos = chaosNone })
}

// StallResize pins one disk at its current size: the engine approves the grow
// and flips the volume to resizing, the agent acknowledges nothing — the
// resize that never finishes, as seen from the control plane.
func (a *Agent) StallResize(volumeID string) bool {
	return a.withDisk(volumeID, func(d *disk) { d.chaos = chaosStallResize })
}

// HealVolume clears a stalled resize; the disk catches up on the next tick.
func (a *Agent) HealVolume(volumeID string) bool {
	return a.withDisk(volumeID, func(d *disk) { d.chaos = chaosNone })
}

func (a *Agent) withDisk(volumeID string, fn func(*disk)) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	d := a.disks[volumeID]
	if d == nil {
		return false
	}
	fn(d)
	return true
}

func (a *Agent) withContainer(replicaID string, fn func(*container)) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	c := a.containers[replicaID]
	if c == nil {
		return false
	}
	fn(c)
	return true
}

// --- introspection ------------------------------------------------------------

type ContainerStatus struct {
	ReplicaID    string `json:"replica_id"`
	Phase        string `json:"phase"`
	Healthy      bool   `json:"healthy"`
	RestartCount int32  `json:"restart_count"`
	Chaos        string `json:"chaos,omitempty"`
}

type VolumeStatus struct {
	VolumeID    string `json:"volume_id"`
	MountPath   string `json:"mount_path"`
	SizeBytes   int64  `json:"size_bytes"`
	TargetBytes int64  `json:"target_bytes"`
	Chaos       string `json:"chaos,omitempty"`
}

type AgentStatus struct {
	HostID     string            `json:"host_id"`
	Hostname   string            `json:"hostname"`
	Region     string            `json:"region"`
	HostDown   bool              `json:"host_down"`
	Containers []ContainerStatus `json:"containers"`
	Volumes    []VolumeStatus    `json:"volumes"`
}

func (a *Agent) Status() AgentStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := AgentStatus{
		HostID:   a.HostID.String(),
		Hostname: a.Hostname,
		Region:   a.Region,
		HostDown: a.hostDown,
		// Non-nil so an idle host serializes as [] rather than null.
		Containers: []ContainerStatus{},
		Volumes:    []VolumeStatus{},
	}
	for id, c := range a.containers {
		st.Containers = append(st.Containers, ContainerStatus{
			ReplicaID:    id,
			Phase:        c.phase,
			Healthy:      c.healthy,
			RestartCount: c.restartCount,
			Chaos:        string(c.chaos),
		})
	}
	for id, d := range a.disks {
		st.Volumes = append(st.Volumes, VolumeStatus{
			VolumeID:    id,
			MountPath:   d.mountPath,
			SizeBytes:   d.sizeBytes,
			TargetBytes: d.targetBytes,
			Chaos:       string(d.chaos),
		})
	}
	return st
}
