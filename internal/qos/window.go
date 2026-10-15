package qos

import (
	"math"
	"sync"
)

// Window is a bounded sliding window of observed end-to-end latencies for one
// service instance (H_{k,m}(t) in the paper). It is safe for concurrent use:
// the dispatch loop reads it while the telemetry goroutine appends.
type Window struct {
	mu   sync.RWMutex
	buf  []float64
	pos  int
	size int
	full bool
}

// NewWindow creates a window holding at most capacity samples (N_max).
func NewWindow(capacity int) *Window {
	if capacity < 2 {
		capacity = 2
	}
	return &Window{buf: make([]float64, capacity)}
}

// Append records a latency observation in milliseconds.
func (w *Window) Append(ms float64) {
	w.mu.Lock()
	w.buf[w.pos] = ms
	w.pos = (w.pos + 1) % len(w.buf)
	if w.pos == 0 {
		w.full = true
	}
	if w.full {
		w.size = len(w.buf)
	} else {
		w.size++
	}
	w.mu.Unlock()
}

// Len returns the number of samples currently held.
func (w *Window) Len() int {
	w.mu.RLock()
	n := w.size
	w.mu.RUnlock()
	return n
}

// Samples returns a copy of the window contents in arrival order.
func (w *Window) Samples() []float64 {
	w.mu.RLock()
	out := make([]float64, 0, w.size)
	if w.full {
		out = append(out, w.buf[w.pos:]...)
		out = append(out, w.buf[:w.pos]...)
	} else {
		out = append(out, w.buf[:w.size]...)
	}
	w.mu.RUnlock()
	return out
}

// Mean returns the sample mean.
func (w *Window) Mean() float64 {
	s := w.Samples()
	if len(s) == 0 {
		return 0
	}
	sum := 0.0
	for _, v := range s {
		sum += v
	}
	return sum / float64(len(s))
}

// Variance returns the unbiased sample variance v_{k,m} of Equation
// (sample_variance): 1/(n-1) * sum (l - lbar)^2. It returns 0 when n < 2.
func (w *Window) Variance() float64 {
	s := w.Samples()
	n := len(s)
	if n < 2 {
		return 0
	}
	mean := 0.0
	for _, v := range s {
		mean += v
	}
	mean /= float64(n)
	ss := 0.0
	for _, v := range s {
		d := v - mean
		ss += d * d
	}
	return ss / float64(n-1)
}

// StdDev returns the sample standard deviation (used by the Silverman-style
// KDE bandwidth rule b = 1.06 * sigmaHat * n^{-1/5}).
func (w *Window) StdDev() float64 {
	return math.Sqrt(w.Variance())
}

// Phi is the standard normal CDF evaluated with the error function.
func Phi(x float64) float64 {
	return 0.5 * (1 + math.Erf(x/math.Sqrt2))
}

// KDE computes the sliding-window Gaussian kernel density estimate of the SLA
// compliance probability, Equation (kde_estimator):
//
//	muHat = 1/|H| * sum_i Phi((tau - l_i) / b)
//
// bandwidth follows the normal-reference rule b = 1.06 * sigmaHat * n^{-1/5},
// which is the setting used by the QEdgeProxy baseline as well.
func (w *Window) KDE(tauMs float64) float64 {
	s := w.Samples()
	n := len(s)
	if n == 0 {
		return 1.0 // no evidence of violation: optimistically usable
	}
	b := bandwidth(s, n)
	if b <= 1e-9 {
		// Degenerate window: fall back to the hard indicator.
		ok := 0
		for _, v := range s {
			if v <= tauMs {
				ok++
			}
		}
		return float64(ok) / float64(n)
	}
	sum := 0.0
	for _, v := range s {
		sum += Phi((tauMs - v) / b)
	}
	return sum / float64(n)
}

func bandwidth(samples []float64, n int) float64 {
	mean := 0.0
	for _, v := range samples {
		mean += v
	}
	mean /= float64(n)
	ss := 0.0
	for _, v := range samples {
		d := v - mean
		ss += d * d
	}
	sigma := math.Sqrt(ss / float64(n))
	return 1.06 * sigma * math.Pow(float64(n), -0.2)
}
