package main

import (
	"fmt"
	"math"
	"strings"
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

// The tier wave has to actually reach HEALTHY: that step is the whole reason it exists,
// and a cycle that never stops breaching is a cycle whose alert never resolves.
func TestTierWaveWalksAllThreeTiers(t *testing.T) {
	cfg := config{signal: "host.full", n: 8, breach: 8, ok: 0.05, warn: 0.87, bad: 0.96,
		// Three ticks to a step, as in a real run where the step is a rule window and the
		// interval is ten seconds. A step equal to the interval is the degenerate case: the
		// first entry of the cycle is held for [start, start+step), which is no ticks at all.
		interval: 10 * time.Second, sevFlap: 30 * time.Second, tierWave: "crit,warn,ok,warn",
		sevWave: sevWaveDefault, tierPhases: 1, prefix: "t", hosts: 1, clusters: 1, spans: 4}
	now := time.Now()
	g := newGenerator(cfg, now)

	var seen []string
	for tick := 1; tick <= 13; tick++ {
		b := g.tick(now.Add(time.Duration(tick)*cfg.interval), false)
		switch {
		case b.bad == cfg.n && b.warn == 0:
			seen = append(seen, "crit")
		case b.warn == cfg.n && b.bad == 0:
			seen = append(seen, "warn")
		case b.bad == 0 && b.warn == 0:
			seen = append(seen, "ok")
		default:
			t.Fatalf("tick %d is a mix (%d critical, %d warning); lockstep should be all one tier", tick, b.bad, b.warn)
		}
		if n := len(seen); n > 1 && seen[n-1] == seen[n-2] {
			seen = seen[:n-1] // same step, still; only the transitions are interesting
		}
		// Whatever the tier, the four states the host CPU rule sums must still add up to
		// exactly the value for that tier — including the healthy one, or the resolve is
		// a resolve to some other number.
		for _, rm := range b.metrics.ResourceMetrics {
			want := map[string]float64{"crit": cfg.bad, "warn": cfg.warn, "ok": cfg.ok}[seen[len(seen)-1]]
			if got := formulaSum(rm.ScopeMetrics[0].Metrics); math.Abs(got-want) > 1e-9 {
				t.Fatalf("tick %d: cpu formula is %v, want %v", tick, got, want)
			}
		}
	}
	got := strings.Join(seen, ",")
	if want := "crit,warn,ok,warn,crit"; got != want {
		t.Errorf("cycle walked %s, want %s", got, want)
	}
}

// formulaSum is what "High CPU usage for host" evaluates: user + system + wait + steal.
func formulaSum(ms []*metricspb.Metric) float64 {
	total := 0.0
	for _, m := range ms {
		if m.Name != "system.cpu.utilization" || m.GetGauge() == nil {
			continue
		}
		dp := m.GetGauge().DataPoints[0]
		for _, a := range dp.Attributes {
			if a.Key != "state" {
				continue
			}
			switch a.Value.GetStringValue() {
			case "user", "system", "wait", "steal":
				total += dp.GetAsDouble()
			}
		}
	}
	return total
}
