// Command agent is the PriGossip-Hazard DaemonSet entrypoint: one instance
// per node of the Computing Continuum.
//
// It runs five cooperating loops (paper, Section "Concurrent Runtime Daemon
// Architecture"):
//
//  1. MQTT Informer subscription   -> peer/endpoint discovery
//  2. L7 forwarding engine         -> SWRR dispatch + latency feedback
//  3. Gossip transport daemon      -> UDP/9443 epsilon-LDP sketches
//  4. Hazard manager               -> 500 ms quarantine evaluation + Cox refit
//  5. Maintenance epoch            -> KDE estimation, pool rebuild, epsilon decay
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/ahmadpanah/prigossip-hazard/internal/agent"
	"github.com/ahmadpanah/prigossip-hazard/internal/bpfx"
	"github.com/ahmadpanah/prigossip-hazard/internal/config"
	"github.com/ahmadpanah/prigossip-hazard/internal/discovery"
	"github.com/ahmadpanah/prigossip-hazard/internal/gossip"
	"github.com/ahmadpanah/prigossip-hazard/internal/mqttx"
	"github.com/ahmadpanah/prigossip-hazard/internal/proxy"
)

func main() {
	var (
		policyPath     = flag.String("policy", envOr("QOS_POLICY", ""), "path to the QoSPolicy JSON manifest")
		listenAddr     = flag.String("listen", envOr("LISTEN_ADDR", ":8080"), "L7 forwarding listen address")
		adminAddr      = flag.String("admin", envOr("ADMIN_ADDR", ":9090"), "admin/metrics listen address")
		broker         = flag.String("mqtt", envOr("MQTT_BROKER", "tcp://mosquitto:1883"), "MQTT broker URL")
		kubeconfig     = flag.String("kubeconfig", envOr("KUBECONFIG", ""), "kubeconfig path (out of cluster)")
		namespace      = flag.String("namespace", envOr("POD_NAMESPACE", "default"), "Kubernetes namespace")
		selector       = flag.String("selector", envOr("QOS_SELECTOR", ""), "label selector of the managed workload")
		backendPort    = flag.Int("backend-port", envOrInt("BACKEND_PORT", 8000), "container port of the managed service")
		service        = flag.String("service", envOr("QOS_SERVICE", "vision-ai"), "managed service name")
		bpfDir         = flag.String("bpf-dir", envOr("BPF_OBJ_DIR", "/var/lib/prigossip-hazard/bpf"), "directory holding the compiled eBPF objects")
		cgroupRoot     = flag.String("cgroup", envOr("CGROUP_ROOT", "/sys/fs/cgroup"), "cgroup v2 root for sock_ops/sk_msg attachment")
		staticBackends = flag.String("backends", envOr("STATIC_BACKENDS", ""), "comma-separated static backend addresses (host:port), used without Kubernetes")
		gossipPort     = flag.Int("gossip-port", envOrInt("GOSSIP_PORT", config.DefaultGossipPort), "UDP port of the gossip transport")
	)
	flag.Parse()

	spec, err := loadPolicy(*policyPath)
	if err != nil {
		log.Fatalf("agent: %v", err)
	}
	spec.NodeName = envOr("POD_NAME", hostname())
	spec.GossipPort = *gossipPort
	spec.KubeConfig = *kubeconfig
	spec.MQTTBroker = *broker
	spec.Service = *service

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	a := agent.New(spec)
	log.Printf("agent: %s starting (M=%d tau=%.0fms rho=%.2f eps=%.2f kappa=%.2f lambda=%.2fs Et=%d)",
		spec.NodeName, spec.Instances, spec.SLA.LatencyThresholdMs, spec.SLA.SuccessRatio,
		spec.Gossip.PrivacyEpsilon, spec.Hazard.WeibullShape, spec.Hazard.ScaleParamSec,
		spec.Hazard.ErrorThreshold)

	// --- discovery -------------------------------------------------------
	reg := discovery.NewRegistry(spec.Service)
	bus := mqttx.New(*broker, "prigossip-"+spec.NodeName)
	if err := bus.Connect(); err != nil {
		log.Printf("agent: MQTT unavailable at startup, will retry in background: %v", err)
	}

	selfAddr := gossipAddress(spec)
	// Subscriptions are registered unconditionally: mqttx queues them while the
	// broker is unreachable and installs them on the first successful connect,
	// so a late-appearing broker never strands the MQTT informer.
	{
		// Learn peer proxies from the agents topic (retained announcements).
		if err := bus.Subscribe(mqttx.TopicRoot+"/agents/"+spec.Service+"/+", func(payload []byte) {
			var ann struct {
				Name string `json:"name"`
				Addr string `json:"addr"`
			}
			if err := json.Unmarshal(payload, &ann); err != nil || ann.Addr == "" || ann.Addr == selfAddr {
				return
			}
			reg.AddPeer(ann.Addr, ann.Name)
		}); err != nil {
			log.Printf("agent: subscribe agents topic: %v", err)
		}
		// Consume our own service endpoint publications (self-healing view).
		if err := bus.Subscribe(mqttx.ServiceTopic(spec.Service), func(payload []byte) {
			var set discovery.EndpointSet
			if err := json.Unmarshal(payload, &set); err == nil {
				reg.Apply(set)
			}
		}); err != nil {
			log.Printf("agent: subscribe service topic: %v", err)
		}
	}

	// Static backend seeding for local testbeds (no Kubernetes API needed).
	if *staticBackends != "" {
		var backends []discovery.Backend
		for i, addr := range splitCSV(*staticBackends) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				log.Printf("agent: bad backend address %q: %v", addr, err)
				continue
			}
			p, _ := strconv.Atoi(port)
			backends = append(backends, discovery.Backend{
				Name: fmt.Sprintf("static-%d", i), IP: host, Port: p,
			})
		}
		if len(backends) > 0 {
			reg.SetBackends(backends)
			log.Printf("agent: %d static backends registered", len(backends))
		}
	}

	// Seed peers with a static list so a single-node test cluster gossips.
	if peers := os.Getenv("GOSSIP_PEERS"); peers != "" {
		for _, p := range splitCSV(peers) {
			if p != selfAddr {
				reg.AddPeer(p, p)
			}
		}
	}

	// --- informer (best effort: the agent still routes without the API) ---
	if cfg, err := restConfig(*kubeconfig); err == nil {
		if client, cerr := kubernetes.NewForConfig(cfg); cerr == nil {
			sel := *selector
			if sel == "" {
				sel = "app=" + spec.Service
			}
			inf, ierr := discovery.NewInformer(client, discovery.Options{
				Namespace:     *namespace,
				LabelSelector: sel,
				Bus:           bus,
				Registry:      reg,
				SelfAddr:      selfAddr,
				SelfName:      spec.NodeName,
				BackendPort:   *backendPort,
			})
			if ierr != nil {
				log.Printf("agent: informer setup failed: %v", ierr)
			} else {
				go func() {
					if err := inf.Run(ctx); err != nil {
						log.Printf("agent: informer stopped: %v", err)
					}
				}()
			}
		}
	} else {
		log.Printf("agent: no Kubernetes API available (%v); running with static configuration", err)
	}

	// --- gossip transport -------------------------------------------------
	sketcher := a.Sketcher()
	tr := gossip.NewTransport(spec, a, sketcher, a.Belief(), reg)
	go func() {
		if err := tr.Run(ctx); err != nil && ctx.Err() == nil {
			log.Printf("agent: gossip transport: %v", err)
		}
	}()

	// --- hazard manager + maintenance loops -------------------------------
	go a.Hazard().Run(ctx)
	go a.Run(ctx)

	// --- kernel data plane (best effort) ----------------------------------
	if bpfx.Available(*bpfDir) {
		if l, err := bpfx.Load(*bpfDir, func(inst int, ms float64) {
			a.Observe(inst, ms)
		}); err == nil {
			a.SetKernelTelemetry(true)
			l.ContextCancel(ctx)
			if err := l.LoadSockOps(*cgroupRoot, *bpfDir); err != nil {
				log.Printf("agent: eBPF intercept unavailable, using user-space L7 proxy only: %v", err)
			}
			log.Printf("agent: kernel telemetry active (programs: %v)", l.AttachedPrograms())
		} else {
			log.Printf("agent: eBPF telemetry unavailable: %v", err)
		}
	} else {
		log.Printf("agent: no eBPF objects in %s; running user-space measurement", *bpfDir)
	}

	// --- L7 forwarding plane ---------------------------------------------
	ph := proxy.New(spec, a, reg)
	proxySrv := &http.Server{Addr: *listenAddr, Handler: ph, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		log.Printf("agent: forwarding engine listening on %s", *listenAddr)
		if err := proxySrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("agent: forwarding engine: %v", err)
			cancel()
		}
	}()

	// --- admin surface ----------------------------------------------------
	admin := http.NewServeMux()
	admin.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	admin.HandleFunc("/status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"node":     spec.NodeName,
			"stats":    a.Stats(),
			"peers":    reg.Peers(),
			"backends": reg.Backends(),
			"weights":  a.Dispatcher().Weights(),
			"explore":  a.Dispatcher().Explore(),
			"omega":    a.Belief().Values(),
			"mqtt":     bus.Connected(),
			"self":     selfAddr,
		})
	})
	admin.HandleFunc("/gossip/peers", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(reg.Peers())
	})
	adminSrv := &http.Server{Addr: *adminAddr, Handler: admin, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := adminSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("agent: admin server: %v", err)
		}
	}()

	<-ctx.Done()
	log.Printf("agent: shutting down")
	shutdownCtx, c2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer c2()
	_ = proxySrv.Shutdown(shutdownCtx)
	_ = adminSrv.Shutdown(shutdownCtx)
	bus.Close()
}

// loadPolicy reads the QoSPolicy manifest (JSON or the JSON projection of the
// CRD) and falls back to the paper's published defaults.
func loadPolicy(path string) (config.Spec, error) {
	spec := config.Default()
	if path == "" {
		return spec, spec.Validate()
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return spec, fmt.Errorf("read policy: %w", err)
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		return spec, fmt.Errorf("parse policy %s: %w", path, err)
	}
	return spec, spec.Validate()
}

func restConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	}
	if home, err := os.UserHomeDir(); err == nil {
		return clientcmd.BuildConfigFromFlags("", home+"/.kube/config")
	}
	return nil, fmt.Errorf("no in-cluster or kubeconfig configuration")
}

func gossipAddress(spec config.Spec) string {
	host := envOr("POD_IP", envOr("HOST_IP", "127.0.0.1"))
	return fmt.Sprintf("%s:%d", host, spec.GossipPort)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envOrInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func hostname() string {
	if h, err := os.Hostname(); err == nil {
		return h
	}
	return "agent"
}

func splitCSV(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}
