// Package telemetry decouples kernel-space latency measurement from the
// user-space estimation pipeline: kprobe events arriving through the
// BPF_MAP_TYPE_RINGBUF are pushed into a bounded lock-free single-producer
// single-consumer ring and drained by the KDE/gossip pipeline.
package telemetry

import (
	"sync/atomic"
)

// Event is one kernel-side round-trip measurement (Equation
// kernel_latency_calc): end-to-end latency of a request routed to an
// instance, in microseconds.
type Event struct {
	Instance   uint32
	LatencyUs  uint32
	StatusCode uint16
	_          uint16
}

// Ring is a bounded SPSC ring buffer of capacity N_max. Head and tail are
// plain atomics: exactly one producer (the eBPF reader goroutine) and one
// consumer (the estimator goroutine) touch them, so no mutex is required.
type Ring struct {
	buf  []Event
	mask uint64
	head atomic.Uint64 // written by producer
	tail atomic.Uint64 // written by consumer
	drop atomic.Uint64
}

// NewRing allocates a ring holding capacity events (rounded to a power of two).
func NewRing(capacity int) *Ring {
	if capacity < 2 {
		capacity = 2
	}
	n := 1
	for n < capacity {
		n <<= 1
	}
	return &Ring{buf: make([]Event, n), mask: uint64(n - 1)}
}

// Offer appends an event; it returns false when the ring is full (the event
// is dropped and counted, preserving real-time behaviour over completeness).
func (r *Ring) Offer(ev Event) bool {
	head := r.head.Load()
	tail := r.tail.Load()
	if head-tail >= uint64(len(r.buf)) {
		r.drop.Add(1)
		return false
	}
	r.buf[head&r.mask] = ev
	// Publish the slot before advancing the cursor.
	r.head.Store(head + 1)
	return true
}

// Take removes the oldest event; ok is false when the ring is empty.
func (r *Ring) Take() (ev Event, ok bool) {
	tail := r.tail.Load()
	head := r.head.Load()
	if tail >= head {
		return Event{}, false
	}
	ev = r.buf[tail&r.mask]
	r.tail.Store(tail + 1)
	return ev, true
}

// Dropped returns the number of events lost to a full ring.
func (r *Ring) Dropped() uint64 { return r.drop.Load() }

// Len returns the number of pending events.
func (r *Ring) Len() int {
	return int(r.head.Load() - r.tail.Load())
}
