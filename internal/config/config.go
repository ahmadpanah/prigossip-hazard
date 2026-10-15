// Package config holds the declarative configuration of a PriGossip-Hazard
// agent. All values map 1:1 onto the QoSPolicy custom resource described in
// Section "Configuration and Declarative Operator Integration" of the paper.
package config

import (
	"fmt"
	"time"
)

// Defaults taken from Table "Architectural Provenance..." of the paper.
const (
	DefaultTauMs          = 80.0 // latency threshold tau
	DefaultRho            = 0.90 // success ratio rho
	DefaultWindowSec      = 10.0 // sliding window W
	DefaultEpsilon        = 1.0  // LDP privacy budget
	DefaultGossipInterval = 2.0  // H_gossip [s]
	DefaultAlphaG         = 0.35 // gossip learning rate
	DefaultTheta          = 3.5  // volatility sensitivity
	DefaultKappa          = 1.85 // Weibull shape
	DefaultLambdaS        = 6.5  // Weibull scale [s]
	DefaultErrorThreshold = 5    // E_t
	DefaultEta            = 0.01 // minimum score regularizer
	DefaultGossipPort     = 9443
	DefaultRingCapacity   = 1000 // N_max samples per instance
	DefaultHorizonSec     = 30.0 // Delta_max censoring horizon
	DefaultUpdateSec      = 1.0  // H_d Cox refit interval
	DefaultTickMs         = 500  // hazard manager evaluation tick
	DefaultCoxRingSize    = 200  // K_rec recovery tuples
	DefaultNewtonEta      = 0.05 // eta_beta damped Newton step
	DefaultMaxLatencyMs   = 160.0
)

// SLA mirrors spec.qosTargets of the QoSPolicy CRD.
type SLA struct {
	LatencyThresholdMs float64 `json:"latencyThresholdMs"`
	SuccessRatio       float64 `json:"successRatio"`
	SlidingWindowSec   float64 `json:"slidingWindowSec"`
}

// Gossip mirrors spec.gossipConfig of the QoSPolicy CRD.
type Gossip struct {
	PrivacyEpsilon float64 `json:"privacyEpsilon"`
	IntervalSec    float64 `json:"intervalSec"`
	LearningRate   float64 `json:"learningRate"`
}

// Hazard mirrors spec.hazardParams of the QoSPolicy CRD.
type Hazard struct {
	WeibullShape   float64 `json:"weibullShape"`
	ScaleParamSec  float64 `json:"scaleParamSec"`
	ErrorThreshold int     `json:"errorThreshold"`
	BetaOmega      float64 `json:"betaOmega"`
	BetaLatency    float64 `json:"betaLatency"`
}

// Dispatch mirrors spec.dispatch of the QoSPolicy CRD.
type Dispatch struct {
	VolatilityTheta float64 `json:"volatilityTheta"`
	MinRegularizer  float64 `json:"minRegularizer"`
	ExploreDecay    float64 `json:"exploreDecay"`
	ExploreReset    float64 `json:"exploreReset"`
}

// Spec is the fully resolved runtime specification of an agent.
type Spec struct {
	Service      string   `json:"service"`
	SLA          SLA      `json:"qosTargets"`
	Gossip       Gossip   `json:"gossipConfig"`
	Hazard       Hazard   `json:"hazardParams"`
	Dispatch     Dispatch `json:"dispatch"`
	Instances    int      `json:"instances"`    // M
	MaxLatencyMs float64  `json:"maxLatencyMs"` // tau_max, bounds the LDP sensitivity
	RingCapacity int      `json:"ringCapacity"` // N_max
	CoxRingSize  int      `json:"coxRingSize"`  // K_rec
	NewtonEta    float64  `json:"newtonEta"`    // eta_beta
	CoxUpdateSec float64  `json:"coxUpdateSec"` // H_d
	HazardTickMs int      `json:"hazardTickMs"` // evaluation tick
	HorizonSec   float64  `json:"horizonSec"`   // Delta_max
	GossipPort   int      `json:"gossipPort"`
	NodeName     string   `json:"-"`
	KubeConfig   string   `json:"-"`
	MQTTBroker   string   `json:"-"`
}

// Default returns the paper's published parameterization.
func Default() Spec {
	return Spec{
		Service: "vision-ai",
		SLA: SLA{
			LatencyThresholdMs: DefaultTauMs,
			SuccessRatio:       DefaultRho,
			SlidingWindowSec:   DefaultWindowSec,
		},
		Gossip: Gossip{
			PrivacyEpsilon: DefaultEpsilon,
			IntervalSec:    DefaultGossipInterval,
			LearningRate:   DefaultAlphaG,
		},
		Hazard: Hazard{
			WeibullShape:   DefaultKappa,
			ScaleParamSec:  DefaultLambdaS,
			ErrorThreshold: DefaultErrorThreshold,
			BetaOmega:      0.45, // Table: optimal beta_Omega
			BetaLatency:    0.02, // Table: optimal beta_l
		},
		Dispatch: Dispatch{
			VolatilityTheta: DefaultTheta,
			MinRegularizer:  DefaultEta,
			ExploreDecay:    0.98,
			ExploreReset:    1.0 - DefaultRho,
		},
		Instances:    10,
		MaxLatencyMs: DefaultMaxLatencyMs,
		RingCapacity: DefaultRingCapacity,
		CoxRingSize:  DefaultCoxRingSize,
		NewtonEta:    DefaultNewtonEta,
		CoxUpdateSec: DefaultUpdateSec,
		HazardTickMs: DefaultTickMs,
		HorizonSec:   DefaultHorizonSec,
		GossipPort:   DefaultGossipPort,
	}
}

// Validate fills unset fields with defaults and rejects impossible settings.
func (s *Spec) Validate() error {
	d := Default()
	if s.SLA.LatencyThresholdMs <= 0 {
		s.SLA.LatencyThresholdMs = d.SLA.LatencyThresholdMs
	}
	if s.SLA.SuccessRatio <= 0 || s.SLA.SuccessRatio >= 1 {
		s.SLA.SuccessRatio = d.SLA.SuccessRatio
	}
	if s.SLA.SlidingWindowSec <= 0 {
		s.SLA.SlidingWindowSec = d.SLA.SlidingWindowSec
	}
	if s.Gossip.PrivacyEpsilon <= 0 {
		s.Gossip.PrivacyEpsilon = d.Gossip.PrivacyEpsilon
	}
	if s.Gossip.IntervalSec <= 0 {
		s.Gossip.IntervalSec = d.Gossip.IntervalSec
	}
	if s.Gossip.LearningRate <= 0 || s.Gossip.LearningRate >= 1 {
		s.Gossip.LearningRate = d.Gossip.LearningRate
	}
	if s.Hazard.WeibullShape <= 0 {
		s.Hazard.WeibullShape = d.Hazard.WeibullShape
	}
	if s.Hazard.ScaleParamSec <= 0 {
		s.Hazard.ScaleParamSec = d.Hazard.ScaleParamSec
	}
	if s.Hazard.ErrorThreshold < 1 {
		s.Hazard.ErrorThreshold = d.Hazard.ErrorThreshold
	}
	if s.Dispatch.VolatilityTheta <= 0 {
		s.Dispatch.VolatilityTheta = d.Dispatch.VolatilityTheta
	}
	if s.Dispatch.MinRegularizer <= 0 {
		s.Dispatch.MinRegularizer = d.Dispatch.MinRegularizer
	}
	if s.Instances < 1 {
		s.Instances = d.Instances
	}
	if s.MaxLatencyMs <= 0 {
		s.MaxLatencyMs = d.MaxLatencyMs
	}
	if s.RingCapacity < 2 {
		s.RingCapacity = d.RingCapacity
	}
	if s.CoxRingSize < 1 {
		s.CoxRingSize = d.CoxRingSize
	}
	if s.NewtonEta <= 0 {
		s.NewtonEta = d.NewtonEta
	}
	if s.CoxUpdateSec <= 0 {
		s.CoxUpdateSec = d.CoxUpdateSec
	}
	if s.HazardTickMs <= 0 {
		s.HazardTickMs = d.HazardTickMs
	}
	if s.HorizonSec <= 0 {
		s.HorizonSec = d.HorizonSec
	}
	if s.GossipPort == 0 {
		s.GossipPort = d.GossipPort
	}
	if s.Gossip.IntervalSec < float64(s.HazardTickMs)/1000.0 {
		return fmt.Errorf("gossip interval %.3fs shorter than hazard tick %dms",
			s.Gossip.IntervalSec, s.HazardTickMs)
	}
	return nil
}

// Window is the sliding observation window W as a duration.
func (s Spec) Window() time.Duration {
	return time.Duration(s.SLA.SlidingWindowSec * float64(time.Second))
}

// GossipInterval is H_gossip as a duration.
func (s Spec) GossipInterval() time.Duration {
	return time.Duration(s.Gossip.IntervalSec * float64(time.Second))
}

// HazardTick is the hazard manager evaluation period.
func (s Spec) HazardTick() time.Duration {
	return time.Duration(s.HazardTickMs) * time.Millisecond
}

// CoxUpdate is H_d, the parameter refit period.
func (s Spec) CoxUpdate() time.Duration {
	return time.Duration(s.CoxUpdateSec * float64(time.Second))
}

// Horizon is Delta_max, the right-censoring horizon.
func (s Spec) Horizon() time.Duration {
	return time.Duration(s.HorizonSec * float64(time.Second))
}
