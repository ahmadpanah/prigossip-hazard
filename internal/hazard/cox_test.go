package hazard

import (
	"math"
	"testing"
)

// makeObservations builds right-censored tuples whose dwell times follow a
// Weibull(kappa, lambda) distribution under a covariate-driven hazard.
func makeObservations(seed uint64, kappa, lambda float64, beta Beta, n int) []Observation {
	rng := &Xoro128{s0: seed, s1: seed ^ 0x9E3779B97F4A7C15}
	out := make([]Observation, 0, n)
	for i := 0; i < n; i++ {
		z := Covariates{
			Omega:  rng.UniformMust(),
			NetRTT: rng.UniformMust(),
		}
		u, _ := rng.Uniform()
		s := ReentryTime(0, u, kappa, lambda, z, beta)
		// Right-censor at 30 s (Delta_max).
		obs := Observation{Dwell: s, Event: true, Z: z}
		if s > 30 {
			obs.Dwell = 30
			obs.Event = false
		}
		out = append(out, obs)
	}
	return out
}

// UniformMust is a test helper on the generator.
func (r *Xoro128) UniformMust() float64 {
	v, err := r.Uniform()
	if err != nil {
		panic(err)
	}
	return v
}

func TestNewtonRaphsonRecoversCovariateSign(t *testing.T) {
	truth := Beta{Omega: 0.45, Latency: 0.02}
	obs := makeObservations(42, 1.85, 6.5, truth, 400)

	beta := Beta{}
	for i := 0; i < 40; i++ {
		beta = NewtonRaphson(beta, obs, 0.05)
	}
	if beta.Omega <= 0 {
		t.Fatalf("beta.Omega = %v, expected a positive coefficient", beta.Omega)
	}
	// The Omega covariate drives most of the variation, so the estimate must
	// land within a reasonable band of the generating value.
	if math.Abs(beta.Omega-truth.Omega) > 0.6 {
		t.Fatalf("beta.Omega = %v, want within 0.6 of %v", beta.Omega, truth.Omega)
	}
}

func TestPartialLikelihoodPrefersTrueBeta(t *testing.T) {
	truth := Beta{Omega: 0.45, Latency: 0.02}
	obs := makeObservations(7, 1.85, 6.5, truth, 300)
	llTrue := LogPartialLikelihood(truth, obs)
	llZero := LogPartialLikelihood(Beta{}, obs)
	if llTrue <= llZero {
		t.Fatalf("log-partial-likelihood at truth (%v) should exceed zero beta (%v)", llTrue, llZero)
	}
}

func TestFitWeibullRecoversParameters(t *testing.T) {
	kappa, lambda := 1.85, 6.5
	var pts []Point
	for i := 1; i <= 60; i++ {
		s := float64(i) * 0.25
		h := math.Pow(s/lambda, kappa) // exact cumulative hazard
		pts = append(pts, Point{X: s, Y: h})
	}
	gotK, gotL, ok := FitWeibull(pts)
	if !ok {
		t.Fatal("FitWeibull reported failure on exact data")
	}
	if math.Abs(gotK-kappa) > 1e-6 {
		t.Fatalf("kappa=%v want %v", gotK, kappa)
	}
	if math.Abs(gotL-lambda) > 1e-6 {
		t.Fatalf("lambda=%v want %v", gotL, lambda)
	}
}

func TestBreslowBaselineIsNonDecreasing(t *testing.T) {
	obs := makeObservations(11, 1.85, 6.5, Beta{Omega: 0.45, Latency: 0.02}, 200)
	pts := BreslowBaseline(Beta{Omega: 0.45, Latency: 0.02}, obs)
	if len(pts) < 2 {
		t.Fatalf("expected several events, got %d", len(pts))
	}
	for i := 1; i < len(pts); i++ {
		if pts[i].Y < pts[i-1].Y-1e-12 {
			t.Fatalf("baseline hazard decreased at index %d", i)
		}
	}
}
