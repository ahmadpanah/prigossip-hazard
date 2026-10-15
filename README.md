# PriGossip-Hazard

Reference implementation of **PriGossip-Hazard: Decentralized QoS-Aware
Routing** — a privacy-preserving, oscillation-resilient decentralized
load-balancing framework for the Computing Continuum, accompanying the paper
*"Desynchronized Multi-Player Bandits for Privacy-Preserving and QoS-Aware
Edge Load Balancing in IoT Continuums"* (Ahmadpanah & Montazerolghaem,
University of Isfahan).

The artifact is self-contained: the Kubernetes `DaemonSet` source, the eBPF
data plane, the Helm charts, the workload generator and the WAN latency traces
are all included. Everything runs without cloud credentials; the full pipeline
can be exercised on a laptop, and the full evaluation on a K3s cluster.

## What is in the box

| Path | Paper counterpart |
|------|-------------------|
| `cmd/agent`, `internal/agent` | The Kubernetes-native DaemonSet: wires telemetry ingestion, KDE QoS estimation, LDP gossip, survival-theoretic quarantine and SWRR dispatch into one control loop. |
| `internal/qos` | Sliding-window QoS estimator (Equation windows, per-client SLA compliance). |
| `internal/gossip` | Asynchronous anti-entropy gossip: ε-LDP second-order latency volatility sketches, count-min aggregation, consensus scores (Section *Privacy-Preserving Gossip*). |
| `internal/hazard` | Weibull queue-depletion survival model, online Cox Proportional Hazards refit (Breslow baseline, Newton–Raphson updates) and the continuous re-admission scheduler that desynchronizes proxies (Section *Survival-Theoretic Re-Admission*). |
| `internal/dispatch` | Smoothed Weighted Round-Robin dispatcher with volatility regularization and decaying exploration. |
| `internal/proxy`, `internal/telemetry` | L7 forwarding engine and the ring buffer feeding the kernel/user-space latency fusion. |
| `internal/discovery`, `internal/mqttx` | Kubernetes EndpointSlice informer and MQTT-based failure announcements. |
| `bpf/` | eBPF C programs: `sock_ops` ingress redirection, `sk_msg` zero-copy fast path, `kprobe` sub-µs RTT timestamping (see [bpf/README.md](bpf/README.md)). |
| `deploy/helm/prigossip-hazard/` | Helm chart: DaemonSet, RBAC, Mosquitto broker, `QoSPolicy` CRD and example CR. |
| `cmd/backend` | Edge-AI backend emulator: PilotNet-style INT8 inference service time (21.5 ± 1.8 ms), bounded queue with shedding. |
| `cmd/workloadgen` | Vehicular edge-AI workload generator: N clients with a Poisson request process, SLA τ/ρ satisfaction, Jain fairness, surge injection, JSON/CSV output. |
| `cmd/tracegen`, `traces/` | Reproducible WAN latency traces for the five European topologies (T1–T5). |
| `examples/policy-local.json` | `QoSPolicy` manifest used by the local quick start (no Kubernetes needed). |

## Requirements

- **Go ≥ 1.22** (module declares `go 1.22.0`).
- **Linux ≥ 5.8 with clang/LLVM + libbpf** only for the accelerated data
  plane. On hosts without eBPF (macOS/Windows dev machines) the agent runs in
  *user-space-only* mode: dispatch, gossip, hazard estimation and the L7 proxy
  keep working; only kernel interception is disabled.
- **Kubernetes/K3s + Helm 3** only for the cluster deployment.

## Quick start (single host, no Kubernetes)

```sh
make build          # -> bin/agent, bin/backend, bin/workloadgen, bin/tracegen

# four emulated edge-AI instances (PilotNet service time, bounded queue)
for id in 0 1 2 3; do ./bin/backend -id $id -addr 127.0.0.1:800$id & done

# one PriGossip-Hazard proxy in user-space-only mode
./bin/agent \
  -policy examples/policy-local.json \
  -backends 127.0.0.1:8000,127.0.0.1:8001,127.0.0.1:8002,127.0.0.1:8003 \
  -listen 127.0.0.1:18080 &

# 16 vehicular clients, 8 rps each, SLA tau=80 ms over a 10 s window
./bin/workloadgen -targets http://127.0.0.1:18080 \
  -clients 16 -surge-clients 0 -rps 8 -duration 15s -tau 80 -window 10s
```

A verified reference run (Go 1.22, 15 s, seed 42) reports `errors: 0`,
`instancesServed: 4` with an even split (≈365 requests each), `p90 ≈ 35 ms`
against the 80 ms SLA and Jain fairness ≈ 1.0. Add `-out summary.json` and
`-csv compliance.csv` to persist per-client compliance curves for plotting.

To exercise quarantine and hazard-based re-admission, kill one backend
mid-run; the agent detects consecutive failures at the `errorThreshold`,
quarantines the instance, and re-admits it on a *staggered* Weibull/Cox
schedule rather than a static cooldown.

## Tests

```sh
make test    # unit tests: hazard (Weibull/Cox), gossip sketches, dispatch, QoS windows, agent loop
make vet     # go vet ./...
```

The tests cover the paper's core claims at unit level: monotone hazard
behaviour for κ ≷ 1, clamped Cox covariates, partial-likelihood/gradient
consistency, sketch error bounds under ε-LDP noise, quarantine triggering and
desynchronized re-admission.

## Cluster deployment (K3s / Kubernetes)

```sh
# 1. build + install the eBPF objects on every node (or bake into a DaemonSet image)
make bpf && sudo make -C bpf install     # -> /var/lib/prigossip-hazard/bpf/*.o

# 2. install the chart (agent DaemonSet + Mosquitto + QoSPolicy CRD)
helm upgrade --install prigossip-hazard deploy/helm/prigossip-hazard \
  --namespace prigossip-hazard --create-namespace \
  --set image.repository=prigossip-hazard/agent

# 3. declare the managed workload's SLO
kubectl apply -f - <<'EOF'
apiVersion: qos.prigossip.io/v1alpha1
kind: QoSPolicy
metadata: {name: vision-ai}
spec:
  service: vision-ai
  latencyThresholdMs: 80
  successRatio: 0.90
EOF
```

Peer discovery uses the Kubernetes EndpointSlice informer for cluster peers
plus the MQTT broker for failure announcements; the gossip transport is
plain UDP (see `gossipPort` in [values.yaml](deploy/helm/prigossip-hazard/values.yaml)).

## Latency traces (T1–T5)

`traces/` holds the directed-pair WAN latency measurements for the five
European topologies of the evaluation:

```
topology,source,destination,avg_rtt_ms,stddev_ms,samples
t1,Frankfurt,Munich,13.94,0.61,30
```

The topology compositions and RTT envelopes are those reported in the paper,
derived from the WonderNetwork global ping repository (30 pings per pair,
daily average ± spread). WonderNetwork's raw dumps are not redistributable,
so the artifact ships deterministic per-pair tables generated from those
envelopes; regenerating them is byte-for-byte reproducible:

```sh
make traces    # runs bin/tracegen -out traces (FNV-hashed latent coordinates, no RNG)
```

To evaluate against real measurements instead, drop a raw WonderNetwork
export into `traces/` using the same six columns — the evaluation harness
reads whichever file `traces/index.csv` lists.


## License

Apache-2.0 — see [LICENSE](LICENSE).
