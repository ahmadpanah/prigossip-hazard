package hazard

import "math"

// Weibull-Cox survival machinery, Section "Survival-Theoretic Dynamic
// Quarantine Engine".

// Covariates z = [Omega_normalized, network_latency_normalized].
// Normalization keeps beta^T z O(1); the paper's covariate weights
// (beta_Omega = 0.45, beta_l = 0.02) are calibrated on these scales.
type Covariates struct {
	Omega  float64 // Omega_{k,m} / v_max
	NetRTT float64 // l^n_{k,m} / tau_max
}

// Dot returns beta^T z, clamped so the exponential of the linear predictor
// stays finite for pathological telemetry.
func (c Covariates) Dot(beta Beta) float64 {
	v := beta.Omega*c.Omega + beta.Latency*c.NetRTT
	if v > 30 {
		v = 30
	} else if v < -30 {
		v = -30
	}
	return v
}

// Beta is the Cox regression coefficient vector beta = [beta_Omega, beta_l].
type Beta struct {
	Omega   float64
	Latency float64
}

// BaselineHazard is h_0(s) = (kappa/lambda)(s/lambda)^{kappa-1} for the
// Weibull baseline with shape kappa > 1 (positive duration dependence).
func BaselineHazard(s, kappa, lambda float64) float64 {
	if s <= 0 || lambda <= 0 || kappa <= 0 {
		return 0
	}
	return (kappa / lambda) * math.Pow(s/lambda, kappa-1)
}

// Survival returns S_{k,m}(s) = exp(-(s/lambda)^kappa * exp(beta^T z)),
// Equation (survival_function).
func Survival(s, kappa, lambda float64, z Covariates, beta Beta) float64 {
	if s <= 0 {
		return 1
	}
	lin := z.Dot(beta)
	pow := math.Pow(s/lambda, kappa) * math.Exp(lin)
	if pow > 700 {
		return 0
	}
	return math.Exp(-pow)
}

// RecoveryProbability returns F_{k,m}(s) = 1 - S_{k,m}(s),
// Equation (recovery_prob).
func RecoveryProbability(s, kappa, lambda float64, z Covariates, beta Beta) float64 {
	return 1 - Survival(s, kappa, lambda, z, beta)
}

// RecoveryDensity is f_{k,m}(s) of Equation (pdf_recovery).
func RecoveryDensity(s, kappa, lambda float64, z Covariates, beta Beta) float64 {
	if s <= 0 {
		return 0
	}
	lin := math.Exp(z.Dot(beta))
	base := math.Pow(s/lambda, kappa-1) * (kappa / lambda) * lin
	return base * math.Exp(-math.Pow(s/lambda, kappa)*lin)
}

// ReentryTime implements the analytic inverse-CDF schedule of Equation
// (analytic_tre):
//
//	t_re = t_q + lambda * (-ln(1-U) * exp(-beta^T z))^{1/kappa}
//
// U must be drawn exactly once per quarantine episode.
func ReentryTime(elapsed float64, u, kappa, lambda float64, z Covariates, beta Beta) float64 {
	if u <= 0 {
		u = math.SmallestNonzeroFloat64
	}
	if u >= 1 {
		u = math.Nextafter(1, 0)
	}
	lin := math.Exp(z.Dot(beta))
	return lambda * math.Pow(-math.Log(1-u)/lin, 1/kappa)
}

// MeanDwell returns E[S_k] = lambda * Gamma(1 + 1/kappa) * exp(-beta^T z/kappa),
// the quantity Sbar_max of Lemma (quarantine_regret) evaluated at covariate z.
func MeanDwell(kappa, lambda float64, z Covariates, beta Beta) float64 {
	return lambda * math.Gamma(1+1/kappa) * math.Exp(-z.Dot(beta)/kappa)
}
