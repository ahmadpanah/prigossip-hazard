// Package dispatch implements the adaptive weight allocation and request
// dispatching of PriGossip-Hazard: exploitation/exploration pooling, the
// volatility-penalized scores of Equations (score_exploit)-(final_weights)
// and the Smooth Weighted Round Robin picker.
package dispatch

import (
	"math"
	"sync"

	"github.com/ahmadpanah/prigossip-hazard/internal/config"
)

// Engine computes normalized routing weights and hands out per-request
// instance selections. Safe for concurrent use by multiple request paths.
type Engine struct {
	spec config.Spec

	mu      sync.Mutex
	weights []float64
	current []float64
	explore float64
	poolE   []int
	poolX   []int
}

// NewEngine allocates the dispatcher for M instances.
func NewEngine(spec config.Spec) *Engine {
	m := spec.Instances
	e := &Engine{
		spec:    spec,
		weights: make([]float64, m),
		current: make([]float64, m),
		explore: spec.Dispatch.ExploreReset,
	}
	if e.explore <= 0 {
		e.explore = 1 - spec.SLA.SuccessRatio
	}
	return e
}

// Explore returns the current exploration budget epsilon(t).
func (e *Engine) Explore() float64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.explore
}

// Weights returns a copy of the current normalized weight vector.
func (e *Engine) Weights() []float64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]float64, len(e.weights))
	copy(out, e.weights)
	return out
}

// Pools returns the current exploitation and exploration pools.
func (e *Engine) Pools() (exploit, explore []int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]int(nil), e.poolE...), append([]int(nil), e.poolX...)
}

// Rebuild recomputes the pools, the volatility-penalized scores and the
// final normalized routing weights from fresh telemetry.
//
//	muHat : KDE estimates per instance (qos.Window.KDE)
//	omega : epistemic volatility belief Omega_k(t)
//	active: false when the instance is quarantined or unreachable
func (e *Engine) Rebuild(muHat, omega []float64, active []bool) {
	e.mu.Lock()
	defer e.mu.Unlock()

	rho := e.spec.SLA.SuccessRatio
	theta := e.spec.Dispatch.VolatilityTheta
	eta := e.spec.Dispatch.MinRegularizer
	m := len(e.weights)
	if len(muHat) < m || len(omega) < m || len(active) < m {
		return
	}

	e.poolE = e.poolE[:0]
	e.poolX = e.poolX[:0]
	for i := 0; i < m; i++ {
		if !active[i] {
			continue
		}
		if muHat[i] >= rho {
			e.poolE = append(e.poolE, i)
		} else {
			e.poolX = append(e.poolX, i)
		}
	}

	for i := range e.weights {
		e.weights[i] = 0
	}

	eps := e.explore
	var sumE, sumX float64
	scoreE := make([]float64, m)
	scoreX := make([]float64, m)
	penalty := func(i int) float64 { return math.Exp(-theta * omega[i]) }

	for _, i := range e.poolE {
		s := (muHat[i] - rho) * penalty(i)
		if s < eta {
			s = eta
		}
		scoreE[i] = s
		sumE += s
	}
	for _, i := range e.poolX {
		s := muHat[i] * penalty(i)
		if s < eta {
			s = eta
		}
		scoreX[i] = s
		sumX += s
	}

	// Redistribute the exploration budget when one of the pools is empty so
	// that no traffic is ever black-holed.
	switch {
	case len(e.poolE) == 0 && len(e.poolX) == 0:
		return
	case len(e.poolE) == 0:
		eps = 1
	case len(e.poolX) == 0:
		eps = 0
	}

	if sumE > 0 {
		for _, i := range e.poolE {
			e.weights[i] = (1 - eps) * scoreE[i] / sumE
		}
	}
	if sumX > 0 {
		for _, i := range e.poolX {
			e.weights[i] += eps * scoreX[i] / sumX
		}
	}
}

// Decay reduces the exploration budget after each maintenance epoch
// (epsilon-decay).
func (e *Engine) Decay() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.explore *= e.spec.Dispatch.ExploreDecay
	if e.explore < 1e-4 {
		e.explore = 1e-4
	}
}

// RaiseExplore restores the exploration budget (reset trigger), used after an
// instance leaves quarantine or when the exploitation pool becomes empty.
func (e *Engine) RaiseExplore() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.explore = 1 - e.spec.SLA.SuccessRatio
}

// Pick selects an instance with Smooth Weighted Round Robin over the current
// weights, returning -1 when no instance is dispatchable.
func (e *Engine) Pick() int {
	e.mu.Lock()
	defer e.mu.Unlock()

	total := 0.0
	best := -1
	bestCW := -math.MaxFloat64
	for i, w := range e.weights {
		if w <= 0 {
			continue
		}
		e.current[i] += w
		total += w
		if e.current[i] > bestCW {
			bestCW = e.current[i]
			best = i
		}
	}
	if best < 0 || total <= 0 {
		return -1
	}
	e.current[best] -= total
	return best
}
