package hazard

import "math"

// Online partial-likelihood machinery of Section "Offline Warm-Start,
// Censoring Protocol, and Online Parameter Adaptation".

// Observation is one right-censored recovery tuple (s_i, delta_i, z_i).
type Observation struct {
	Dwell float64    // s_i, seconds (capped at Delta_max)
	Event bool       // delta_i = 1 when full recovery was observed
	Z     Covariates // covariate vector
}

// LogPartialLikelihood evaluates Equation (partial_likelihood_formal):
//
//	l(beta) = sum_i delta_i [ beta^T z_i - ln sum_{j in R(s_i)} exp(beta^T z_j) ]
//
// The risk set R(s_i) contains observations with s_j >= s_i.
func LogPartialLikelihood(beta Beta, obs []Observation) float64 {
	total := 0.0
	for _, oi := range obs {
		if !oi.Event {
			continue
		}
		total += oi.Z.Dot(beta)
		denom := 0.0
		for _, oj := range obs {
			if oj.Dwell < oi.Dwell {
				continue
			}
			denom += math.Exp(oj.Z.Dot(beta))
		}
		if denom <= 0 {
			return math.Inf(-1)
		}
		total -= math.Log(denom)
	}
	return total
}

// Gradient evaluates Equation (gradient_cox): sum over events of
// (z_i - zbar(s_i, beta)).
func Gradient(beta Beta, obs []Observation) Beta {
	var g Beta
	for _, oi := range obs {
		if !oi.Event {
			continue
		}
		mean := riskSetMean(beta, obs, oi.Dwell)
		g.Omega += oi.Z.Omega - mean.Omega
		g.Latency += oi.Z.NetRTT - mean.NetRTT
	}
	return g
}

// Hessian evaluates Equation (hessian_cox): negative sum of the risk-set
// covariance of the covariates under the exponential tilt.
func Hessian(beta Beta, obs []Observation) (hOO, hOL, hLL float64) {
	for _, oi := range obs {
		if !oi.Event {
			continue
		}
		mean := riskSetMean(beta, obs, oi.Dwell)
		secondOO, secondOL, secondLL := riskSetSecond(beta, obs, oi.Dwell)
		hOO += secondOO - mean.Omega*mean.Omega
		hOL += secondOL - mean.Omega*mean.NetRTT
		hLL += secondLL - mean.NetRTT*mean.NetRTT
	}
	return -hOO, -hOL, -hLL
}

// riskSetMean returns zbar(s_i, beta): the exponentially tilted mean of the
// covariates over the risk set R(s_i) = {j : s_j >= s_i}.
func riskSetMean(beta Beta, obs []Observation, dwell float64) Covariates {
	var wsum float64
	for j := range obs {
		if obs[j].Dwell < dwell {
			continue
		}
		wsum += math.Exp(obs[j].Z.Dot(beta))
	}
	if wsum <= 0 {
		return Covariates{}
	}
	var mo, ml float64
	for j := range obs {
		if obs[j].Dwell < dwell {
			continue
		}
		w := math.Exp(obs[j].Z.Dot(beta)) / wsum
		mo += w * obs[j].Z.Omega
		ml += w * obs[j].Z.NetRTT
	}
	return Covariates{Omega: mo, NetRTT: ml}
}

func riskSetSecond(beta Beta, obs []Observation, dwell float64) (oo, ol, ll float64) {
	var wsum float64
	for j := range obs {
		if obs[j].Dwell < dwell {
			continue
		}
		wsum += math.Exp(obs[j].Z.Dot(beta))
	}
	if wsum <= 0 {
		return
	}
	for j := range obs {
		if obs[j].Dwell < dwell {
			continue
		}
		w := math.Exp(obs[j].Z.Dot(beta)) / wsum
		zo, zl := obs[j].Z.Omega, obs[j].Z.NetRTT
		oo += w * zo * zo
		ol += w * zo * zl
		ll += w * zl * zl
	}
	return
}

// NewtonRaphson performs the damped update of Equation (newton_raphson):
//
//	beta <- beta - eta * H^{-1} grad
//
// A 2x2 solve keeps the update cheap enough for the 1 Hz maintenance cadence
// (H_d = 1.0 s) on a single edge core.
func NewtonRaphson(beta Beta, obs []Observation, eta float64) Beta {
	if len(obs) == 0 {
		return beta
	}
	g := Gradient(beta, obs)
	hOO, hOL, hLL := Hessian(beta, obs)

	// Regularize toward the identity for numerical stability.
	const lam = 1e-4
	hOO -= lam
	hLL -= lam

	det := hOO*hLL - hOL*hOL
	if math.Abs(det) < 1e-12 {
		return beta
	}
	// Solve H d = g, then beta <- beta - eta * d.
	dO := (hLL*g.Omega - hOL*g.Latency) / det
	dL := (-hOL*g.Omega + hOO*g.Latency) / det
	return Beta{
		Omega:   clamp(beta.Omega-eta*dO, -5, 5),
		Latency: clamp(beta.Latency-eta*dL, -5, 5),
	}
}

// BreslowBaseline evaluates Equation (breslow_estimator) at the sorted event
// times: H0(s) = sum_{s_i <= s} delta_i / sum_{j in R(s_i)} exp(beta^T z_j).
func BreslowBaseline(beta Beta, obs []Observation) []Point {
	events := make([]Observation, 0, len(obs))
	for _, o := range obs {
		if o.Event {
			events = append(events, o)
		}
	}
	sortObservations(events)
	out := make([]Point, 0, len(events))
	var cum float64
	for _, e := range events {
		var denom float64
		for _, o := range obs {
			if o.Dwell >= e.Dwell {
				denom += math.Exp(o.Z.Dot(beta))
			}
		}
		if denom <= 0 {
			continue
		}
		cum += 1 / denom
		out = append(out, Point{X: e.Dwell, Y: cum})
	}
	return out
}

// Point is a (dwell, cumulative hazard) pair used by the Weibull refit.
type Point struct {
	X float64
	Y float64
}

// FitWeibull regresses ln H0(s) = kappa * ln s - kappa * ln lambda by
// ordinary least squares, yielding updated shape and scale parameters.
func FitWeibull(pts []Point) (kappa, lambda float64, ok bool) {
	type pair struct{ x, y float64 }
	var data []pair
	for _, p := range pts {
		if p.X <= 0 || p.Y <= 0 {
			continue
		}
		data = append(data, pair{math.Log(p.X), math.Log(p.Y)})
	}
	if len(data) < 2 {
		return 0, 0, false
	}
	var n = float64(len(data))
	var sx, sy, sxx, sxy float64
	for _, d := range data {
		sx += d.x
		sy += d.y
		sxx += d.x * d.x
		sxy += d.x * d.y
	}
	den := n*sxx - sx*sx
	if math.Abs(den) < 1e-12 {
		return 0, 0, false
	}
	a := (n*sxy - sx*sy) / den // slope = kappa
	b := (sy - a*sx) / n       // intercept = -kappa ln lambda
	if a <= 0 {
		return 0, 0, false
	}
	return a, math.Exp(-b / a), true
}

// sortObservations orders tuples by dwell time ascending (insertion sort; the
// ring buffer holds at most K_rec = 200 tuples).
func sortObservations(obs []Observation) {
	for i := 1; i < len(obs); i++ {
		for j := i; j > 0 && obs[j].Dwell < obs[j-1].Dwell; j-- {
			obs[j], obs[j-1] = obs[j-1], obs[j]
		}
	}
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
