// Command backend emulates one quantized PilotNet inference replica of the
// testbed: an INT8 forward pass costs 21.5 +/- 1.8 ms on the benchmarked
// edge cores, requests are served by a small FIFO worker pool, and overload
// manifests as queue growth and eventual deadline shedding.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

type server struct {
	id        int
	workers   int
	service   time.Duration // mean inference time
	jitter    time.Duration
	maxQueue  time.Duration
	lines     []time.Time // per-worker "free at" timeline
	mu        sync.Mutex
	served    atomic.Uint64
	rejected  atomic.Uint64
	inflight  atomic.Int64
	queueWait atomic.Int64 // accumulated queue wait, microseconds
	rng       *rand.Rand
}

func main() {
	var (
		id       = flag.Int("id", 0, "instance identifier echoed to the client")
		addr     = flag.String("addr", ":8000", "listen address")
		workers  = flag.Int("workers", 4, "concurrent inference workers")
		base     = flag.Duration("service", 21*time.Millisecond+500*time.Microsecond, "mean inference time (PilotNet INT8: 21.5 +/- 1.8 ms)")
		jitter   = flag.Duration("jitter", 1800*time.Microsecond, "uniform service-time jitter")
		maxQueue = flag.Duration("max-queue", 120*time.Millisecond, "maximum tolerated queue wait before shedding")
	)
	flag.Parse()

	s := &server{
		id:       *id,
		workers:  *workers,
		service:  *base,
		jitter:   *jitter,
		maxQueue: *maxQueue,
		lines:    make([]time.Time, *workers),
		rng:      rand.New(rand.NewSource(time.Now().UnixNano() + int64(*id))),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/stats", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"instance":   s.id,
			"served":     s.served.Load(),
			"rejected":   s.rejected.Load(),
			"inflight":   s.inflight.Load(),
			"avgQueueUs": s.queueWait.Load() / int64(max1(s.served.Load())),
		})
	})
	mux.HandleFunc("/infer", s.handleInfer)
	mux.HandleFunc("/", s.handleInfer)

	log.Printf("backend[%d]: listening on %s (workers=%d service=%s jitter=%s)",
		s.id, *addr, s.workers, s.service, s.jitter)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func max1(v uint64) uint64 {
	if v == 0 {
		return 1
	}
	return v
}

// handleInfer schedules the request on the least-busy worker line, sleeps the
// simulated inference time and reports the realized service latency.
func (s *server) handleInfer(w http.ResponseWriter, r *http.Request) {
	arrival := time.Now()
	s.inflight.Add(1)
	defer s.inflight.Add(-1)

	// Select the worker that becomes free first (a work-conserving pool).
	s.mu.Lock()
	best := 0
	for i := 1; i < len(s.lines); i++ {
		if s.lines[i].Before(s.lines[best]) {
			best = i
		}
	}
	start := arrival
	if s.lines[best].After(start) {
		start = s.lines[best]
	}
	wait := start.Sub(arrival)
	if wait > s.maxQueue {
		// Queue is saturated: shed the request (deadline violation).
		s.mu.Unlock()
		s.rejected.Add(1)
		w.Header().Set("X-PriGossip-Instance", fmt.Sprint(s.id))
		http.Error(w, "queue saturated", http.StatusServiceUnavailable)
		return
	}
	svc := s.service + time.Duration(s.rng.Int63n(int64(2*s.jitter)+1)) - s.jitter
	if svc < time.Millisecond {
		svc = time.Millisecond
	}
	s.lines[best] = start.Add(svc)
	s.mu.Unlock()

	s.queueWait.Add(wait.Microseconds())
	time.Sleep(time.Until(start)) // drain the simulated queue
	time.Sleep(svc)               // run the inference

	s.served.Add(1)
	w.Header().Set("X-PriGossip-Instance", fmt.Sprint(s.id))
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Server-Timing", fmt.Sprintf("queue;dur=%.2f, inference;dur=%.2f",
		float64(wait.Microseconds())/1000.0, float64(svc.Microseconds())/1000.0))
	_ = json.NewEncoder(w).Encode(map[string]any{
		"instance":  s.id,
		"queueMs":   float64(wait.Microseconds()) / 1000.0,
		"serviceMs": float64(svc.Microseconds()) / 1000.0,
		"steering":  "pilotnet-int8",
	})
}
