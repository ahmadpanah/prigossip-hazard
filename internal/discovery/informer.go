package discovery

import (
	"context"
	"encoding/json"
	"log"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"github.com/ahmadpanah/prigossip-hazard/internal/mqttx"
)

// Informer is the MQTT Informer: an event-driven cluster discovery module
// built on the client-go cache package. It observes Pod lifecycle mutations
// and Service endpoint changes for the QoS-labelled workload and republishes
// them over MQTT so that local agents avoid API-server polling.
type Informer struct {
	client        kubernetes.Interface
	registry      *Registry
	bus           *mqttx.Client
	namespace     string
	labelSelector labels.Selector
	selfAddr      string
	selfName      string
	port          int
}

// Options configures the informer.
type Options struct {
	Namespace     string
	LabelSelector string // e.g. "app=vision-ai"
	Bus           *mqttx.Client
	Registry      *Registry
	SelfAddr      string // local gossip address host:port
	SelfName      string // this agent's pod name
	BackendPort   int    // container port of the managed service
}

// NewInformer wires an informer against an in-cluster (or kubeconfig) client.
func NewInformer(client kubernetes.Interface, opt Options) (*Informer, error) {
	sel, err := labels.Parse(opt.LabelSelector)
	if err != nil {
		return nil, err
	}
	if opt.Namespace == "" {
		opt.Namespace = "default"
	}
	return &Informer{
		client:        client,
		registry:      opt.Registry,
		bus:           opt.Bus,
		namespace:     opt.Namespace,
		labelSelector: sel,
		selfAddr:      opt.SelfAddr,
		selfName:      opt.SelfName,
		port:          opt.BackendPort,
	}, nil
}

// Run starts the shared informer factory, syncs the caches and re-announces
// the registry periodically until ctx is cancelled.
func (in *Informer) Run(ctx context.Context) error {
	factory := informers.NewSharedInformerFactoryWithOptions(
		in.client,
		30*time.Second,
		informers.WithNamespace(in.namespace),
		informers.WithTweakListOptions(func(o *metav1.ListOptions) {
			o.LabelSelector = in.labelSelector.String()
		}),
	)

	pods := factory.Core().V1().Pods()
	_, err := pods.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { in.onPods() },
		UpdateFunc: func(_, _ interface{}) { in.onPods() },
		DeleteFunc: func(obj interface{}) { in.onPods() },
	})
	if err != nil {
		return err
	}

	eps := factory.Core().V1().Endpoints()
	_, err = eps.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { in.onEndpoints() },
		UpdateFunc: func(_, _ interface{}) { in.onEndpoints() },
		DeleteFunc: func(obj interface{}) { in.onEndpoints() },
	})
	if err != nil {
		return err
	}

	factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), pods.Informer().HasSynced, eps.Informer().HasSynced) {
		return context.Canceled
	}
	log.Printf("discovery: informer caches synced (selector=%q ns=%q)", in.labelSelector, in.namespace)

	in.announceSelf()
	in.onPods()
	in.onEndpoints()

	// Periodic re-announcement compensates for lost retained messages.
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			in.announceSelf()
			in.publishEndpoints()
		}
	}
}

// onPods republishes the local endpoint view whenever the labelled pod set
// changes, and refreshes the peer-proxy list from agent pods.
func (in *Informer) onPods() {
	in.publishEndpoints()
}

// onEndpoints publishes the endpoint view whenever Service endpoints change,
// so agents in other routing domains learn about failovers without polling
// the API server.
func (in *Informer) onEndpoints() {
	in.publishEndpoints()
}

func (in *Informer) publishEndpoints() {
	if in.bus == nil {
		return
	}
	pods, err := in.client.CoreV1().Pods(in.namespace).List(context.Background(),
		metav1.ListOptions{LabelSelector: in.labelSelector.String()})
	if err != nil {
		log.Printf("discovery: list pods: %v", err)
		return
	}
	set := EndpointSet{Service: in.registry.Service()}
	for _, p := range pods.Items {
		if p.Status.Phase != corev1.PodRunning || p.Status.PodIP == "" {
			continue
		}
		set.Nodes = append(set.Nodes, Backend{
			Name: p.Name,
			IP:   p.Status.PodIP,
			Port: in.port,
			Node: p.Spec.NodeName,
		})
	}
	payload, err := json.Marshal(set)
	if err != nil {
		return
	}
	in.registry.Apply(set)
	if err := in.bus.Publish(mqttx.ServiceTopic(in.registry.Service()), false, payload); err != nil {
		log.Printf("discovery: publish endpoints: %v", err)
	}
}

// announceSelf advertises this agent's gossip address (retained, so newly
// started proxies discover the ring immediately).
func (in *Informer) announceSelf() {
	if in.bus == nil || in.selfAddr == "" {
		return
	}
	payload, _ := json.Marshal(map[string]string{"name": in.selfName, "addr": in.selfAddr})
	topic := mqttx.TopicRoot + "/agents/" + in.registry.Service() + "/" + in.selfName
	if err := in.bus.Publish(topic, true, payload); err != nil {
		log.Printf("discovery: announce self: %v", err)
	}
}
