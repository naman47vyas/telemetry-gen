// Every host metric, not just the one series an alert reads.
//
// The other profiles emit exactly what their rule evaluates, which is enough to fire the
// alert and leaves the host page behind it empty. This one emits the whole default set the
// OpenTelemetry hostmetrics receiver produces — CPU, memory, disk, filesystem, load,
// network, paging and processes — so a synthetic host looks like a real one everywhere it
// appears, not just on the one chart.
//
// Names, instrument types, units and attribute values are the receiver's own, taken from
// its scrapers' documentation.md. The two utilization gauges (system.cpu.utilization,
// system.memory.utilization) are off by default upstream and on in the mw-agent, which is
// what the single-metric host profiles here already rely on.
//
// It lives in its own file because it is forty-odd metrics of detail that nobody reading
// the batch loop in main.go needs to scroll past.
package main

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
)

// The synthetic machine every host in this profile is a copy of, give or take the per-host
// variation below. One shape for all of them keeps the numbers legible; the sizes are a
// mundane 8-core cloud box.
const (
	hfCores    = 8
	hfMemTotal = 32 << 30 // 32 GiB
	hfSwapSize = 8 << 30  // 8 GiB
	hfFsTotal  = 500 << 30
	hfInodes   = 32 << 20
)

var (
	hfDisks  = []string{"sda", "sdb"}
	hfMounts = []struct{ device, mountpoint, fstype string }{
		{"/dev/sda1", "/", "ext4"},
		{"/dev/sdb1", "/var", "ext4"},
	}
	// TCP connection states, the ones a machine actually holds a pile of.
	hfConnStates = []struct {
		state string
		n     float64
	}{{"ESTABLISHED", 180}, {"TIME_WAIT", 240}, {"LISTEN", 22}, {"CLOSE_WAIT", 6}}
	hfProcStates = []struct {
		status string
		n      float64
	}{{"sleeping", 310}, {"running", 4}, {"idle", 96}, {"zombies", 1}, {"blocked", 2}}
)

const hfNIC = "eth0"
const hfSwapDev = "/dev/sda2"

// hfVary is a per-host multiplier around 1, stable for the life of a host: hosts differ from
// one another, but any single host's chart is its own steady line rather than noise.
func hfVary(c container, salt string, spread float64) float64 {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d", salt, c.idx)))
	return 1 + (float64(binary.BigEndian.Uint16(sum[:2]))/65535*2-1)*spread
}

// hfWobble is the same idea per tick, so a chart is a line with life in it and not a ruler.
// It is only ever applied to metrics no alert rule reads. The values that decide a severity
// are left exact: ±3% around 87% would wander across the 85-90 warning band and hand you a
// host that changes badge on its own.
func hfWobble(c container, seq int, salt string, amp float64) float64 {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%d", salt, c.idx, seq)))
	return 1 + (float64(binary.BigEndian.Uint16(sum[:2]))/65535*2-1)*amp
}

// hfSum builds one cumulative Sum datapoint. Cumulative counters carry a start time and are
// expected to climb; a flat one is indistinguishable from a stalled agent, and any rule that
// diffs over a window sees nothing at all.
func hfSum(name string, now, start time.Time, monotonic bool, v float64, attrs ...*commonpb.KeyValue) *metricspb.Metric {
	return &metricspb.Metric{
		Name: name,
		Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{
			AggregationTemporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
			IsMonotonic:            monotonic,
			DataPoints: []*metricspb.NumberDataPoint{{
				StartTimeUnixNano: uint64(start.UnixMilli()) * 1e6,
				TimeUnixNano:      uint64(now.UnixMilli()) * 1e6,
				Value:             &metricspb.NumberDataPoint_AsDouble{AsDouble: v},
				Attributes:        attrs,
			}},
		}},
	}
}

// hfSumInt is the same for the counters the receiver declares as Int — counts of things,
// where a fractional value would be the wrong type to chart.
func hfSumInt(name string, now, start time.Time, monotonic bool, v int64, attrs ...*commonpb.KeyValue) *metricspb.Metric {
	return &metricspb.Metric{
		Name: name,
		Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{
			AggregationTemporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
			IsMonotonic:            monotonic,
			DataPoints: []*metricspb.NumberDataPoint{{
				StartTimeUnixNano: uint64(start.UnixMilli()) * 1e6,
				TimeUnixNano:      uint64(now.UnixMilli()) * 1e6,
				Value:             &metricspb.NumberDataPoint_AsInt{AsInt: v},
				Attributes:        attrs,
			}},
		}},
	}
}

// hostFullMetrics builds one host's entire metric set for one tick. `v` is the host's CPU
// busy FRACTION — the same value the host.cpu profile takes — so the healthy/warning/
// critical cohorts drive CPU here and everything else stays plausible around it.
func hostFullMetrics(cfg config, c container, _ string, v float64, seq int, now time.Time) []*metricspb.Metric {
	// A cumulative counter is only meaningful against a start, and its value is a rate times
	// how long the host has been up — which, for a synthetic host, is how many ticks it has
	// lived through.
	start := now.Add(-time.Duration(seq) * cfg.interval)
	up := cfg.interval.Seconds() * float64(seq)
	m := make([]*metricspb.Metric, 0, 64)

	// ---------------------------------------------------------------------------- CPU
	// "High CPU usage for host" is the formula user + system + wait + steal, so those four
	// are made to add up to EXACTLY v: the three small ones are fixed and user takes the
	// remainder. Spreading the busy fraction any other way puts a host aimed at the middle
	// of the 85-90 warning band at 89.7% and lets it read critical instead.
	const cpuSystem, cpuWait, cpuSteal = 0.004, 0.002, 0.001
	const cpuNice, cpuSoftirq, cpuIRQ = 0.001, 0.001, 0.001
	cpuStates := []struct {
		state string
		frac  float64
	}{
		{"user", math.Max(v-cpuSystem-cpuWait-cpuSteal, 0)},
		{"system", cpuSystem}, {"wait", cpuWait}, {"steal", cpuSteal},
		{"nice", cpuNice}, {"softirq", cpuSoftirq}, {"interrupt", cpuIRQ},
		{"idle", math.Max(1-v-cpuNice-cpuSoftirq-cpuIRQ, 0)},
	}
	m = append(m, hfSumInt("system.cpu.logical.count", now, start, false, hfCores))
	for _, s := range cpuStates {
		m = append(m, gauge("system.cpu.utilization", now, s.frac, dpAttr("state", s.state)))
		// Seconds of CPU spent in that state, across every core, since the host came up.
		m = append(m, hfSum("system.cpu.time", now, start, true, s.frac*up*hfCores, dpAttr("state", s.state)))
	}

	// -------------------------------------------------------------------------- memory
	// Held at a plausible level rather than driven by v: v is how busy the CPU is, and a
	// busy host is not necessarily a full one. Use high-memory-usage-for-host.sh to fire
	// the memory rule.
	memStates := []struct {
		state string
		frac  float64
	}{
		// Capped so the states still add up to exactly 1 at the top of the per-host spread —
		// the fixed states below take 0.30, and a "free" clamped off the bottom would leave
		// a host whose memory adds up to 102%.
		{"used", math.Min(0.62*hfVary(c, "mem", 0.15), 0.69)},
		{"cached", 0.18}, {"buffered", 0.04}, {"inactive", 0.05},
		{"slab_reclaimable", 0.02}, {"slab_unreclaimable", 0.01},
	}
	taken := 0.0
	for _, s := range memStates {
		taken += s.frac
	}
	memStates = append(memStates, struct {
		state string
		frac  float64
	}{"free", 1 - taken})
	for _, s := range memStates {
		m = append(m, gauge("system.memory.utilization", now, s.frac, dpAttr("state", s.state)))
		m = append(m, hfSumInt("system.memory.usage", now, start, false, int64(s.frac*hfMemTotal), dpAttr("state", s.state)))
	}

	// ---------------------------------------------------------------------------- disk
	for _, d := range hfDisks {
		r := hfVary(c, "disk"+d, 0.45)
		dev := dpAttr("device", d)
		read, write := dpAttr("direction", "read"), dpAttr("direction", "write")
		m = append(m,
			hfSumInt("system.disk.io", now, start, true, int64(2.4e6*r*up), dev, read),
			hfSumInt("system.disk.io", now, start, true, int64(1.1e6*r*up), dev, write),
			hfSumInt("system.disk.operations", now, start, true, int64(180*r*up), dev, read),
			hfSumInt("system.disk.operations", now, start, true, int64(90*r*up), dev, write),
			hfSumInt("system.disk.merged", now, start, true, int64(12*r*up), dev, read),
			hfSumInt("system.disk.merged", now, start, true, int64(34*r*up), dev, write),
			hfSum("system.disk.operation_time", now, start, true, 0.05*r*up, dev, read),
			hfSum("system.disk.operation_time", now, start, true, 0.03*r*up, dev, write),
			hfSum("system.disk.io_time", now, start, true, 0.08*r*up, dev),
			hfSum("system.disk.weighted_io_time", now, start, true, 0.12*r*up, dev),
			// Not monotonic: the queue depth right now, which goes up and down.
			hfSumInt("system.disk.pending_operations", now, start, false, int64(math.Round(2*hfWobble(c, seq, "pending"+d, 0.9))), dev),
		)
	}

	// ---------------------------------------------------------------------- filesystem
	for _, f := range hfMounts {
		used := 0.46 * hfVary(c, "fs"+f.mountpoint, 0.35)
		attrs := []*commonpb.KeyValue{
			dpAttr("device", f.device), dpAttr("mode", "rw"),
			dpAttr("mountpoint", f.mountpoint), dpAttr("type", f.fstype),
		}
		// Reserved is the root-only 5% ext4 keeps back; free is what is actually left.
		const reserved = 0.05
		for _, s := range []struct {
			state string
			frac  float64
		}{{"used", used}, {"reserved", reserved}, {"free", math.Max(1-used-reserved, 0.01)}} {
			with := append(append([]*commonpb.KeyValue{}, attrs...), dpAttr("state", s.state))
			m = append(m,
				hfSumInt("system.filesystem.usage", now, start, false, int64(s.frac*hfFsTotal), with...),
				hfSumInt("system.filesystem.inodes.usage", now, start, false, int64(s.frac*hfInodes), with...),
			)
		}
		m = append(m, gauge("system.filesystem.utilization", now, used, attrs...))
	}

	// ---------------------------------------------------------------------------- load
	// Load tracks the CPU: a box at 90% of eight cores is carrying about seven runnable
	// threads. The longer averages lag, which is what makes the three lines worth charting.
	load := v * hfCores
	m = append(m,
		gauge("system.cpu.load_average.1m", now, load*hfWobble(c, seq, "l1", 0.12)),
		gauge("system.cpu.load_average.5m", now, load*hfWobble(c, seq, "l5", 0.06)),
		gauge("system.cpu.load_average.15m", now, load*hfWobble(c, seq, "l15", 0.03)),
	)

	// ------------------------------------------------------------------------- network
	nr := hfVary(c, "net", 0.5)
	dev := dpAttr("device", hfNIC)
	rx, tx := dpAttr("direction", "receive"), dpAttr("direction", "transmit")
	m = append(m,
		hfSumInt("system.network.io", now, start, true, int64(3.1e6*nr*up), dev, rx),
		hfSumInt("system.network.io", now, start, true, int64(1.7e6*nr*up), dev, tx),
		hfSumInt("system.network.packets", now, start, true, int64(2600*nr*up), dev, rx),
		hfSumInt("system.network.packets", now, start, true, int64(1900*nr*up), dev, tx),
		// Errors and drops are rare on a healthy NIC, but a flat zero reads as "not
		// collected"; a handful an hour reads as a working interface.
		hfSumInt("system.network.errors", now, start, true, int64(0.002*nr*up), dev, rx),
		hfSumInt("system.network.errors", now, start, true, int64(0.001*nr*up), dev, tx),
		hfSumInt("system.network.dropped", now, start, true, int64(0.004*nr*up), dev, rx),
		hfSumInt("system.network.dropped", now, start, true, int64(0.003*nr*up), dev, tx),
	)
	for _, s := range hfConnStates {
		m = append(m, hfSumInt("system.network.connections", now, start, false,
			int64(math.Round(s.n*hfVary(c, "conn", 0.3)*hfWobble(c, seq, "conn"+s.state, 0.15))),
			dpAttr("protocol", "tcp"), dpAttr("state", s.state)))
	}

	// -------------------------------------------------------------------------- paging
	swapUsed := 0.12 * hfVary(c, "swap", 0.6)
	for _, s := range []struct {
		state string
		frac  float64
	}{{"used", swapUsed}, {"cached", 0.01}, {"free", math.Max(1-swapUsed-0.01, 0.01)}} {
		m = append(m,
			hfSumInt("system.paging.usage", now, start, false, int64(s.frac*hfSwapSize), dpAttr("device", hfSwapDev), dpAttr("state", s.state)),
			gauge("system.paging.utilization", now, s.frac, dpAttr("device", hfSwapDev), dpAttr("state", s.state)),
		)
	}
	pr := hfVary(c, "page", 0.5)
	m = append(m,
		hfSumInt("system.paging.faults", now, start, true, int64(1400*pr*up), dpAttr("type", "minor")),
		hfSumInt("system.paging.faults", now, start, true, int64(0.6*pr*up), dpAttr("type", "major")),
		hfSumInt("system.paging.operations", now, start, true, int64(0.9*pr*up), dpAttr("direction", "page_in"), dpAttr("type", "major")),
		hfSumInt("system.paging.operations", now, start, true, int64(6*pr*up), dpAttr("direction", "page_in"), dpAttr("type", "minor")),
		hfSumInt("system.paging.operations", now, start, true, int64(0.4*pr*up), dpAttr("direction", "page_out"), dpAttr("type", "major")),
		hfSumInt("system.paging.operations", now, start, true, int64(4*pr*up), dpAttr("direction", "page_out"), dpAttr("type", "minor")),
	)

	// ----------------------------------------------------------------------- processes
	for _, p := range hfProcStates {
		m = append(m, hfSumInt("system.processes.count", now, start, false,
			int64(math.Round(p.n*hfVary(c, "proc", 0.25)*hfWobble(c, seq, "proc"+p.status, 0.05))),
			dpAttr("status", p.status)))
	}
	m = append(m, hfSumInt("system.processes.created", now, start, true, int64(1.2*hfVary(c, "spawn", 0.5)*up)))

	return m
}
