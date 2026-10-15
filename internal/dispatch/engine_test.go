package dispatch

import (
	"testing"

	"github.com/ahmadpanah/prigossip-hazard/internal/config"
)

func testSpec(m int) config.Spec {
	s := config.Default()
	s.Instances = m
	if err := s.Validate(); err != nil {
		panic(err)
	}
	return s
}

func TestRebuildPartitionsAndNormalizes(t *testing.T) {
	e := NewEngine(testSpec(4))
	mu := []float64{0.99, 0.95, 0.30, 0.20}
	om := []float64{0, 0, 0, 0}
	active := []bool{true, true, true, true}

	e.Rebuild(mu, om, active)
	w := e.Weights()

	var sum float64
	for _, v := range w {
		sum += v
	}
	if sum < 0.999 || sum > 1.001 {
		t.Fatalf("weights sum to %v, want ~1", sum)
	}
	if w[2] <= 0 || w[3] <= 0 {
		t.Fatalf("exploration pool got no mass: %v", w)
	}
	if w[0] <= w[2] {
		t.Fatalf("exploitation arm should outweigh exploration arm: %v", w)
	}
}

func TestQuarantinedInstanceGetsZeroWeight(t *testing.T) {
	e := NewEngine(testSpec(3))
	e.Rebuild([]float64{0.99, 0.99, 0.99}, []float64{0, 0, 0}, []bool{true, false, true})
	w := e.Weights()
	if w[1] != 0 {
		t.Fatalf("quarantined weight=%v want 0", w[1])
	}
	if w[0]+w[2] < 0.999 {
		t.Fatalf("remaining weights sum to %v", w[0]+w[2])
	}
}

func TestVolatilityPenaltySuppressesUnstableArm(t *testing.T) {
	s := testSpec(2)
	e := NewEngine(s)
	e.Rebuild([]float64{0.99, 0.99}, []float64{0, 2.0}, []bool{true, true})
	w := e.Weights()
	if w[1] >= w[0] {
		t.Fatalf("volatile arm not penalized: %v", w)
	}
}

func TestSWRRDistributesProportionally(t *testing.T) {
	s := testSpec(2)
	e := NewEngine(s)
	e.mu.Lock()
	e.weights = []float64{0.75, 0.25}
	e.mu.Unlock()

	counts := map[int]int{}
	for i := 0; i < 400; i++ {
		counts[e.Pick()]++
	}
	if counts[0] < 280 || counts[0] > 320 {
		t.Fatalf("arm0 picked %d/400 times, want ~300", counts[0])
	}
	if counts[1] < 80 || counts[1] > 120 {
		t.Fatalf("arm1 picked %d/400 times, want ~100", counts[1])
	}
}

func TestPickReturnsMinusOneWhenAllInactive(t *testing.T) {
	e := NewEngine(testSpec(2))
	e.Rebuild([]float64{0.99, 0.99}, []float64{0, 0}, []bool{false, false})
	if got := e.Pick(); got != -1 {
		t.Fatalf("Pick()=%d want -1", got)
	}
}

func TestDecayAndRaise(t *testing.T) {
	e := NewEngine(testSpec(2))
	start := e.Explore()
	for i := 0; i < 50; i++ {
		e.Decay()
	}
	if e.Explore() >= start {
		t.Fatalf("explore budget did not decay: %v -> %v", start, e.Explore())
	}
	e.RaiseExplore()
	if e.Explore() < 0.09 || e.Explore() > 0.11 {
		t.Fatalf("raise should restore 1-rho=0.1, got %v", e.Explore())
	}
}
