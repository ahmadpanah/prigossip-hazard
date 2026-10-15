// Package mqttx wraps the Eclipse Paho MQTT client used by the MQTT Informer
// (Section "Cluster Discovery via the MQTT Informer"). Endpoint descriptors
// are published as binary JSON payloads and consumed by local agent
// instances.
package mqttx

import (
	"fmt"
	"log"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// TopicRoot is the root of the discovery topic tree:
//
//	k8s/continuum/services/{service-name}/endpoints/+
//	k8s/continuum/agents/{service-name}/+
const TopicRoot = "k8s/continuum"

// ServiceTopic returns the publication topic for one service's endpoints.
func ServiceTopic(service string) string {
	return fmt.Sprintf("%s/services/%s/endpoints/+", TopicRoot, service)
}

// AgentTopic returns the topic on which peer proxies announce themselves.
func AgentTopic(service string) string {
	return fmt.Sprintf("%s/agents/%s/+", TopicRoot, service)
}

// Client is a thin, auto-reconnecting MQTT session. Subscriptions registered
// while the broker is unreachable are queued and (re)established by the
// on-connect handler, so a missing broker never strands the informer.
type Client struct {
	broker   string
	clientID string
	c        mqtt.Client

	mu   sync.Mutex
	subs map[string]func(payload []byte)
}

// New creates a disconnected client bound to broker (tcp://host:1883).
func New(broker, clientID string) *Client {
	return &Client{broker: broker, clientID: clientID, subs: map[string]func([]byte){}}
}

// Connect establishes the session; it retries in the background so a
// temporarily unreachable broker never blocks agent startup.
func (c *Client) Connect() error {
	opts := mqtt.NewClientOptions().
		AddBroker(c.broker).
		SetClientID(c.clientID).
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetConnectRetryInterval(2 * time.Second).
		SetKeepAlive(30 * time.Second).
		SetOrderMatters(false)
	opts.SetOnConnectHandler(func(mqtt.Client) {
		log.Printf("mqtt: connected to %s", c.broker)
		c.resubscribe()
	})
	opts.SetConnectionLostHandler(func(_ mqtt.Client, err error) {
		log.Printf("mqtt: connection lost: %v", err)
	})
	c.c = mqtt.NewClient(opts)
	tok := c.c.Connect()
	// Cap the synchronous wait so a missing broker cannot delay the proxy's
	// forwarding engine; SetConnectRetry keeps dialing in the background and
	// the on-connect handler installs any queued subscriptions.
	if !tok.WaitTimeout(time.Second) {
		return fmt.Errorf("mqtt: broker %s unreachable, dialing in background", c.broker)
	}
	if err := tok.Error(); err != nil {
		return fmt.Errorf("mqtt: connect to %s: %w (background retry continues)", c.broker, err)
	}
	return nil
}

// resubscribe installs every queued topic filter (idempotent across
// reconnects).
func (c *Client) resubscribe() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for topic, handler := range c.subs {
		tok := c.c.Subscribe(topic, 1, func(_ mqtt.Client, msg mqtt.Message) {
			handler(msg.Payload())
		})
		if tok.WaitTimeout(2*time.Second) && tok.Error() == nil {
			log.Printf("mqtt: subscribed to %s", topic)
		} else {
			log.Printf("mqtt: subscribe %s pending: %v", topic, tok.Error())
		}
	}
}

// Connected reports session state.
func (c *Client) Connected() bool { return c.c != nil && c.c.IsConnected() }

// Publish sends a payload (binary JSON) at QoS 1.
func (c *Client) Publish(topic string, retained bool, payload []byte) error {
	if c.c == nil {
		return fmt.Errorf("mqtt: not connected")
	}
	tok := c.c.Publish(topic, 1, retained, payload)
	if !tok.WaitTimeout(3 * time.Second) {
		return fmt.Errorf("mqtt: publish to %s timed out", topic)
	}
	return tok.Error()
}

// Subscribe registers a handler for a topic filter at QoS 1. When the broker
// is currently unreachable the subscription is queued and established on the
// next successful (re)connect, so Subscribe always returns nil once a client
// exists.
func (c *Client) Subscribe(topic string, handler func(payload []byte)) error {
	if c.c == nil {
		return fmt.Errorf("mqtt: client not initialised")
	}
	c.mu.Lock()
	c.subs[topic] = handler
	connected := c.c.IsConnected()
	c.mu.Unlock()
	if !connected {
		return nil // deferred to the on-connect handler
	}
	tok := c.c.Subscribe(topic, 1, func(_ mqtt.Client, msg mqtt.Message) {
		handler(msg.Payload())
	})
	if !tok.WaitTimeout(5 * time.Second) {
		return fmt.Errorf("mqtt: subscribe to %s timed out", topic)
	}
	return tok.Error()
}

// Close tears the session down.
func (c *Client) Close() {
	if c.c != nil && c.c.IsConnected() {
		c.c.Disconnect(250)
	}
}
