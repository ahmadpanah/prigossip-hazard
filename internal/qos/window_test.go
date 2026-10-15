package qos

import (
	"testing"
)

func TestWindowRingOrder(t *testing.T) {
	w := NewWindow(4)
	for _, v := range []float64{1, 2, 3, 4, 5} {
		w.Append(v)
	}
	got := w.Samples()
	want := []float64{2, 3, 4, 5}
	if len(got) != len(want) {
		t.Fatalf("len=%d want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sample[%d]=%v want %v", i, got[i], want[i])
		}
	}
}

func TestVarianceMatchesTextbook(t *testing.T) {
	w := NewWindow(8)
	for _, v := range []float64{2, 4, 4, 4, 5, 5, 7, 9} {
		w.Append(v)
	}
	// population variance of this classic set is 4, unbiased is 32/7.
	if got := w.Variance(); got < 4.571 || got > 4.572 {
		t.Fatalf("variance=%v want ~4.5714", got)
	}
}

func TestKDemonotone(t *testing.T) {
	good := NewWindow(8)
	for i := 0; i < 8; i++ {
		good.Append(40)
	}
	bad := NewWindow(8)
	for i := 0; i < 8; i++ {
		bad.Append(150)
	}
	gm, bm := good.KDE(80), bad.KDE(80)
	if gm <= 0.99 {
		t.Fatalf("compliant window KDE=%v, want ~1", gm)
	}
	if bm >= 0.01 {
		t.Fatalf("violating window KDE=%v, want ~0", bm)
	}
}

func TestKDEDegenerateWindowIsIndicator(t *testing.T) {
	w := NewWindow(4)
	w.Append(50)
	w.Append(50)
	if got := w.KDE(80); got != 1 {
		t.Fatalf("KDE=%v want 1", got)
	}
	w2 := NewWindow(4)
	w2.Append(120)
	w2.Append(120)
	if got := w2.KDE(80); got != 0 {
		t.Fatalf("KDE=%v want 0", got)
	}
}
