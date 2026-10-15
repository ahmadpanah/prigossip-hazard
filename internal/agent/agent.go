// Package agent wires the PriGossip-Hazard control loop together: telemetry
// ingestion, KDE QoS estimation, LDP gossip, survival-theoretic quarantine
// and SWRR dispatch (Algorithm: "PriGossip-Hazard: Decentralized QoS-Aware
// Routing").
package agent

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/ahmadpanah/prigossip-hazard/internal/config"
	"github.com/ahmadpanah/prigossip-hazard/internal/dispatch"
	"github.com/ahmadpanah/prigossip-hazard/internal/gossip"
	"github.com/ahmadpanah/prigossip-hazard/internal/hazard"
	"github.com/ahmadpanah/prigossip-hazard/internal/qos"
)

// Agent is the per-node control plane of the DaemonSet.
type Agent struct {
	spec config.Spec

	windows []*qos.Window
	netRTT  []float64 // running propagation floor l^n_{k,m}
	errCnt  []int     // consecutive SLA violations e_{k,m}

	belief   *gossip.Belief
	sketcher *gossip.Sketcher
	hazard   *hazard.Manager
	disp     *dispatch.Engine

	mu          sync.Mutex
	quarant     int
	recovered   int
	observed    uint64
	kernelTelem bool
}

// New builds an agent for M service instances.
func New(spec config.Spec) *Agent {
	m := spec.Instances
	a := &Agent{
		spec:    spec,
		windows: make([]*qos.Window, m),
		netRTT:  make([]float64, m),
		errCnt:  make([]int, m),
	}
	for i := range a.windows {
		a.windows[i] = qos.NewWindow(spec.RingCapacity)
		a.netRTT[i] = spec.MaxLatencyMs // conservative until first sample
	}
	a.belief = gossip.NewBelief(m, spec.Gossip.LearningRate)
	a.sketcher = gossip.NewSketcher(spec, a, nil)
	a.hazard = hazard.NewManager(spec)
	a.disp = dispatch.NewEngine(spec)
	a.hazard.OnRecover(func(rec hazard.Record, dwell time.Duration) {
		a.mu.Lock()
		a.recovered++
		a.errCnt[rec.InstanceID] = 0
		a.mu.Unlock()
		a.disp.RaiseExplore()
		log.Printf("agent: instance %d re-admitted after %.2fs (U=%.4f)",
			rec.InstanceID, dwell.Seconds(), rec.ThresholdU)
	})
	return a
}

// --- gossip.Source implementation ---

// Window returns the observation window of instance m (nil when out of range).
func (a *Agent) Window(instance int) *qos.Window {
	if instance < 0 || instance >= len(a.windows) {
		return nil
	}
	return a.windows[instance]
}

// Instances returns M.
func (a *Agent) Instances() int { return len(a.windows) }

// NodeName returns the node identity of this agent (POD_NAME / NODE_NAME),
// used in diagnostic headers and logs.
func (a *Agent) NodeName() string {
	if a.spec.NodeName != "" {
		return a.spec.NodeName
	}
	return "agent"
}

// --- accessors for the surrounding components ---

// Hazard exposes the quarantine manager.
func (a *Agent) Hazard() *hazard.Manager { return a.hazard }

// Belief exposes the epistemic volatility matrix.
func (a *Agent) Belief() *gossip.Belief { return a.belief }

// Sketcher exposes the LDP sketch builder.
func (a *Agent) Sketcher() *gossip.Sketcher { return a.sketcher }

// Dispatcher exposes the routing engine.
func (a *Agent) Dispatcher() *dispatch.Engine { return a.disp }

// Covariates returns z = [Omega_{k,m}, l^n_{k,m}] on the normalized scale
// expected by the Cox model.
func (a *Agent) Covariates(instance int) hazard.Covariates {
	if instance < 0 || instance >= len(a.windows) {
		return hazard.Covariates{}
	}
	vmax := a.sketcher.VMax()
	om := a.belief.At(instance)
	if vmax > 0 {
		om /= vmax
	}
	return hazard.Covariates{
		Omega:  om,
		NetRTT: a.netRTT[instance] / a.spec.MaxLatencyMs,
	}
}

// Active reports whether instance m is currently dispatchable.
func (a *Agent) Active(instance int) bool {
	return !a.hazard.Active(instance)
}

// Stats snapshots agent counters for the status endpoint.
type Stats struct {
	Observed             uint64  `json:"observations"`
	QuarantineEvents     int     `json:"quarantineEvents"`
	CurrentlyQuarantined int     `json:"currentlyQuarantined"`
	Recovered            int     `json:"recovered"`
	Released             uint64  `json:"released"`
	Censored             uint64  `json:"censored"`
	Refits               uint64  `json:"refits"`
	Explore              float64 `json:"exploreBudget"`
	Kappa                float64 `json:"weibullShape"`
	LambdaS              float64 `json:"weibullScaleSec"`
}

// Stats returns the current counters.
func (a *Agent) Stats() Stats {
	a.mu.Lock()
	s := Stats{
		Observed:             a.observed,
		QuarantineEvents:     a.quarant,
		CurrentlyQuarantined: len(a.hazardActive()),
		Recovered:            a.recovered,
	}
	a.mu.Unlock()
	s.Released, s.Censored, s.Refits = a.hazard.Stats()
	s.Explore = a.disp.Explore()
	s.Kappa, s.LambdaS = a.hazard.Params()
	return s
}

func (a *Agent) hazardActive() []int {
	out := []int{}
	for i := 0; i < a.Instances(); i++ {
		if a.hazard.Active(i) {
			out = append(out, i)
		}
	}
	return out
}

// SetKernelTelemetry switches latency accounting to the eBPF kprobe stream
// when the kernel probes are attached; the L7 proxy then stops double-
// counting its own measurements.
func (a *Agent) SetKernelTelemetry(on bool) {
	a.mu.Lock()
	a.kernelTelem = on
	a.mu.Unlock()
}

// KernelTelemetry reports whether kernel-side measurement is authoritative.
func (a *Agent) KernelTelemetry() bool {
	a.mu.Lock()
	on := a.kernelTelem
	a.mu.Unlock()
	return on
}

// Observe ingests one end-to-end latency measurement: it updates the sliding
// window, the network-floor estimate and the consecutive error counter, and
// triggers quarantine when e_{k,m} >= E_t (Algorithm, request-arrival block).
func (a *Agent) Observe(instance int, latencyMs float64) {
	if instance < 0 || instance >= len(a.windows) {
		return
	}
	a.windows[instance].Append(latencyMs)
	// Network floor l^n_{k,m}: a running minimum of the observed latency
	// (the propagation component) that creeps upward when the path degrades
	// permanently, so the Cox covariate tracks route changes.
	if latencyMs < a.netRTT[instance] {
		a.netRTT[instance] = latencyMs
	} else {
		a.netRTT[instance] = 0.999*a.netRTT[instance] + 0.001*latencyMs
	}

	a.mu.Lock()
	a.observed++
	if latencyMs > a.spec.SLA.LatencyThresholdMs {
		a.errCnt[instance]++
	} else {
		a.errCnt[instance] = 0
	}
	threshold := a.spec.Hazard.ErrorThreshold
	shouldQuarantine := a.errCnt[instance] >= threshold && !a.hazard.Active(instance)
	if shouldQuarantine {
		a.quarant++
	}
	a.mu.Unlock()

	if shouldQuarantine {
		z := a.Covariates(instance)
		if rec, ok := a.hazard.Quarantine(instance, time.Now(), z); ok {
			log.Printf("agent: instance %d quarantined (e=%d) U=%.4f entry=%s",
				instance, threshold, rec.ThresholdU, rec.EntryTime.Format(time.RFC3339Nano))
			a.disp.RaiseExplore()
		}
	}
}

// Maintenance performs one maintenance epoch (H_d-aligned): it re-admits
// released instances, recomputes pools and weights, and decays the
// exploration budget.
func (a *Agent) Maintenance() {
	// Re-admissions evaluated by the hazard manager loop are reflected here.
	m := a.Instances()
	muHat := make([]float64, m)
	omega := make([]float64, m)
	active := make([]bool, m)
	for i := 0; i < m; i++ {
		muHat[i] = a.windows[i].KDE(a.spec.SLA.LatencyThresholdMs)
		omega[i] = a.belief.At(i)
		active[i] = !a.hazard.Active(i)
	}
	a.disp.Rebuild(muHat, omega, active)
	a.disp.Decay()
}

// Route returns the instance selected by SWRR for the next request, or -1.
func (a *Agent) Route() int { return a.disp.Pick() }

// Run drives the maintenance epoch loop until ctx is cancelled. Gossip
// transmission and hazard evaluation run on their own tickers.
func (a *Agent) Run(ctx context.Context) {
	ticker := time.NewTicker(a.spec.HazardTick())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.Maintenance()
		}
	}
}
