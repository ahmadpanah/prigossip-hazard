//go:build linux

// Package bpfx loads the prebuilt eBPF objects produced by bpf/Makefile and
// wires the kernel latency ring buffer into the user-space telemetry
// pipeline. Loading is best-effort: without the objects (or without the
// required kernel features) the agent falls back to user-space measurement.
package bpfx

import (
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	"github.com/cilium/ebpf/rlimit"
)

// Event mirrors struct latency_event of bpf/common.h (32 bytes).
type Event struct {
	LatencyNs uint64
	Instance  uint32
	Bytes     uint32
	StartNs   uint64
	AckNs     uint64
}

// Loader owns the attached programs and the ring-buffer reader.
type Loader struct {
	coll  *ebpf.Collection
	rd    *ringbuf.Reader
	links []link.Link
	extra []*ebpf.Collection
}

// Available reports whether compiled objects exist in dir.
func Available(dir string) bool {
	if dir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, "kprobe_latency.o"))
	return err == nil
}

// Load attaches the latency kprobes and starts draining the ring buffer into
// onEvent(instance, latencyMs). The returned loader must be closed.
func Load(dir string, onEvent func(instance int, latencyMs float64)) (*Loader, error) {
	if err := rlimit.RemoveMemlock(); err != nil {
		log.Printf("bpfx: remove memlock: %v (continuing)", err)
	}
	coll, err := ebpf.LoadCollection(filepath.Join(dir, "kprobe_latency.o"))
	if err != nil {
		return nil, fmt.Errorf("load kprobe_latency.o: %w", err)
	}
	rd, err := ringbuf.NewReader(coll.Maps["latency_ring"])
	if err != nil {
		coll.Close()
		return nil, fmt.Errorf("open latency_ring: %w", err)
	}

	l := &Loader{coll: coll, rd: rd}
	for prog, event := range map[string]string{
		"prigossip_tcp_sendmsg":      "tcp_sendmsg",
		"prigossip_tcp_cleanup_rbuf": "tcp_cleanup_rbuf",
	} {
		p := coll.Programs[prog]
		if p == nil {
			continue
		}
		ln, err := link.Kprobe(event, p, nil)
		if err != nil {
			l.Close()
			return nil, fmt.Errorf("attach kprobe %s: %w", event, err)
		}
		l.links = append(l.links, ln)
	}
	go l.drain(onEvent)
	log.Printf("bpfx: latency kprobes attached from %s", dir)
	return l, nil
}

// LoadSockOps attaches the ingress intercept (sock_ops) and the sk_msg fast
// path to a cgroup v2 hierarchy. Returns an error when the kernel refuses the
// attachment; callers treat that as a graceful degradation.
func (l *Loader) LoadSockOps(cgroupPath string, objDir string) error {
	objs, err := ebpf.LoadCollection(filepath.Join(objDir, "sock_ops.o"))
	if err != nil {
		return fmt.Errorf("load sock_ops.o: %w", err)
	}
	l.coll = l.coll // primary collection owns the kprobe programs
	ln, err := link.AttachCgroup(link.CgroupOptions{
		Path:    cgroupPath,
		Attach:  ebpf.AttachCGroupSockOps,
		Program: objs.Programs["prigossip_sock_ops"],
	})
	if err != nil {
		objs.Close()
		return fmt.Errorf("attach sock_ops to %s: %w", cgroupPath, err)
	}
	l.links = append(l.links, ln)
	l.extra = append(l.extra, objs)

	msgObjs, err := ebpf.LoadCollection(filepath.Join(objDir, "sk_msg.o"))
	if err == nil {
		if ln2, err2 := link.AttachCgroup(link.CgroupOptions{
			Path:    cgroupPath,
			Attach:  ebpf.AttachCGroupSkMsg,
			Program: msgObjs.Programs["prigossip_sk_msg"],
		}); err2 == nil {
			l.links = append(l.links, ln2)
			l.extra = append(l.extra, msgObjs)
		} else {
			log.Printf("bpfx: sk_msg attach: %v (continuing without fast path)", err2)
		}
	}
	return nil
}

func (l *Loader) drain(onEvent func(int, float64)) {
	for {
		record, err := l.rd.Read()
		if err != nil {
			if err == ringbuf.ErrClosed {
				return
			}
			log.Printf("bpfx: ringbuf read: %v", err)
			return
		}
		if len(record.RawSample) < 32 {
			continue
		}
		ev := decode(record.RawSample)
		if onEvent != nil {
			onEvent(int(ev.Instance), float64(ev.LatencyNs)/1e6)
		}
	}
}

func decode(b []byte) Event {
	return Event{
		LatencyNs: binary.LittleEndian.Uint64(b[0:8]),
		Instance:  binary.LittleEndian.Uint32(b[8:12]),
		Bytes:     binary.LittleEndian.Uint32(b[12:16]),
		StartNs:   binary.LittleEndian.Uint64(b[16:24]),
		AckNs:     binary.LittleEndian.Uint64(b[24:32]),
	}
}

// Close detaches every program and releases the maps.
func (l *Loader) Close() {
	if l == nil {
		return
	}
	if l.rd != nil {
		_ = l.rd.Close()
	}
	for _, ln := range l.links {
		_ = ln.Close()
	}
	for _, c := range l.extra {
		_ = c.Close()
	}
	if l.coll != nil {
		_ = l.coll.Close()
	}
}

// AttachedPrograms lists the names of the loaded programs (diagnostics).
func (l *Loader) AttachedPrograms() []string {
	var out []string
	if l == nil || l.coll == nil {
		return out
	}
	for name := range l.coll.Programs {
		out = append(out, name)
	}
	return out
}

// ContextCancel is a convenience for callers that want to close on context
// cancellation.
func (l *Loader) ContextCancel(ctx context.Context) {
	go func() {
		<-ctx.Done()
		time.Sleep(50 * time.Millisecond)
		l.Close()
	}()
}
