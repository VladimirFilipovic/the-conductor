Engine takes users desired state for the service and continuosly runs trying to fulfill those demands.

Engines requirements:

- Decide where each workload runs across a heterogeneous multi-region bare-metal fleet (bin-packing under
  CPU/RAM/volume/region constraints).
- Drive each deploy through a lifecycle: schedule → start → health-gate → shift traffic → drain old → reclaim.
- Self-heal: detect dead replicas/hosts and reconcile back to desired state.
- Treat stateless and stateful workloads with fundamentally different placement & rollout rules.
- Be declarative and level-triggered — survive control-plane restarts, never lose track of reality.
