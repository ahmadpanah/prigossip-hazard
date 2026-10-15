/* SPDX-License-Identifier: Apache-2.0
 *
 * Shared definitions for the PriGossip-Hazard eBPF data plane
 * (Section "Data Plane: Dual-Layer eBPF Acceleration and L7 Request
 * Proxying").
 */
#ifndef PRIGOSPISTIC_BPF_COMMON_H
#define PRIGOSPISTIC_BPF_COMMON_H

#define __BPF__ 1

/* Wire-format event pushed to user space through BPF_MAP_TYPE_RINGBUF. */
struct latency_event {
	__u64 latency_ns;  /* T_ack - T_start, Equation (kernel_latency_calc) */
	__u32 instance;    /* backend instance index m */
	__u32 bytes;       /* bytes drained from the receive buffer */
	__u64 start_ns;    /* T_start, monotonic nanoseconds */
	__u64 ack_ns;      /* T_ack, monotonic nanoseconds */
};

/* Per-socket measurement state: when the request was committed and which
 * instance the socket is bound to.
 */
struct sock_stamp {
	__u64 start_ns;
	__u32 instance;
	__u32 pad;
};

/* Well-known port of the managed edge-AI service (overridable at load time). */
#define PRIGOSPISTIC_SERVICE_PORT 8000
#define PRIGOSPISTIC_AGENT_PORT   8080

#endif /* PRIGOSPISTIC_BPF_COMMON_H */
