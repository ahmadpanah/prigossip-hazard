// Package gossip implements the locally differentially private epistemic
// gossip protocol of PriGossip-Hazard: variance sketch construction with
// calibrated Laplace noise (Equation ldp_perturbation), the exponentially
// weighted consensus operator (Equation gossip_consensus) and the UDP
// transport daemon described in Section "Gossip Transport Daemon".
package gossip

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"math"

	"github.com/ahmadpanah/prigossip-hazard/internal/config"
	"github.com/ahmadpanah/prigossip-hazard/internal/qos"
)

// Source supplies per-instance observation windows; it is satisfied by the
// telemetry registry of the agent.
type Source interface {
	Window(instance int) *qos.Window
	Instances() int
}

// RNG abstracts the uniform source so tests can inject deterministic streams.
type RNG interface {
	// Uniform returns a value in the open interval (0, 1).
	Uniform() (float64, error)
}

// CryptoRNG maps crypto/rand bytes onto (0,1); the paper specifies a
// cryptographically secure generator for the Laplace draws.
type CryptoRNG struct{}

func (CryptoRNG) Uniform() (float64, error) {
	for {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			return 0, err
		}
		v := binary.LittleEndian.Uint64(b[:])
		if v == 0 {
			continue // reject 0 so -ln(1-u) is finite
		}
		// 53-bit mantissa mapping into (0,1)
		u := float64(v>>11) / float64(1<<53)
		if u <= 0 {
			u = math.SmallestNonzeroFloat64
		}
		if u >= 1 {
			u = math.Nextafter(1, 0)
		}
		return u, nil
	}
}

// SampleLaplace draws from Lap(b) via inverse-CDF sampling:
//
//	X = -b * sgn(u-0.5) * ln(1 - 2|u-0.5|)
func SampleLaplace(u, scale float64) float64 {
	if scale <= 0 {
		return 0
	}
	x := u - 0.5
	sign := 1.0
	if x < 0 {
		sign = -1
	}
	return -scale * sign * math.Log(1-2*math.Abs(x))
}

// Sketcher turns local latency histories into the perturbed vector
// \tilde{v}_k(t) transmitted to peers.
type Sketcher struct {
	spec config.Spec
	src  Source
	rng  RNG
}

// NewSketcher wires a sketcher to its observation source.
func NewSketcher(spec config.Spec, src Source, rng RNG) *Sketcher {
	if rng == nil {
		rng = CryptoRNG{}
	}
	return &Sketcher{spec: spec, src: src, rng: rng}
}

// Sensitivity returns the global L1 sensitivity of the variance vector,
// Equation (l1_vector_sensitivity): Delta v_1 = tau_max^2 / n_min with
// n_min = min_m n_{k,m} over instances holding at least two samples.
func (s *Sketcher) Sensitivity() float64 {
	nMin := math.MaxInt32
	m := s.src.Instances()
	for i := 0; i < m; i++ {
		w := s.src.Window(i)
		if w == nil {
			continue
		}
		n := w.Len()
		if n < 2 {
			continue
		}
		if n < nMin {
			nMin = n
		}
	}
	if nMin == math.MaxInt32 {
		return 0 // nothing observable yet, no data-dependent sensitivity
	}
	tauMax := s.spec.MaxLatencyMs
	return tauMax * tauMax / float64(nMin)
}

// VMax is the maximum variance of any distribution supported on
// [0, tau_max], i.e. tau_max^2 / 4 (projection ceiling).
func (s *Sketcher) VMax() float64 {
	tauMax := s.spec.MaxLatencyMs
	return tauMax * tauMax / 4
}

// RawVariance returns the unperturbed empirical variance vector v_k(t).
func (s *Sketcher) RawVariance() []float64 {
	m := s.src.Instances()
	out := make([]float64, m)
	for i := 0; i < m; i++ {
		if w := s.src.Window(i); w != nil {
			out[i] = w.Variance()
		}
	}
	return out
}

// Build applies Equation (ldp_perturbation):
//
//	v~ = Pi_[0,vmax]( v + Lap(Delta_v1 / eps) )
//
// and returns the transmitted vector together with the noise scale used.
func (s *Sketcher) Build() (vec []float64, scale float64, err error) {
	raw := s.RawVariance()
	delta := s.Sensitivity()
	scale = delta / s.spec.Gossip.PrivacyEpsilon
	vmax := s.VMax()
	vec = make([]float64, len(raw))
	for i, v := range raw {
		u, uerr := s.rng.Uniform()
		if uerr != nil {
			return nil, 0, fmt.Errorf("rng: %w", uerr)
		}
		noisy := v + SampleLaplace(u, scale)
		vec[i] = math.Min(math.Max(0, noisy), vmax)
	}
	return vec, scale, nil
}

// LaplaceScale is the calibrated noise scale Delta_v1 / epsilon.
func (s *Sketcher) LaplaceScale() float64 {
	return s.Sensitivity() / s.spec.Gossip.PrivacyEpsilon
}
