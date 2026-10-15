// Package proxy implements the user-space L7 forwarding engine of the data
// plane (Section "Data Plane: Dual-Layer eBPF Acceleration and L7 Request
// Proxying"): HTTP/1.1 request delineation in user space with per-request
// Smooth Weighted Round Robin dispatch over persistent backend connection
// pools.
package proxy

import (
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/ahmadpanah/prigossip-hazard/internal/agent"
	"github.com/ahmadpanah/prigossip-hazard/internal/config"
	"github.com/ahmadpanah/prigossip-hazard/internal/discovery"
)

// Handler forwards requests to the instance selected by the dispatch engine
// and feeds the measured round-trip latency back into the KDE estimator.
type Handler struct {
	spec     config.Spec
	agent    *agent.Agent
	registry *discovery.Registry

	mu    sync.Mutex
	pool  map[string]*http.Transport
	total uint64
}

// New builds a forwarding handler.
func New(spec config.Spec, a *agent.Agent, reg *discovery.Registry) *Handler {
	return &Handler{
		spec:     spec,
		agent:    a,
		registry: reg,
		pool:     make(map[string]*http.Transport),
	}
}

// transport returns (and lazily creates) the persistent connection pool for
// one backend, i.e. the server-facing TCP state machine of the paper.
func (h *Handler) transport(key string, addr string) *http.Transport {
	h.mu.Lock()
	defer h.mu.Unlock()
	if t, ok := h.pool[key]; ok {
		return t
	}
	t := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   2 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: time.Duration(h.spec.SLA.LatencyThresholdMs*4) * time.Millisecond,
		ForceAttemptHTTP2:     false,
	}
	h.pool[key] = t
	return t
}

// ServeHTTP delineates one request, samples the target instance with SWRR,
// forwards the payload and records the end-to-end latency (Equation
// kernel_latency_calc is the kernel-side equivalent of this measurement).
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	backends := h.registry.Backends()
	if len(backends) == 0 {
		http.Error(w, "no backend endpoints", http.StatusServiceUnavailable)
		return
	}
	inst := h.agent.Route()
	if inst < 0 {
		http.Error(w, "no dispatchable instance", http.StatusServiceUnavailable)
		return
	}
	b := backends[inst%len(backends)]

	out := r.Clone(r.Context())
	out.RequestURI = ""
	out.URL.Scheme = "http"
	out.URL.Host = net.JoinHostPort(b.IP, strconv.Itoa(b.Port))
	out.Header.Set("X-PriGossip-Instance", strconv.Itoa(inst))
	out.Header.Set("X-PriGossip-Proxy", h.agent.NodeName())

	start := time.Now()
	resp, err := h.transport(b.Name+"/"+b.IP, out.URL.Host).RoundTrip(out)
	if err != nil {
		// A transport failure is an SLA violation: count it as a request that
		// exceeded the deadline so the error counter can react.
		elapsed := float64(h.spec.SLA.LatencyThresholdMs) * 2
		if !h.agent.KernelTelemetry() {
			h.agent.Observe(inst, elapsed)
		}
		log.Printf("proxy: instance %d (%s) error: %v", inst, out.URL.Host, err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	// Expose the selected instance so load generators can compute Jain's
	// Fairness Index over per-instance request counts.
	w.Header().Set("X-PriGossip-Instance", strconv.Itoa(inst))
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)

	elapsed := float64(time.Since(start).Microseconds()) / 1000.0
	// When the eBPF kprobes are attached they own the measurement (kernel
	// fidelity, Equation kernel_latency_calc); the proxy only accounts for
	// them in that mode.
	if !h.agent.KernelTelemetry() {
		h.agent.Observe(inst, elapsed)
	}
}
