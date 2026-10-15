package gossip

import (
	"context"
	"log"
	"math/rand"
	"net"
	"sync"
	"time"

	"github.com/ahmadpanah/prigossip-hazard/internal/config"
)

// PeerRegistry resolves the set of candidate peers; it is fed by the MQTT
// informer (peer proxy endpoints) and can be seeded statically for tests.
type PeerRegistry interface {
	Peers() []string
}

// StaticRegistry is a plain list-backed registry.
type StaticRegistry struct {
	mu    sync.RWMutex
	peers []string
}

func (r *StaticRegistry) Set(peers []string) {
	r.mu.Lock()
	r.peers = append([]string(nil), peers...)
	r.mu.Unlock()
}

func (r *StaticRegistry) Add(peer string) {
	r.mu.Lock()
	for _, p := range r.peers {
		if p == peer {
			r.mu.Unlock()
			return
		}
	}
	r.peers = append(r.peers, peer)
	r.mu.Unlock()
}

// Peers returns a copy of the current peer list.
func (r *StaticRegistry) Peers() []string {
	r.mu.RLock()
	out := append([]string(nil), r.peers...)
	r.mu.RUnlock()
	return out
}

// Transport is the Gossip Transport Daemon: a non-blocking UDP socket pool
// running on port 9443 with a fixed H_gossip transmission period.
type Transport struct {
	spec     config.Spec
	src      Source
	sketcher *Sketcher
	belief   *Belief
	peers    PeerRegistry

	conn     *net.UDPConn
	selfAddr string
	rng      *rand.Rand
	onSketch func(peer int, dg Datagram)

	mu       sync.Mutex
	lastSent time.Time
	sent     uint64
	received uint64
}

// NewTransport prepares a daemon bound to the configured gossip port.
func NewTransport(spec config.Spec, src Source, sketcher *Sketcher, belief *Belief, peers PeerRegistry) *Transport {
	return &Transport{
		spec:     spec,
		src:      src,
		sketcher: sketcher,
		belief:   belief,
		peers:    peers,
		rng:      rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// OnSketch registers a hook invoked for every integrated peer datagram
// (used by metrics and the hazard covariate refresh).
func (t *Transport) OnSketch(fn func(peer int, dg Datagram)) {
	t.onSketch = fn
}

// SelfAddr returns the local address actually bound (host:port).
func (t *Transport) SelfAddr() string { return t.selfAddr }

// Stats reports counters for the agent's status endpoint.
func (t *Transport) Stats() (sent, received uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sent, t.received
}

// Run binds the socket and drives the send/receive loops until ctx is done.
func (t *Transport) Run(ctx context.Context) error {
	addr := &net.UDPAddr{IP: net.IPv4zero, Port: t.spec.GossipPort}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return err
	}
	t.conn = conn
	t.selfAddr = conn.LocalAddr().String()

	go t.recvLoop(ctx)

	// Align transmissions to H_gossip boundaries.
	interval := t.spec.GossipInterval()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Immediate round on startup so fresh proxies join the consensus quickly.
	t.sendRound()

	for {
		select {
		case <-ctx.Done():
			return conn.Close()
		case <-ticker.C:
			t.sendRound()
		}
	}
}

// sendRound picks one peer uniformly at random and transmits the perturbed
// sketch (Algorithm line: "Sample random peer j in K \ {k}").
func (t *Transport) sendRound() {
	peers := t.peers.Peers()
	if len(peers) == 0 {
		return
	}
	target := peers[t.rng.Intn(len(peers))]
	udpAddr, err := net.ResolveUDPAddr("udp", target)
	if err != nil {
		log.Printf("gossip: resolve %s: %v", target, err)
		return
	}
	vec, _, err := t.sketcher.Build()
	if err != nil {
		log.Printf("gossip: sketch: %v", err)
		return
	}
	dg := Datagram{Timestamp: uint64(time.Now().UnixNano()), Variances: vec}
	buf, err := dg.Encode()
	if err != nil {
		log.Printf("gossip: encode: %v", err)
		return
	}
	if _, err := t.conn.WriteToUDP(buf, udpAddr); err != nil {
		log.Printf("gossip: send to %s: %v", target, err)
		return
	}
	t.mu.Lock()
	t.sent++
	t.lastSent = time.Now()
	t.mu.Unlock()
}

func (t *Transport) recvLoop(ctx context.Context) {
	buf := make([]byte, 1500)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if err := t.conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond)); err != nil {
			return
		}
		n, from, err := t.conn.ReadFromUDP(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			select {
			case <-ctx.Done():
				return
			default:
				log.Printf("gossip: read: %v", err)
				return
			}
		}
		dg, err := Decode(buf[:n])
		if err != nil {
			log.Printf("gossip: decode from %s: %v", from, err)
			continue
		}
		peerID := peerKey(from.String())
		if t.belief.Observe(peerID, dg.Timestamp, dg.Variances) {
			t.mu.Lock()
			t.received++
			t.mu.Unlock()
			if t.onSketch != nil {
				t.onSketch(peerID, dg)
			}
		}
	}
}

// peerKey turns "10.0.0.5:9443" into a stable integer identity.
func peerKey(addr string) int {
	h := 0
	for i := 0; i < len(addr); i++ {
		h = 31*h + int(addr[i])
	}
	if h < 0 {
		h = -h
	}
	return h
}
