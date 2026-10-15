package hazard

import (
	"math"
	"testing"
)

func TestSurvivalIsMonotoneAndNormalized(t *testing.T) {
	z := Covariates{Omega: 0.2, NetRTT: 0.3}
	b := Beta{Omega: 0.45, Latency: 0.02}
	kappa, lambda := 1.85, 6.5

	prev := 1.0
	for _, s := range []float64{0, 1, 2, 4, 6.5, 10, 20, 100} {
		sv := Survival(s, kappa, lambda, z, b)
		if sv > prev+1e-12 {
			t.Fatalf("survival increased at s=%v: %v > %v", s, sv, prev)
		}
		if sv < 0 || sv > 1 {
			t.Fatalf("survival out of range at s=%v: %v", s, sv)
		}
		prev = sv
	}
	if last := Survival(1e6, kappa, lambda, z, b); last > 1e-9 {
		t.Fatalf("survival should vanish for large s, got %v", last)
	}
	if RecoveryProbability(0, kappa, lambda, z, b) != 0 {
		t.Fatal("recovery probability at s=0 must be 0")
	}
}

func TestRecoveryDensityIntegratesToOne(t *testing.T) {
	z := Covariates{}
	b := Beta{}
	kappa, lambda := 1.85, 6.5
	const steps = 200000
	const dt = 0.0005
	var integral float64
	for i := 0; i < steps; i++ {
		s := float64(i) * dt
		integral += RecoveryDensity(s, kappa, lambda, z, b) * dt
	}
	if math.Abs(integral-1) > 0.01 {
		t.Fatalf("density integral = %v, want ~1", integral)
	}
}

func TestReentryTimeFollowsWeibullMean(t *testing.T) {
	z := Covariates{Omega: 0.1, NetRTT: 0.2}
	b := Beta{Omega: 0.45, Latency: 0.02}
	kappa, lambda := 1.85, 6.5
	rng := NewXoro128()

	const n = 20000
	var sum float64
	for i := 0; i < n; i++ {
		u, err := rng.Uniform()
		if err != nil {
			t.Fatal(err)
		}
		s := ReentryTime(0, u, kappa, lambda, z, b)
		if s <= 0 || math.IsNaN(s) || math.IsInf(s, 0) {
			t.Fatalf("invalid reentry time %v for u=%v", s, u)
		}
		sum += s
	}
	mean := sum / n
	want := MeanDwell(kappa, lambda, z, b)
	relErr := math.Abs(mean-want) / want
	if relErr > 0.05 {
		t.Fatalf("mean dwell %v vs analytic %v (rel err %.3f > 0.05)", mean, want, relErr)
	}
}

func TestReentryTimesAreDistinctWithProbabilityOne(t *testing.T) {
	z := Covariates{}
	b := Beta{}
	kappa, lambda := 1.85, 6.5
	rng := NewXoro128()

	const k = 64
	seen := map[float64]int{}
	for i := 0; i < k; i++ {
		u, _ := rng.Uniform()
		s := ReentryTime(0, u, kappa, lambda, z, b)
		seen[s]++
	}
	for s, c := range seen {
		if c > 1 {
			t.Fatalf("two proxies drew the identical re-entry instant %v (%d times)", s, c)
		}
	}
}

func TestPositiveDurationDependence(t *testing.T) {
	kappa, lambda := 1.85, 6.5
	h1 := BaselineHazard(2.0, kappa, lambda)
	h2 := BaselineHazard(8.0, kappa, lambda)
	if h2 <= h1 {
		t.Fatalf("kappa>1 requires increasing hazard: h(2)=%v h(8)=%v", h1, h2)
	}
	// kappa < 1 inverts the dependence (the paper's failure mode).
	if BaselineHazard(8, 0.8, lambda) >= BaselineHazard(2, 0.8, lambda) {
		t.Fatal("kappa<1 must be decreasing")
	}
}

func TestCovariateDotIsClamped(t *testing.T) {
	c := Covariates{Omega: 1e9, NetRTT: 1e9}
	if v := c.Dot(Beta{Omega: 1, Latency: 1}); v != 30 {
		t.Fatalf("dot = %v, want clamp at 30", v)
	}
}
