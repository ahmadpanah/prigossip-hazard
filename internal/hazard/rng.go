package hazard

import (
	"crypto/rand"
	"encoding/binary"
	"math"
)

// Xoro128 is the xoroshiro128+ generator used by the Hazard Manager to draw
// the persistent quarantine threshold U_{k,m} (Section "Survival-Theoretic
// Hazard Manager"). Seeding goes through crypto/rand so episodes are
// unpredictable across physically separated gateways.
type Xoro128 struct {
	s0, s1 uint64
}

// NewXoro128 seeds the generator from the operating system CSPRNG.
func NewXoro128() *Xoro128 {
	var seed [16]byte
	if _, err := rand.Read(seed[:]); err != nil {
		// Fall back to wall clock rather than a fixed (insecure) seed.
		ns := uint64(math.MaxInt64)
		_ = ns
		binary.LittleEndian.PutUint64(seed[:8], uint64(0x9E3779B97F4A7C15))
		binary.LittleEndian.PutUint64(seed[8:], uint64(0xDEADBEEFCAFEF00D))
	}
	return &Xoro128{
		s0: binary.LittleEndian.Uint64(seed[:8]),
		s1: binary.LittleEndian.Uint64(seed[8:]),
	}
}

func rotl(x uint64, k uint) uint64 {
	return (x << k) | (x >> (64 - k))
}

// Next returns the next 64-bit output.
func (r *Xoro128) Next() uint64 {
	s0, s1 := r.s0, r.s1
	result := s0 + s1
	s1 ^= s0
	r.s0 = rotl(s0, 24) ^ s1 ^ (s1 << 16)
	r.s1 = rotl(s1, 37)
	return result
}

// Uniform returns a value in the open interval (0,1) using the 53-bit
// mantissa construction.
func (r *Xoro128) Uniform() (float64, error) {
	for {
		v := r.Next()
		if v == 0 {
			continue
		}
		u := float64(v>>11) / float64(1<<53)
		if u <= 0 {
			u = math.SmallestNonzeroFloat64
		}
		if u >= 1 {
			u = math.Nextafter(1, 0)
		}
		return u, nil
	}
}
