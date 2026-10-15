// SPDX-License-Identifier: Apache-2.0
//
// Kernel-level telemetry acquisition: BPF kprobes attached to
// tcp_sendmsg and tcp_cleanup_rbuf across both client and backend socket
// boundaries. The ingress probe records a 64-bit monotonic nanosecond
// timestamp as the request bytes enter the socket buffer; the egress probe
// captures the timestamp at which the response payload is read and evicted
// from the receive buffer. The difference is the round-trip response
// duration L_{k,m}(t), exported through BPF_MAP_TYPE_RINGBUF to the
// user-space KDE pipeline.

#include <linux/bpf.h>
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>

#include "common.h"

char LICENSE[] SEC("license") = "GPL";

/* Socket cookie -> {T_start, instance} written by the agent when it binds a
 * connection to a backend, and by tcp_sendmsg for measured flows.
 */
struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 8192);
	__type(key, __u64); /* bpf_get_socket_cookie */
	__type(value, struct sock_stamp);
} stamps SEC(".maps");

/* Instance index of each measured socket (set from user space). */
struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 8192);
	__type(key, __u64);
	__type(value, __u32);
} sock_instance SEC(".maps");

/* Lock-free event stream to user space. */
struct {
	__uint(type, BPF_MAP_TYPE_RINGBUF);
	__uint(max_entries, 1 << 18); /* 256 KiB */
} latency_ring SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
	__uint(max_entries, 3);
	__type(key, __u32);
	__type(value, __u64);
} probe_stats SEC(".maps");

enum { PROBE_STARTS = 0, PROBE_ACKS = 1, PROBE_DROPS = 2 };

static __always_inline void bump(__u32 slot)
{
	__u64 *v = bpf_map_lookup_elem(&probe_stats, &slot);
	if (v)
		*v += 1;
}

/* T_start: the final byte of the request has entered the socket buffer. */
SEC("kprobe/tcp_sendmsg")
int BPF_KPROBE(prigossip_tcp_sendmsg, struct sock *sk, struct msghdr *msg,
		size_t size)
{
	if (!sk || size == 0)
		return 0;

	__u64 cookie = bpf_get_socket_cookie((struct sock *)sk);
	__u32 inst = 0;
	__u32 *ip = bpf_map_lookup_elem(&sock_instance, &cookie);
	if (ip)
		inst = *ip;

	struct sock_stamp st = {};
	st.start_ns = bpf_ktime_get_ns();
	st.instance = inst;
	bpf_map_update_elem(&stamps, &cookie, &st, BPF_ANY);
	bump(PROBE_STARTS);
	return 0;
}

/* T_ack: the response payload is read out of the receive buffer. */
SEC("kprobe/tcp_cleanup_rbuf")
int BPF_KPROBE(prigossip_tcp_cleanup_rbuf, struct sock *sk, int copied)
{
	if (!sk || copied <= 0)
		return 0;

	__u64 cookie = bpf_get_socket_cookie((struct sock *)sk);
	struct sock_stamp *st = bpf_map_lookup_elem(&stamps, &cookie);
	if (!st)
		return 0;

	__u64 ack = bpf_ktime_get_ns();
	__u64 start = st->start_ns;
	__u32 inst = st->instance;
	bpf_map_delete_elem(&stamps, &cookie);
	if (ack <= start)
		return 0;

	struct latency_event *ev =
		bpf_ringbuf_reserve(&latency_ring, sizeof(*ev), 0);
	if (!ev) {
		bump(PROBE_DROPS);
		return 0;
	}
	ev->latency_ns = ack - start;
	ev->start_ns = start;
	ev->ack_ns = ack;
	ev->instance = inst;
	ev->bytes = (__u32)copied;
	bpf_ringbuf_submit(ev, 0);
	bump(PROBE_ACKS);
	return 0;
}
