package gossip

import (
	"math"
	"testing"

	"github.com/ahmadpanah/prigossip-hazard/internal/config"
	"github.com/ahmadpanah/prigossip-hazard/internal/qos"
)

// fakeSource exposes fixed windows per instance.
type fakeSource struct {
	w []*qos.Window
}

func (f *fakeSource) Window(i int) *qos.Window {
	if i < 0 || i >= len(f.w) {
		return nil
	}
	return f.w[i]
}
func (f *fakeSource) Instances() int { return len(f.w) }

func newSource(vals ...[]float64) *fakeSource {
	s := &fakeSource{}
	for _, v := range vals {
		w := qos.NewWindow(1024)
		for _, x := range v {
			w.Append(x)
		}
		s.w = append(s.w, w)
	}
	return s
}

// deterministicRNG returns a fixed sequence of uniforms.
type deterministicRNG struct {
	seq []float64
	i   int
}

func (r *deterministicRNG) Uniform() (float64, error) {
	v := r.seq[r.i%len(r.seq)]
	r.i++
	return v, nil
}

func TestSensitivityMatchesLemma(t *testing.T) {
	spec := config.Default()
	spec.MaxLatencyMs = 160
	src := newSource(
		[]float64{10, 20, 30, 40, 50},         // n = 5
		[]float64{10, 20, 30, 40, 50, 60, 70}, // n = 7
	)
	s := NewSketcher(spec, src, nil)
	got := s.Sensitivity()
	want := spec.MaxLatencyMs * spec.MaxLatencyMs / 5 // n_min = 5
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("sensitivity = %v, want tau_max^2/n_min = %v", got, want)
	}
}

func TestSensitivityZeroWithoutTwoSamples(t *testing.T) {
	spec := config.Default()
	src := newSource([]float64{10}, nil)
	s := NewSketcher(spec, src, nil)
	if got := s.Sensitivity(); got != 0 {
		t.Fatalf("sensitivity = %v, want 0 when no instance has n >= 2", got)
	}
}

func TestLaplaceScaleIsSensitivityOverEpsilon(t *testing.T) {
	spec := config.Default()
	spec.Gossip.PrivacyEpsilon = 2.0
	src := newSource([]float64{1, 2, 3, 4, 5, 6})
	s := NewSketcher(spec, src, nil)
	want := s.Sensitivity() / 2.0
	if math.Abs(s.LaplaceScale()-want) > 1e-9 {
		t.Fatalf("scale = %v want %v", s.LaplaceScale(), want)
	}
}

func TestLaplaceSampleHasExpectedScale(t *testing.T) {
	// Inverse-CDF sampler: empirical standard deviation should be sqrt(2)*b.
	b := 3.0
	rng := &XoroSeq{}
	var sum, sumsq float64
	const n = 200000
	for i := 0; i < n; i++ {
		u, err := rng.Uniform()
		if err != nil {
			t.Fatal(err)
		}
		x := SampleLaplace(u, b)
		sum += x
		sumsq += x * x
	}
	mean := sum / n
	vari := sumsq/n - mean*mean
	sd := math.Sqrt(vari)
	if math.Abs(sd-math.Sqrt(2)*b) > 0.1*b {
		t.Fatalf("laplace sd = %v, want ~%v", sd, math.Sqrt(2)*b)
	}
	if math.Abs(mean) > 0.1 {
		t.Fatalf("laplace mean = %v, want ~0", mean)
	}
}

// XoroSeq is a tiny deterministic uniform source for statistical tests.
type XoroSeq struct{ n uint64 }

func (x *XoroSeq) Uniform() (float64, error) {
	x.n += 0x9E3779B97F4A7C15
	v := x.n
	v ^= v >> 30
	v *= 0xBF58476D1CE4E5B9
	v ^= v >> 27
	return float64(v>>11)/float64(1<<53) + 1e-12, nil
}

func TestSketchProjectsIntoFeasibleRange(t *testing.T) {
	spec := config.Default()
	src := newSource([]float64{0, 160, 0, 160, 80})
	s := NewSketcher(spec, src, &deterministicRNG{seq: []float64{0.5, 0.5}})
	vec, scale, err := s.Build()
	if err != nil {
		t.Fatal(err)
	}
	if len(vec) != 1 {
		t.Fatalf("expected one coordinate, got %d", len(vec))
	}
	vmax := s.VMax()
	if vec[0] < 0 || vec[0] > vmax {
		t.Fatalf("projected variance %v outside [0, %v]", vec[0], vmax)
	}
	if scale <= 0 {
		t.Fatalf("noise scale must be positive, got %v", scale)
	}
	// u = 0.5 is the median of the Laplace distribution => zero noise.
	if math.Abs(vec[0]-src.w[0].Variance()) > 1e-9 {
		t.Fatalf("median draw should leave the variance unchanged: %v vs %v",
			vec[0], src.w[0].Variance())
	}
}

func TestVMaxIsTauSquaredOverFour(t *testing.T) {
	spec := config.Default()
	spec.MaxLatencyMs = 80
	s := NewSketcher(spec, newSource([]float64{1, 2}), nil)
	if got, want := s.VMax(), 80.0*80.0/4.0; got != want {
		t.Fatalf("VMax = %v want %v", got, want)
	}
}

func TestDatagramRoundTrip(t *testing.T) {
	in := Datagram{Timestamp: 123456789, Variances: []float64{1.5, 0, 3.25, 99.75}}
	buf, err := in.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if len(buf) != 12+8*4 {
		t.Fatalf("encoded size = %d, want %d", len(buf), 12+8*4)
	}
	out, err := Decode(buf)
	if err != nil {
		t.Fatal(err)
	}
	if out.Timestamp != in.Timestamp {
		t.Fatalf("timestamp mismatch: %v vs %v", out.Timestamp, in.Timestamp)
	}
	for i := range in.Variances {
		if math.Abs(out.Variances[i]-in.Variances[i]) > 1e-5 {
			t.Fatalf("variance[%d] = %v want %v", i, out.Variances[i], in.Variances[i])
		}
	}
}

func TestDecodeRejectsGarbage(t *testing.T) {
	if _, err := Decode([]byte{1, 2, 3}); err == nil {
		t.Fatal("short datagram must be rejected")
	}
	good, _ := Datagram{Timestamp: 1, Variances: []float64{1}}.Encode()
	good[0] = 0xFF
	if _, err := Decode(good); err == nil {
		t.Fatal("bad magic must be rejected")
	}
}

func TestBeliefConsensusAndDedup(t *testing.T) {
	b := NewBelief(3, 0.35)
	if !b.Observe(7, 100, []float64{10, 0, 5}) {
		t.Fatal("first observation must be integrated")
	}
	if b.Observe(7, 100, []float64{10, 0, 5}) {
		t.Fatal("replayed datagram must be ignored")
	}
	if !b.Observe(7, 101, []float64{10, 0, 5}) {
		t.Fatal("newer timestamp must be integrated")
	}
	want := (1-0.35)*(1-0.35)*0 + 0.35*10*(1-0.35) + 0.35*10
	got := b.At(0)
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("omega = %v want %v", got, want)
	}
	if b.At(1) != 0 {
		t.Fatalf("omega[1] = %v want 0", b.At(1))
	}
}
