package gossip

import "sync"

// Belief maintains the epistemic volatility matrix Omega_k in R^M and applies
// the exponentially weighted asynchronous consensus operator of Equation
// (gossip_consensus):
//
//	Omega_m <- (1 - alpha_g) * Omega_m + alpha_g * v~_peer,m
type Belief struct {
	mu     sync.RWMutex
	omega  []float64
	alpha  float64
	peerTs map[int]uint64 // last integrated timestamp per peer (dedup)
}

// NewBelief allocates a zero-initialized belief vector for M instances with
// learning rate alpha_g.
func NewBelief(m int, alpha float64) *Belief {
	return &Belief{
		omega:  make([]float64, m),
		alpha:  alpha,
		peerTs: make(map[int]uint64),
	}
}

// Observe integrates one received sketch. The timestamp guards against
// replaying the same datagram twice within a gossip round.
func (b *Belief) Observe(peerID int, ts uint64, sketch []float64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if last, ok := b.peerTs[peerID]; ok && ts <= last {
		return false
	}
	b.peerTs[peerID] = ts
	for i := range b.omega {
		if i >= len(sketch) {
			break
		}
		b.omega[i] = (1-b.alpha)*b.omega[i] + b.alpha*sketch[i]
	}
	return true
}

// Values returns a copy of Omega_k(t).
func (b *Belief) Values() []float64 {
	b.mu.RLock()
	out := make([]float64, len(b.omega))
	copy(out, b.omega)
	b.mu.RUnlock()
	return out
}

// At returns Omega_{k,m}.
func (b *Belief) At(m int) float64 {
	b.mu.RLock()
	v := 0.0
	if m >= 0 && m < len(b.omega) {
		v = b.omega[m]
	}
	b.mu.RUnlock()
	return v
}
