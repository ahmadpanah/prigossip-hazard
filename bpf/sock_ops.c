// SPDX-License-Identifier: Apache-2.0
//
// Ingress socket interception: a cgroup sock_ops program attached to the
// root cgroup hierarchy. On completion of the TCP three-way handshake
// (BPF_SOCK_OPS_ACTIVE_ESTABLISHED_CB) the program interrogates the
// destination port and, when the flow targets a registered edge-AI service,
// commits the socket to an in-kernel sockhash so that the user-space
// forwarding engine receives the connection directly, bypassing the
// standard host iptables/netfilter evaluation path.

#include <linux/bpf.h>
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_endian.h>

#include "common.h"

char LICENSE[] SEC("license") = "GPL";

/* Registered service ports (populated by the agent before attachment). */
struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 64);
	__type(key, __u32);   /* port, host byte order */
	__type(value, __u32); /* instance index, unused by the redirect path */
} service_ports SEC(".maps");

/* Sockets steered toward the user-space engine / sk_msg fast path. */
struct {
	__uint(type, BPF_MAP_TYPE_SOCKHASH);
	__uint(max_entries, 1024);
	__uint(key_size, 4);
} sock_map SEC(".maps");

/* Observability counters exposed to the agent. */
struct {
	__uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
	__uint(max_entries, 4);
	__type(key, __u32);
	__type(value, __u64);
} intercept_stats SEC(".maps");

enum {
	STAT_HANDSHAKES = 0,
	STAT_MATCHED = 1,
	STAT_INSERTED = 2,
	STAT_ERRORS = 3,
};

static __always_inline void bump(__u32 slot)
{
	__u64 *v = bpf_map_lookup_elem(&intercept_stats, &slot);
	if (v)
		*v += 1;
}

SEC("sockops")
int prigossip_sock_ops(struct bpf_sock_ops *ctx)
{
	__u32 op = ctx->op;
	__u32 slot;

	if (op != BPF_SOCK_OPS_ACTIVE_ESTABLISHED_CB &&
	    op != BPF_SOCK_OPS_PASSIVE_ESTABLISHED_CB)
		return 0;

	bump(STAT_HANDSHAKES);

	/* remote_port is in network byte order for established callbacks;
	 * local_port is host order. Active connections address the service
	 * through remote_port, accepted connections through local_port.
	 */
	__u32 port = (op == BPF_SOCK_OPS_ACTIVE_ESTABLISHED_CB)
			     ? bpf_ntohs(ctx->remote_port)
			     : bpf_ntohs(ctx->local_port);

	__u32 *inst = bpf_map_lookup_elem(&service_ports, &port);
	if (!inst)
		return 0;

	bump(STAT_MATCHED);

	__u32 key = ctx->remote_ip4;
	if (!key)
		key = ctx->local_ip4;
	if (!key)
		key = ctx->sk;

	long ret = bpf_sock_hash_update(ctx, &sock_map, &key, BPF_NOEXIST);
	if (ret) {
		/* Retry without the existence guard: the socket may already
		 * have been inserted by an earlier callback.
		 */
		ret = bpf_sock_hash_update(ctx, &sock_map, &key, 0);
		if (ret) {
			bump(STAT_ERRORS);
			return 1;
		}
	}
	bump(STAT_INSERTED);

	/* Tell the agent that ESTABLISHED sockets may be forwarded without
	 * further netfilter traversal.
	 */
	ctx->reply = 1;
	return 1;
}
