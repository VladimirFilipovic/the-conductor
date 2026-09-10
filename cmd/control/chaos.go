package control

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"text/tabwriter"
	"time"
)

const chaosUsage = `conductor chaos — drive the simulated agent fleet (agentsim control API)

Usage:
  conductor chaos agents                     List agents and their containers
  conductor chaos kill-host <host-id>        Host goes silent (heartbeats stop)
  conductor chaos recover-host <host-id>     Host resumes heartbeating
  conductor chaos crash <replica-id>         Container dies terminally (failed)
  conductor chaos crashloop <replica-id>     Container dies+restarts every tick
  conductor chaos stall <replica-id>         Container never passes health checks
  conductor chaos heal <replica-id>          Clear chaos, resume normal life

Flags:
  --addr   agentsim control API (default localhost:7780)
`

// cmdChaos talks to the agentsim fleet's HTTP control API — chaos flows
// through the agents (they lie or go silent over the real gRPC transport),
// never through direct database edits.
func cmdChaos(args []string) error {
	fs := flag.NewFlagSet("chaos", flag.ContinueOnError)
	addr := fs.String("addr", "localhost:7780", "agentsim control API address")
	fs.Usage = func() { fmt.Fprint(os.Stderr, chaosUsage) }
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fmt.Print(chaosUsage)
		return nil
	}

	client := &http.Client{Timeout: 5 * time.Second}
	base := "http://" + *addr

	sub, id := rest[0], ""
	if len(rest) > 1 {
		id = rest[1]
	}

	if sub == "agents" {
		return chaosListAgents(client, base)
	}

	actions := map[string]struct{ action, target string }{
		"kill-host":    {"host_kill", "host"},
		"recover-host": {"host_recover", "host"},
		"crash":        {"replica_crash", "replica"},
		"crashloop":    {"replica_crashloop", "replica"},
		"stall":        {"replica_stall_health", "replica"},
		"heal":         {"replica_heal", "replica"},
	}
	act, ok := actions[sub]
	if !ok {
		return fmt.Errorf("chaos: unknown subcommand %q\n\n%s", sub, chaosUsage)
	}
	if id == "" {
		return fmt.Errorf("chaos %s: missing id", sub)
	}

	body := map[string]string{"action": act.action, act.target: id}
	payload, _ := json.Marshal(body)
	resp, err := client.Post(base+"/chaos", "application/json", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("chaos: %w (is `conductor agentsim` running?)", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return fmt.Errorf("chaos %s: %s", sub, e.Error)
	}
	fmt.Printf("chaos %s %s: ok\n", sub, id)
	return nil
}

func chaosListAgents(client *http.Client, base string) error {
	resp, err := client.Get(base + "/agents")
	if err != nil {
		return fmt.Errorf("chaos: %w (is `conductor agentsim` running?)", err)
	}
	defer resp.Body.Close()

	var agents []struct {
		HostID     string `json:"host_id"`
		Hostname   string `json:"hostname"`
		Region     string `json:"region"`
		HostDown   bool   `json:"host_down"`
		Containers []struct {
			ReplicaID    string `json:"replica_id"`
			Phase        string `json:"phase"`
			Healthy      bool   `json:"healthy"`
			RestartCount int32  `json:"restart_count"`
			Chaos        string `json:"chaos"`
		} `json:"containers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&agents); err != nil {
		return fmt.Errorf("chaos: decode agents: %w", err)
	}

	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(w, "HOST\tHOSTNAME\tREGION\tSTATE\tREPLICA\tPHASE\tHEALTHY\tRESTARTS\tCHAOS")
	for _, a := range agents {
		state := "up"
		if a.HostDown {
			state = "DOWN"
		}
		if len(a.Containers) == 0 {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t-\t\t\t\t\n", a.HostID, a.Hostname, a.Region, state)
			continue
		}
		for i, c := range a.Containers {
			host, name, region, st := a.HostID, a.Hostname, a.Region, state
			if i > 0 {
				host, name, region, st = "", "", "", ""
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%v\t%d\t%s\n",
				host, name, region, st, c.ReplicaID, c.Phase, c.Healthy, c.RestartCount, c.Chaos)
		}
	}
	return w.Flush()
}
