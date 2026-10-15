// Command tracegen produces the pairwise latency traces used by the
// evaluation harness for the five European topologies (T1-T5) of Section
// "Network Topologies and Edge-AI Workload".
//
// Provenance: the topology compositions and RTT envelopes are those reported
// in the paper (derived from the WonderNetwork global repository). The
// generator turns those envelopes into deterministic per-pair averages and
// deviations so the artifact is reproducible without redistributing
// WonderNetwork's raw dump; drop a raw export into traces/ using the same
// columns to evaluate against real measurements.
package main

import (
	"encoding/csv"
	"flag"
	"fmt"
	"hash/fnv"
	"math"
	"os"
	"path/filepath"
	"strconv"
)

// topology describes one non-isomorphic WAN topology.
type topology struct {
	ID     string
	Name   string
	Cities []string
	MinRTT float64 // ms
	MaxRTT float64 // ms
}

// The five topologies of the experimental testbed.
var topologies = []topology{
	{
		ID: "t1", Name: "Central European Core",
		Cities: []string{"Frankfurt", "Munich", "Zurich", "Vienna"},
		MinRTT: 12, MaxRTT: 38,
	},
	{
		ID: "t2", Name: "Western European Corridor",
		Cities: []string{"London", "Paris", "Brussels", "Amsterdam"},
		MinRTT: 18, MaxRTT: 45,
	},
	{
		ID: "t3", Name: "Southern European Ring",
		Cities: []string{"Milan", "Madrid", "Marseille", "Rome"},
		MinRTT: 24, MaxRTT: 62,
	},
	{
		ID: "t4", Name: "Nordic-Baltic Mesh",
		Cities: []string{"Stockholm", "Helsinki", "Oslo", "Copenhagen"},
		MinRTT: 15, MaxRTT: 52,
	},
	{
		ID: "t5", Name: "Pan-European High-Diameter Transit",
		Cities: []string{"Dublin", "Bucharest", "Lisbon", "Warsaw"},
		MinRTT: 35, MaxRTT: 110,
	},
}

func main() {
	outDir := flag.String("out", "traces", "output directory")
	flag.Parse()

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		fatal(err)
	}

	index := filepath.Join(*outDir, "index.csv")
	f, err := os.Create(index)
	if err != nil {
		fatal(err)
	}
	w := csv.NewWriter(f)
	_ = w.Write([]string{"topology", "name", "cities", "min_rtt_ms", "max_rtt_ms", "pairs"})

	for _, t := range topologies {
		path := filepath.Join(*outDir, fmt.Sprintf("%s_%s.csv", t.ID, snake(t.Name)))
		if err := writeTrace(path, t); err != nil {
			fatal(err)
		}
		_ = w.Write([]string{
			t.ID, t.Name, strconv.Itoa(len(t.Cities)),
			fmt.Sprintf("%.1f", t.MinRTT), fmt.Sprintf("%.1f", t.MaxRTT),
			strconv.Itoa(len(t.Cities) * (len(t.Cities) - 1)),
		})
		fmt.Printf("wrote %s (%d directed pairs, %.0f-%.0f ms)\n",
			path, len(t.Cities)*(len(t.Cities)-1), t.MinRTT, t.MaxRTT)
	}
	w.Flush()
	f.Close()
}

// writeTrace emits the directed pair table:
//
//	topology,source,destination,avg_rtt_ms,stddev_ms,samples
func writeTrace(path string, t topology) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	w := csv.NewWriter(f)
	defer w.Flush()
	if err := w.Write([]string{
		"topology", "source", "destination", "avg_rtt_ms", "stddev_ms", "samples",
	}); err != nil {
		return err
	}

	// Assign each city a stable latent coordinate; pair distances map onto
	// the topology's RTT envelope while keeping the matrix symmetric.
	coord := make(map[string]float64, len(t.Cities))
	for _, c := range t.Cities {
		coord[c] = latent(t.ID, c)
	}

	for _, src := range t.Cities {
		for _, dst := range t.Cities {
			if src == dst {
				continue
			}
			span := math.Abs(coord[src] - coord[dst])
			if span > 1 {
				span = 1
			}
			rtt := t.MinRTT + span*(t.MaxRTT-t.MinRTT)
			// WonderNetwork reports the average of 30 pings plus the spread;
			// the deviation grows with distance (congestion variance).
			dev := 0.04*rtt + 0.5*span*(t.MaxRTT-t.MinRTT)*0.05
			if err := w.Write([]string{
				t.ID, src, dst,
				fmt.Sprintf("%.2f", rtt),
				fmt.Sprintf("%.2f", dev),
				"30",
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

// latent derives a deterministic coordinate in [0,1] for a city inside a
// topology, so regenerating the traces is byte-for-byte reproducible.
func latent(topologyID, city string) float64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(topologyID + "/" + city))
	v := h.Sum64()
	return float64(v%10_000) / 10_000.0
}

func snake(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r == ' ' || r == '-':
			out = append(out, '_')
		case r >= 'A' && r <= 'Z':
			out = append(out, r+('a'-'A'))
		default:
			out = append(out, r)
		}
	}
	return string(out)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
