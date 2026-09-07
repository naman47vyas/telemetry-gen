package main

import (
	"fmt"
	"testing"
	"time"

	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
)

// Cumulative counters have to climb. A flat one is indistinguishable from a stalled agent,
// and any rule that diffs over a window sees nothing at all — the mistake k8s.container.
// restarts was written to avoid, and easy to reintroduce in a metric set this size.
func TestHostFullCountersClimb(t *testing.T) {
	cfg := config{signal: "host.full", n: 3, breach: 1, warnN: 1, ok: 0.05, warn: 0.87, bad: 0.96,
		interval: 10 * time.Second, prefix: "t", hosts: 1, clusters: 1, spans: 4}
	now := time.Now()
	g := newGenerator(cfg, now)

	first, last := map[string]float64{}, map[string]float64{}
	const ticks = 30 // five minutes, a rule window
	for tick := 1; tick <= ticks; tick++ {
		b := g.tick(now.Add(time.Duration(tick)*cfg.interval), false)
		for ri, rm := range b.metrics.ResourceMetrics {
			for mi, m := range rm.ScopeMetrics[0].Metrics {
				sum := m.GetSum()
				if sum == nil || !sum.IsMonotonic {
					continue
				}
				dp := sum.DataPoints[0]
				v := dp.GetAsDouble() + float64(dp.GetAsInt())
				key := fmt.Sprintf("%d|%d|%s", ri, mi, m.Name)
				if p, seen := last[key]; seen && v < p {
					t.Fatalf("%s went backwards: %v -> %v at tick %d", m.Name, p, v, tick)
				}
				if _, seen := first[key]; !seen {
					first[key] = v
				}
				last[key] = v
			}
			if dp := statesSum(rm.ScopeMetrics[0].Metrics, "system.cpu.utilization"); dp < 0.999 || dp > 1.001 {
				t.Errorf("cpu utilization states sum to %v, not 1", dp)
			}
			if dp := statesSum(rm.ScopeMetrics[0].Metrics, "system.memory.utilization"); dp < 0.999 || dp > 1.001 {
				t.Errorf("memory utilization states sum to %v, not 1", dp)
			}
		}
	}

	var flat []string
	for k, v := range last {
		if v == first[k] {
			flat = append(flat, k)
		}
	}
	// system.network.errors and .dropped are deliberately rare — a healthy NIC drops a
	// packet every few minutes, and rounding holds them at zero for a while. Everything
	// else must have moved within a rule window.
	if len(flat)*10 > len(last) {
		t.Errorf("%d of %d monotonic series never moved in %d ticks: %v", len(flat), len(last), ticks, flat)
	}
	t.Logf("%d monotonic series, %d still flat after %d ticks (rare counters)", len(last), len(flat), ticks)
}

func statesSum(ms []*metricspb.Metric, name string) float64 {
	total := 0.0
	for _, m := range ms {
		if m.Name != name || m.GetGauge() == nil {
			continue
		}
		total += m.GetGauge().DataPoints[0].GetAsDouble()
	}
	return total
}
