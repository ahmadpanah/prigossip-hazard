// Command workloadgen is the asynchronous load-generation tool of Section
// "Network Topologies and Edge-AI Workload": it emulates vehicular clients
// dispatching continuous PilotNet inference requests over persistent
// HTTP/1.1 connections and reports the QoS metrics used in the evaluation
// (per-client SLA satisfaction, Jain's Fairness Index and tail latency).
package main

import (
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Default seed schedule of the statistical protocol (N = 10 runs).
var defaultSeeds = []int64{42, 107, 251, 389, 523, 641, 757, 881, 997, 1103}

type options struct {
	targets      []string
	clients      int
	surgeClients int
	rps          float64
	duration     time.Duration
	surgeAt      time.Duration
	seed         int64
	tau          float64
	rho          float64
	window       time.Duration
	path         string
	out          string
	csv          string
	timeout      time.Duration
}

type clientResult struct {
	id          int
	latencies   []float64 // ms, all samples
	samples     int
	failures    int
	windowOK    int
	windowTotal int
	finalComply float64
	timeseries  []tsPoint
}

type tsPoint struct {
	At     time.Time `json:"at"`
	Client int       `json:"client"`
	Comply float64   `json:"comply"`
}

type summary struct {
	Seed             int64   `json:"seed"`
	Clients          int     `json:"clients"`
	TargetRPS        float64 `json:"targetRps"`
	ActualRPS        float64 `json:"actualRps"`
	DurationSec      float64 `json:"durationSec"`
	SLAMetRatio      float64 `json:"slaMetRatio"`      // fraction of clients meeting rho
	MeanClientComply float64 `json:"meanClientComply"` // mean per-client compliance
	JainFairness     float64 `json:"jainFairness"`     // JFI over instance request counts
	P50Ms            float64 `json:"p50Ms"`
	P90Ms            float64 `json:"p90Ms"`
	P99Ms            float64 `json:"p99Ms"`
	MeanMs           float64 `json:"meanMs"`
	Requests         int     `json:"requests"`
	Errors           int     `json:"errors"`
	InstancesServed  int     `json:"instancesServed"`
	InstanceCounts   []int   `json:"instanceCounts"`
}

func main() {
	opts := parseFlags()
	rng := rand.New(rand.NewSource(opts.seed))

	httpClient := &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        opts.clients * 2,
			MaxIdleConnsPerHost: 8,
			IdleConnTimeout:     60 * time.Second,
			ForceAttemptHTTP2:   false,
		},
		Timeout: opts.timeout,
		// Persistent HTTP/1.1 keep-alive connections, as in the testbed.
	}

	var (
		mu         sync.Mutex
		results    = make([]*clientResult, 0, opts.clients+opts.surgeClients)
		instCounts = map[int]int{}
		totalReq   int
		totalErr   int
		errKinds   = map[string]int{}
	)

	stop := make(chan struct{})
	var wg sync.WaitGroup

	launch := func(id int, until time.Duration) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res := &clientResult{id: id}
			// Exponential inter-arrival times => Poisson process at rate rps.
			jitter := time.NewTimer(poissonDelay(rng, opts.rps))
			defer jitter.Stop()
			deadline := time.Now().Add(until)
			window := make([]struct {
				at time.Time
				ok bool
			}, 0, 256)

			for {
				select {
				case <-stop:
					mu.Lock()
					results = append(results, res)
					mu.Unlock()
					return
				case <-jitter.C:
				}
				if time.Now().After(deadline) {
					jitter.Reset(poissonDelay(rng, opts.rps))
					continue
				}
				ok, latMs, inst, err := issue(httpClient, opts)
				now := time.Now()
				mu.Lock()
				totalReq++
				if err != nil {
					totalErr++
					// Bucket by the first path segment of the error string so
					// failures are diagnosable without flooding the map.
					msg := err.Error()
					if len(msg) > 80 {
						msg = msg[:80]
					}
					errKinds[msg]++
				}
				if inst >= 0 {
					instCounts[inst]++
				}
				mu.Unlock()

				res.samples++
				if !ok {
					res.failures++
				}
				res.latencies = append(res.latencies, latMs)

				window = append(window, struct {
					at time.Time
					ok bool
				}{now, ok && latMs <= opts.tau})
				cutoff := now.Add(-opts.window)
				front := 0
				for front < len(window) && window[front].at.Before(cutoff) {
					front++
				}
				window = append(window[:0:0], window[front:]...)

				if len(window) > 0 {
					good := 0
					for _, w := range window {
						if w.ok {
							good++
						}
					}
					res.finalComply = float64(good) / float64(len(window))
					res.windowOK, res.windowTotal = good, len(window)
					if len(res.timeseries) == 0 || now.Sub(res.timeseries[len(res.timeseries)-1].At) >= time.Second {
						res.timeseries = append(res.timeseries, tsPoint{At: now, Client: id, Comply: res.finalComply})
					}
				}
				jitter.Reset(poissonDelay(rng, opts.rps))
			}
		}()
	}

	// Initial fleet.
	for i := 0; i < opts.clients; i++ {
		launch(i, opts.duration)
	}

	// Unannounced +50% workload surge.
	if opts.surgeClients > 0 && opts.surgeAt > 0 {
		go func() {
			select {
			case <-stop:
			case <-time.After(opts.surgeAt):
				fmt.Fprintf(os.Stderr, "workloadgen: injecting +%d clients at t=%.1fs\n",
					opts.surgeClients, opts.surgeAt.Seconds())
				for i := 0; i < opts.surgeClients; i++ {
					launch(opts.clients+i, opts.duration)
				}
			}
		}()
	}

	time.Sleep(opts.duration)
	close(stop)
	wg.Wait()

	sum := buildSummary(opts, results, instCounts, totalReq, totalErr)
	if totalErr > 0 {
		fmt.Fprintf(os.Stderr, "workloadgen: %d transport errors by kind:\n", totalErr)
		for msg, n := range errKinds {
			fmt.Fprintf(os.Stderr, "  %4d x %s\n", n, msg)
		}
	}
	if !seedInSchedule(opts.seed) {
		fmt.Fprintf(os.Stderr, "workloadgen: seed %d is outside the paper's schedule %v\n", opts.seed, defaultSeeds)
	}
	report(sum, opts, results)
}

// seedInSchedule reports whether the seed belongs to the paper's published
// N = 10 repetition schedule.
func seedInSchedule(seed int64) bool {
	for _, s := range defaultSeeds {
		if s == seed {
			return true
		}
	}
	return false
}

func parseFlags() options {
	var o options
	targets := flag.String("targets", "http://127.0.0.1:8080", "comma-separated proxy base URLs")
	flag.IntVar(&o.clients, "clients", 120, "number of vehicular clients (4 per proxy)")
	flag.IntVar(&o.surgeClients, "surge-clients", 60, "additional clients injected at -surge-at (+50%)")
	flag.Float64Var(&o.rps, "rps", 10, "requests per second per client")
	flag.DurationVar(&o.duration, "duration", 120*time.Second, "run duration")
	flag.DurationVar(&o.surgeAt, "surge-at", 60*time.Second, "time of the workload surge")
	flag.Int64Var(&o.seed, "seed", 42, "global random seed (see the paper's seed schedule)")
	flag.Float64Var(&o.tau, "tau", 80, "SLA latency threshold in ms")
	flag.Float64Var(&o.rho, "rho", 0.90, "SLA success ratio")
	flag.DurationVar(&o.window, "window", 10*time.Second, "SLA sliding window W")
	flag.StringVar(&o.path, "path", "/infer", "request path")
	flag.StringVar(&o.out, "out", "", "write JSON summary to this file")
	flag.StringVar(&o.csv, "csv", "", "write per-client compliance CSV to this file")
	flag.DurationVar(&o.timeout, "timeout", 5*time.Second, "per-request timeout")
	flag.Parse()

	for _, t := range strings.Split(*targets, ",") {
		if t = strings.TrimSpace(t); t != "" {
			o.targets = append(o.targets, strings.TrimRight(t, "/"))
		}
	}
	if len(o.targets) == 0 {
		o.targets = []string{"http://127.0.0.1:8080"}
	}
	return o
}

func poissonDelay(rng *rand.Rand, rps float64) time.Duration {
	if rps <= 0 {
		return time.Second
	}
	u := rng.Float64()
	if u <= 0 {
		u = 1e-9
	}
	// Exponential inter-arrival with mean 1/rps.
	d := time.Duration(-math.Log(u) / rps * float64(time.Second))
	if d < time.Millisecond {
		d = time.Millisecond
	}
	return d
}

// issue sends one inference request, returns (met, latencyMs, instance, err).
func issue(c *http.Client, o options) (bool, float64, int, error) {
	base := o.targets[rand.Intn(len(o.targets))]
	start := time.Now()
	resp, err := c.Get(base + o.path)
	if err != nil {
		return false, float64(o.tau) * 2, -1, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	lat := float64(time.Since(start).Microseconds()) / 1000.0
	inst := -1
	if v := resp.Header.Get("X-PriGossip-Instance"); v != "" {
		inst, _ = strconv.Atoi(v)
	}
	return resp.StatusCode == 200 && lat <= o.tau, lat, inst, nil
}

func buildSummary(o options, results []*clientResult, instCounts map[int]int, totalReq, totalErr int) summary {
	s := summary{
		Seed:        o.seed,
		Clients:     len(results),
		TargetRPS:   o.rps * float64(len(results)),
		DurationSec: o.duration.Seconds(),
		Requests:    totalReq,
		Errors:      totalErr,
	}
	if totalReq > 0 && o.duration > 0 {
		s.ActualRPS = float64(totalReq) / o.duration.Seconds()
	}

	var all []float64
	met := 0
	var complySum float64
	for _, r := range results {
		all = append(all, r.latencies...)
		complySum += r.finalComply
		if r.finalComply >= o.rho {
			met++
		}
	}
	if len(results) > 0 {
		s.MeanClientComply = complySum / float64(len(results))
		s.SLAMetRatio = float64(met) / float64(len(results))
	}
	sort.Float64s(all)
	if len(all) > 0 {
		s.P50Ms = percentile(all, 0.50)
		s.P90Ms = percentile(all, 0.90)
		s.P99Ms = percentile(all, 0.99)
		var sum float64
		for _, v := range all {
			sum += v
		}
		s.MeanMs = sum / float64(len(all))
	}

	// Jain's Fairness Index over per-instance request counts.
	keys := make([]int, 0, len(instCounts))
	for k := range instCounts {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	var xs []float64
	for _, k := range keys {
		xs = append(xs, float64(instCounts[k]))
		s.InstanceCounts = append(s.InstanceCounts, instCounts[k])
	}
	s.InstancesServed = len(xs)
	s.JainFairness = jain(xs)
	return s
}

func jain(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var sum, sq float64
	for _, x := range xs {
		sum += x
		sq += x * x
	}
	if sq == 0 {
		return 0
	}
	return (sum * sum) / (float64(len(xs)) * sq)
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := p * float64(len(sorted)-1)
	lo := int(math.Floor(idx))
	hi := int(math.Ceil(idx))
	if lo == hi {
		return sorted[lo]
	}
	frac := idx - float64(lo)
	return sorted[lo]*(1-frac) + sorted[hi]*frac
}

func report(s summary, o options, results []*clientResult) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(s)

	if o.out != "" {
		f, err := os.Create(o.out)
		if err == nil {
			e := json.NewEncoder(f)
			e.SetIndent("", "  ")
			_ = e.Encode(s)
			f.Close()
		}
	}
	if o.csv != "" {
		f, err := os.Create(o.csv)
		if err != nil {
			fmt.Fprintf(os.Stderr, "workloadgen: %v\n", err)
			return
		}
		w := csv.NewWriter(f)
		_ = w.Write([]string{"at", "client", "comply"})
		rows := 0
		for _, r := range results {
			for _, p := range r.timeseries {
				_ = w.Write([]string{
					p.At.Format(time.RFC3339Nano),
					strconv.Itoa(p.Client),
					strconv.FormatFloat(p.Comply, 'f', 4, 64),
				})
				rows++
			}
		}
		w.Flush()
		f.Close()
		fmt.Fprintf(os.Stderr, "workloadgen: wrote %s (%d clients, %d samples)\n", o.csv, len(results), rows)
	}
}
