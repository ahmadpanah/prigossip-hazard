# eBPF data plane

Three programs implement the dual-layer forwarding pipeline described in the
paper (Section *Data Plane: Dual-Layer eBPF Acceleration and L7 Request
Proxying*):

| Object                | Hook / type            | Responsibility |
|-----------------------|------------------------|----------------|
| `sock_ops.o`          | `BPF_PROG_TYPE_SOCK_OPS` on the root cgroup | Ingress socket redirection on `BPF_SOCK_OPS_ACTIVE_ESTABLISHED_CB`; commits matched flows to `bpf_sockmap`/`sockhash`, bypassing iptables/netfilter. |
| `sk_msg.o`            | `BPF_PROG_TYPE_SK_MSG`  | Zero-copy fast path via `bpf_msg_redirect_hash` for co-located backends (28–35 µs saving per local round trip). |
| `kprobe_latency.c`    | kprobes on `tcp_sendmsg` and `tcp_cleanup_rbuf` | Sub-µs round-trip timestamping (`bpf_ktime_get_ns`) exported through `BPF_MAP_TYPE_RINGBUF`. |

## Building

```sh
# needs clang, libbpf headers, kernel headers (or a generated vmlinux.h)
cd bpf
make
sudo make install          # -> /var/lib/prigossip-hazard/bpf/*.o
```

The agent loads the objects from `BPF_OBJ_DIR` (default
`/var/lib/prigossip-hazard/bpf`). When the directory is absent — for example
on macOS/Windows development hosts, or in clusters without the `bpf` LSM /
Cilium socket-level load balancing enabled — the agent logs a notice and runs
in *user-space only* mode: dispatch, gossip, hazard estimation and the L7
proxy keep working, only kernel-side interception and sub-µs timestamping are
disabled.

## Verifying

```sh
sudo bpftool prog show name prigossip_sock_ops
sudo bpftool map show name latency_ring
cat /sys/fs/bpf/prigossip/stats      # optional pinned counters
```

Latency events are consumed by the agent's telemetry pipeline and pushed into
the sliding windows that drive the KDE estimator, the $\epsilon$-LDP variance
sketches and the Cox covariate $\mathbf{z}_{k,m}$.
