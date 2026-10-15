// Package discovery implements the MQTT Informer: a client-go cache based
// observer of Pod, Node and Endpoint mutations that serializes endpoint
// descriptors to JSON and publishes them over the MQTT topic tree.
package discovery

import (
	"encoding/json"
	"sort"
	"sync"
)

// Backend is one routable service replica.
type Backend struct {
	Name string `json:"name"`
	IP   string `json:"ip"`
	Port int    `json:"port"`
	Node string `json:"node,omitempty"`
}

// EndpointSet is the JSON payload published on the service topic.
type EndpointSet struct {
	Service string    `json:"service"`
	Nodes   []Backend `json:"endpoints"`
}

// Registry holds the current backend and peer-proxy views of this agent.
type Registry struct {
	mu       sync.RWMutex
	service  string
	backends map[string]Backend
	peers    map[string]string // addr -> pod name
}

// NewRegistry creates an empty registry for one managed service.
func NewRegistry(service string) *Registry {
	return &Registry{
		service:  service,
		backends: make(map[string]Backend),
		peers:    make(map[string]string),
	}
}

// Service returns the managed service name.
func (r *Registry) Service() string { return r.service }

// SetBackends replaces the backend view atomically.
func (r *Registry) SetBackends(list []Backend) {
	r.mu.Lock()
	next := make(map[string]Backend, len(list))
	for _, b := range list {
		next[b.Name+"/"+b.IP] = b
	}
	r.backends = next
	r.mu.Unlock()
}

// Apply merges a published EndpointSet (received over MQTT).
func (r *Registry) Apply(set EndpointSet) {
	if set.Service != r.service {
		return
	}
	r.SetBackends(set.Nodes)
}

// Backends returns the current backend list sorted by name.
func (r *Registry) Backends() []Backend {
	r.mu.RLock()
	out := make([]Backend, 0, len(r.backends))
	for _, b := range r.backends {
		out = append(out, b)
	}
	r.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// AddPeer registers a fellow proxy (gossip target).
func (r *Registry) AddPeer(addr, name string) {
	r.mu.Lock()
	r.peers[addr] = name
	r.mu.Unlock()
}

// RemovePeer drops a departed proxy.
func (r *Registry) RemovePeer(addr string) {
	r.mu.Lock()
	delete(r.peers, addr)
	r.mu.Unlock()
}

// Peers returns the gossip addresses of all known proxies.
func (r *Registry) Peers() []string {
	r.mu.RLock()
	out := make([]string, 0, len(r.peers))
	for addr := range r.peers {
		out = append(out, addr)
	}
	r.mu.RUnlock()
	sort.Strings(out)
	return out
}

// Marshal encodes the registry view as the binary JSON payload of the
// informer.
func (r *Registry) Marshal() ([]byte, error) {
	return json.Marshal(EndpointSet{Service: r.service, Nodes: r.Backends()})
}
