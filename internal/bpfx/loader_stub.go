//go:build !linux

// Package bpfx is unavailable outside Linux: the agent degrades to
// user-space latency measurement (which is what the test harness uses on
// development machines).
package bpfx

import (
	"context"
	"fmt"
)

// Event mirrors the kernel-side structure (unused on this platform).
type Event struct {
	LatencyNs uint64
	Instance  uint32
	Bytes     uint32
	StartNs   uint64
	AckNs     uint64
}

// Loader is a no-op stand-in.
type Loader struct{}

// Available always reports false off Linux.
func Available(dir string) bool { return false }

// Load always fails off Linux.
func Load(dir string, onEvent func(instance int, latencyMs float64)) (*Loader, error) {
	return nil, fmt.Errorf("eBPF telemetry requires Linux")
}

// LoadSockOps always fails off Linux.
func (l *Loader) LoadSockOps(cgroupPath string, objDir string) error {
	return fmt.Errorf("eBPF data plane requires Linux")
}

// AttachedPrograms returns nothing off Linux.
func (l *Loader) AttachedPrograms() []string { return nil }

// ContextCancel is a no-op off Linux.
func (l *Loader) ContextCancel(ctx context.Context) {}

// Close is a no-op off Linux.
func (l *Loader) Close() {}
