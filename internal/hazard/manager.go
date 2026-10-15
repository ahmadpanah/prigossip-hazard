package hazard

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/ahmadpanah/prigossip-hazard/internal/config"
)

// Record is the persistent quarantine descriptor stored by the Hazard
// Manager (Listing: QuarantineRecord).
type Record struct {
	InstanceID int
	EntryTime  time.Time
	Covariates Covariates
	ThresholdU float64 // sampled exactly once at entry
}

// Manager owns the quarantine table, the persistent uniform thresholds, the
// ring buffer of recovery tuples and the online Cox refit.
type Manager struct {
	spec config.Spec
	rng  *Xoro128

	mu        sync.Mutex
	records   map[int]Record
	ring      []Observation
	beta      Beta
	kappa     float64
	lambda    float64
	censored  uint64
	freed     uint64
	refits    uint64
	onRecover func(Record, time.Duration)

	now func() time.Time
}

// NewManager builds a manager initialized with the CRD hazard parameters.
func NewManager(spec config.Spec) *Manager {
	return &Manager{
		spec:    spec,
		rng:     NewXoro128(),
		records: make(map[int]Record),
		ring:    make([]Observation, 0, spec.CoxRingSize),
		beta:    Beta{Omega: spec.Hazard.BetaOmega, Latency: spec.Hazard.BetaLatency},
		kappa:   spec.Hazard.WeibullShape,
		lambda:  spec.Hazard.ScaleParamSec,
		now:     time.Now,
	}
}

// OnRecover registers the callback fired when an instance leaves quarantine.
func (m *Manager) OnRecover(fn func(Record, time.Duration)) { m.onRecover = fn }

// Params returns the current Weibull shape and scale.
func (m *Manager) Params() (kappa, lambda float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.kappa, m.lambda
}

// Beta returns the current regression coefficients.
func (m *Manager) Beta() Beta {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.beta
}

// Active reports whether an instance is currently quarantined.
func (m *Manager) Active(instance int) bool {
	m.mu.Lock()
	_, ok := m.records[instance]
	m.mu.Unlock()
	return ok
}

// Count returns the number of quarantined instances.
func (m *Manager) Count() int {
	m.mu.Lock()
	n := len(m.records)
	m.mu.Unlock()
	return n
}

// Stats returns (released, censored, refits) counters.
func (m *Manager) Stats() (released, censored, refits uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.freed, m.censored, m.refits
}

// Quarantine starts a new episode for instance: it draws the persistent
// threshold U_{k,m} exactly once (Equation single_draw_u) and stores the
// descriptor. Returns false if the instance is already quarantined.
func (m *Manager) Quarantine(instance int, now time.Time, z Covariates) (Record, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.records[instance]; exists {
		return Record{}, false
	}
	u, err := m.rng.Uniform()
	if err != nil {
		u = 0.5
	}
	rec := Record{
		InstanceID: instance,
		EntryTime:  now,
		Covariates: z,
		ThresholdU: u,
	}
	m.records[instance] = rec
	return rec, true
}

// Release removes an instance from quarantine without observing recovery
// (used when the pod is evicted or reconfigured).
func (m *Manager) Release(instance int) {
	m.mu.Lock()
	delete(m.records, instance)
	m.mu.Unlock()
}

// Tick evaluates the recovery condition of Equation (staggered_condition)
// against each persistent threshold and returns the instances that may be
// re-admitted together with their realized dwell times.
func (m *Manager) Tick(now time.Time) []Release {
	m.mu.Lock()
	defer m.mu.Unlock()

	var out []Release
	kappa, lambda := m.kappa, m.lambda
	beta := m.beta
	horizon := m.spec.Horizon()

	for id, rec := range m.records {
		s := now.Sub(rec.EntryTime).Seconds()
		if s < 0 {
			s = 0
		}
		prob := RecoveryProbability(s, kappa, lambda, rec.Covariates, beta)
		expired := s >= horizon.Seconds()
		if prob < rec.ThresholdU && !expired {
			continue
		}
		delete(m.records, id)
		obs := Observation{Dwell: s, Event: !expired, Z: rec.Covariates}
		if expired {
			obs.Dwell = horizon.Seconds()
			m.censored++
		} else {
			m.freed++
		}
		m.ring = append(m.ring, obs)
		if len(m.ring) > m.spec.CoxRingSize {
			m.ring = m.ring[len(m.ring)-m.spec.CoxRingSize:]
		}
		out = append(out, Release{Record: rec, Dwell: time.Duration(s * float64(time.Second)), ReentryTime: now})
	}
	return out
}

// Release describes a completed quarantine episode.
type Release struct {
	Record      Record
	Dwell       time.Duration
	ReentryTime time.Time
}

// ScheduledReentry returns the analytic re-admission instant of Equation
// (analytic_tre) for an active record, i.e. how much longer the instance
// must remain quarantined.
func (m *Manager) ScheduledReentry(rec Record) time.Time {
	m.mu.Lock()
	kappa, lambda, beta := m.kappa, m.lambda, m.beta
	m.mu.Unlock()
	s := ReentryTime(0, rec.ThresholdU, kappa, lambda, rec.Covariates, beta)
	return rec.EntryTime.Add(time.Duration(s * float64(time.Second)))
}

// Refit performs the damped Newton-Raphson update of Equation
// (newton_raphson), extracts the Breslow baseline and re-estimates the
// Weibull parameters. Safe to call when fewer than two events exist.
func (m *Manager) Refit() {
	m.mu.Lock()
	if len(m.ring) < 2 {
		m.mu.Unlock()
		return
	}
	obs := make([]Observation, len(m.ring))
	copy(obs, m.ring)
	eta := m.spec.NewtonEta
	beta := m.beta
	m.mu.Unlock()

	// Two sweeps keep the update stable without over-fitting a 200-tuple ring.
	for i := 0; i < 2; i++ {
		beta = NewtonRaphson(beta, obs, eta)
	}

	pts := BreslowBaseline(beta, obs)
	if kappa, lambda, ok := FitWeibull(pts); ok {
		m.mu.Lock()
		m.kappa, m.lambda = kappa, lambda
		m.beta = beta
		m.refits++
		m.mu.Unlock()
		return
	}
	m.mu.Lock()
	m.beta = beta
	m.refits++
	m.mu.Unlock()
}

// Run drives the two maintenance loops: the 500 ms hazard evaluation tick and
// the H_d = 1.0 s parameter refit, both until ctx is cancelled.
func (m *Manager) Run(ctx context.Context) {
	tick := time.NewTicker(m.spec.HazardTick())
	defer tick.Stop()
	refit := time.NewTicker(m.spec.CoxUpdate())
	defer refit.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			for _, rel := range m.Tick(time.Now()) {
				if m.onRecover != nil {
					m.onRecover(rel.Record, rel.Dwell)
				}
			}
		case <-refit.C:
			k0, l0 := m.Params()
			m.Refit()
			k1, l1 := m.Params()
			if k1 != k0 || l1 != l0 {
				log.Printf("hazard: Weibull refit kappa=%.3f lambda=%.3fs", k1, l1)
			}
		}
	}
}
