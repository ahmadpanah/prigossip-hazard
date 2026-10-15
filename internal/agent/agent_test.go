package agent

import (
	"testing"
	"time"

	"github.com/ahmadpanah/prigossip-hazard/internal/config"
)

func testSpec() config.Spec {
	s := config.Default()
	s.Instances = 4
	s.HazardTickMs = 5
	s.Gossip.IntervalSec = 0.5
	if err := s.Validate(); err != nil {
		panic(err)
	}
	return s
}

func TestQuarantineTriggersAfterErrorThreshold(t *testing.T) {
	a := New(testSpec())
	spec := a.spec

	// Healthy traffic first: no quarantine.
	for i := 0; i < 50; i++ {
		a.Observe(0, 30)
	}
	if a.Hazard().Active(0) {
		t.Fatal("instance quarantined despite compliant traffic")
	}

	// E_t consecutive SLA violations.
	for i := 0; i < spec.Hazard.ErrorThreshold; i++ {
		a.Observe(0, 500)
	}
	if !a.Hazard().Active(0) {
		t.Fatalf("instance not quarantined after %d violations", spec.Hazard.ErrorThreshold)
	}

	// A success resets the counter, but quarantine persists until the
	// survival engine releases it.
	a.Observe(0, 20)
	if !a.Hazard().Active(0) {
		t.Fatal("quarantine should persist until the hazard engine releases it")
	}
}

func TestMaintenanceBuildsWeightsAndExcludesQuarantined(t *testing.T) {
	a := New(testSpec())
	for inst := 0; inst < a.Instances(); inst++ {
		lat := 20.0
		if inst == 3 {
			lat = 500.0
		}
		for i := 0; i < 20; i++ {
			a.Observe(inst, lat)
		}
	}
	// Force quarantine of instance 3.
	for i := 0; i < a.spec.Hazard.ErrorThreshold; i++ {
		a.Observe(3, 500)
	}

	a.Maintenance()
	w := a.Dispatcher().Weights()

	var sum float64
	for _, v := range w {
		sum += v
	}
	if sum < 0.999 || sum > 1.001 {
		t.Fatalf("weights sum to %v, want 1", sum)
	}
	if w[3] != 0 {
		t.Fatalf("quarantined instance has weight %v, want 0", w[3])
	}
	if a.Route() == 3 {
		t.Fatal("SWRR routed to a quarantined instance")
	}
}

func TestHazardReleasesInstanceAndRestoresRouting(t *testing.T) {
	spec := testSpec()
	// Short scale so the survival threshold is crossed quickly under test.
	spec.Hazard.ScaleParamSec = 0.05
	spec.HorizonSec = 5.0
	a := New(spec)

	for i := 0; i < spec.Hazard.ErrorThreshold; i++ {
		a.Observe(1, 400)
	}
	if !a.Hazard().Active(1) {
		t.Fatal("quarantine did not start")
	}

	// Tick until P_rec(s) >= U releases the instance (Equation
	// staggered_condition, evaluated against the persistent threshold).
	deadline := time.Now().Add(3 * time.Second)
	for a.Hazard().Active(1) && time.Now().Before(deadline) {
		for _, rel := range a.Hazard().Tick(time.Now()) {
			t.Logf("released instance %d after %s", rel.Record.InstanceID, rel.Dwell)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if a.Hazard().Active(1) {
		t.Fatal("instance was never re-admitted by the survival engine")
	}
	released, censored, _ := a.Hazard().Stats()
	if released != 1 || censored != 0 {
		t.Fatalf("released=%d censored=%d, want 1/0", released, censored)
	}

	a.Maintenance()
	w := a.Dispatcher().Weights()
	if w[1] == 0 {
		t.Fatalf("re-admitted instance received zero weight: %v", w)
	}
}

func TestCensoredEpisodeIsRecordedAtHorizon(t *testing.T) {
	spec := testSpec()
	// Recovery is essentially impossible within the horizon: the episode must
	// be right-censored at Delta_max (Equation censoring_indicator).
	spec.Hazard.ScaleParamSec = 10000
	spec.HorizonSec = 0.05
	a := New(spec)

	for i := 0; i < spec.Hazard.ErrorThreshold; i++ {
		a.Observe(2, 400)
	}
	if !a.Hazard().Active(2) {
		t.Fatal("quarantine did not start")
	}
	time.Sleep(80 * time.Millisecond)
	for _, rel := range a.Hazard().Tick(time.Now()) {
		t.Logf("censored episode for instance %d after %s", rel.Record.InstanceID, rel.Dwell)
	}
	released, censored, _ := a.Hazard().Stats()
	if censored != 1 || released != 0 {
		t.Fatalf("released=%d censored=%d, want 0/1", released, censored)
	}
	if a.Hazard().Active(2) {
		t.Fatal("censored instance should be released for probing")
	}
}

func TestCovariatesAreNormalized(t *testing.T) {
	a := New(testSpec())
	for i := 0; i < 30; i++ {
		a.Observe(0, 120)
	}
	a.Belief().Observe(1, 1, []float64{1000, 0, 0, 0})
	z := a.Covariates(0)
	if z.Omega < 0 || z.Omega > 1.5 {
		t.Fatalf("normalized Omega = %v, expected O(1)", z.Omega)
	}
	if z.NetRTT <= 0 || z.NetRTT > 2 {
		t.Fatalf("normalized RTT = %v, expected O(1)", z.NetRTT)
	}
}

func TestStatsSnapshot(t *testing.T) {
	a := New(testSpec())
	for i := 0; i < 10; i++ {
		a.Observe(0, 50)
	}
	s := a.Stats()
	if s.Observed != 10 {
		t.Fatalf("observed = %d, want 10", s.Observed)
	}
	if s.Kappa <= 0 || s.LambdaS <= 0 {
		t.Fatalf("Weibull parameters not initialized: %v %v", s.Kappa, s.LambdaS)
	}
}
