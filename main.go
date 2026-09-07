// telemetrygen emits synthetic OTLP metrics straight into capture-metrics so the alert
// pipeline can be exercised at any cardinality without real workloads.
//
// It speaks raw OTLP protobuf over gRPC rather than the OTel SDK: the SDK binds one
// resource per MeterProvider, and the whole point here is N distinct resources per batch.
//
// Every interval it sends one batch containing one ResourceMetrics per synthetic
// container. Each resource carries the same 21 attributes the real mw-agent sends for a
// Docker container (captured from a live row); the pipeline derives type=container from
// the presence of container.id, and uniq_cpu_core falls back to 1 for hosts with no CPU
// metrics, so the fake hosts need nothing else.
//
// Two signals are not metrics at all. APM latency and APM errors both live in the spans
// table, so the trace.service and trace.error profiles emit ResourceSpans instead — same
// batch loop, same chunking, the traces service rather than the metrics one.
package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	collectormetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	// capture-metrics gzips its responses; registering the codec lets the client decode them.
	_ "google.golang.org/grpc/encoding/gzip"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// Attributes shared by every synthetic resource, copied from a real container row so the
// pipeline sees exactly the shape it already resolves. Per-container and per-host values
// are filled in by resourceFor.
var hostTemplate = map[string]string{
	"host.arch":              "amd64",
	"host.cpu.cache.l2.size": "12288",
	"host.cpu.family":        "6",
	"host.cpu.model.id":      "186",
	"host.cpu.model.name":    "13th Gen Intel(R) Core(TM) i7-1355U",
	"host.cpu.stepping":      "3",
	"host.cpu.vendor.id":     "GenuineIntel",
	"mw.agent.version":       "1.21.3",
	"mw.agent_id":            "1",
	"os.description":         "Ubuntu 24.04.3 LTS (Noble Numbat) (Linux synthetic)",
	"os.type":                "linux",
}

type config struct {
	endpoint, apiKey, authHeader string
	signal, metric, prefix       string
	n, breach, hosts, churn      int
	clusters, spans, warnN       int
	ok, bad, warn                float64
	interval, duration, flap     time.Duration
	sevFlap                      time.Duration
	sevWave                      string
	recoverOnExit, dryRun        bool
	tls, plaintext               bool
	hostName                     string
}

// container is one synthetic group. gen increments when churn retires it, which mints a
// new container.id and name — a brand-new group as far as the engine is concerned.
type container struct {
	idx, gen int
}

func (c container) name(prefix, word string) string {
	if c.gen == 0 {
		return fmt.Sprintf("%s-%s-%04d", prefix, word, c.idx+1)
	}
	return fmt.Sprintf("%s-%s-%04d-g%d", prefix, word, c.idx+1, c.gen)
}

func (c container) id(prefix string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%d", prefix, c.idx, c.gen)))
	return hex.EncodeToString(sum[:])
}

func kv(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}

// kvInt is the same for the handful of span attributes that are numbers on the wire
// (http.response.status_code), where a string would be the wrong type to filter on.
func kvInt(k string, v int64) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: v}}}
}

// signalProfile is one kind of thing to synthesise: which metric it emits, sensible healthy
// and breaching values, and how to build one resource's attributes so the pipeline derives the
// right resource type. Adding a metric to test is adding a profile here.
type signalProfile struct {
	metric string
	ok     float64 // healthy value
	bad    float64 // breaching value — crosses the rule's CRITICAL threshold
	// warn is the value for the cohort that should come out WARNING rather than critical:
	// high enough to breach, low enough not to reach critical. NaN means this signal has no
	// middle (an enum has no value between Running and Failed), and asking for a warning
	// cohort on such a signal is refused rather than quietly shipped as critical.
	warn float64
	// attrs builds one entity's resource attributes. host is the entity's parent bucket
	// (host / namespace / node), spread across cfg.hosts.
	attrs func(cfg config, c container, host string, startedOn int64) []*commonpb.KeyValue
	// metrics builds the datapoints for one entity given its value and the tick sequence
	// (seq, for counters). nil = one gauge of `metric` with `value`. A profile can emit
	// several metrics (a formula), attach datapoint attributes (a pinned state), or emit a
	// whole machine's worth (host.full) — which is why it takes the entity as well: the
	// numbers that are not the alert's have to differ from host to host to look real.
	metrics func(cfg config, c container, metric string, value float64, seq int, now time.Time) []*metricspb.Metric
	// spans, when set, makes this a TRACE profile: the tick builds one ResourceSpans per
	// entity from these instead of a ResourceMetrics, and the batch goes to the traces
	// service. `value` is then milliseconds of request duration, not a gauge reading.
	spans func(cfg config, c container, value float64, seq int, now time.Time) []*tracepb.Span
}

// dpAttr is a datapoint attribute (e.g. state=used), how the agent pins a metric to a state.
func dpAttr(k, v string) *commonpb.KeyValue {
	return kv(k, v)
}

var signalProfiles = map[string]signalProfile{
	// Container CPU utilisation (percent on the wire): the "High Container CPU" rule.
	"container": {
		metric: "container.cpu.utilization",
		ok:     2,
		bad:    95,
		warn:   87, // inside the 85-90 warning band; critical starts above 90
		attrs: func(cfg config, c container, host string, startedOn int64) []*commonpb.KeyValue {
			id := c.id(cfg.prefix)
			attrs := []*commonpb.KeyValue{
				kv("container.hostname", id[:12]),
				kv("container.id", id),
				kv("container.image.name", "synthetic/idle:latest"),
				kv("container.name", c.name(cfg.prefix, "container")),
				kv("container.runtime", "docker"),
				kv("container.started_on", fmt.Sprint(startedOn)),
				kv("host.id", host),
				kv("host.ip", "[\"Interface: eth0, IP: 10.0.0.1\"]"),
				kv("host.mac", "[\"Interface: eth0, MAC: 02-00-00-00-00-01\"]"),
				kv("host.name", host),
			}
			for k, v := range hostTemplate {
				attrs = append(attrs, kv(k, v))
			}
			return attrs
		},
	},
	// Pod phase for the "Pods are failing" rule: 2 = Running (healthy), 4 = Failed (breaching
	// >3). k8s.pod is derived from k8s.pod.uid; here "host" is the pod's namespace, so a run
	// spreads pods across cfg.hosts namespaces. k8s.pod.phase is raw (no scaling).
	"k8s.pod": {
		metric: "k8s.pod.phase",
		ok:     2,
		bad:    4,
		// No warning tier: the phases above Running are Succeeded, Failed and Unknown, and
		// none of them is "a bit failed". container.status is the same story.
		warn: math.NaN(),
		attrs: func(cfg config, c container, host string, startedOn int64) []*commonpb.KeyValue {
			id := c.id(cfg.prefix)
			pod := c.name(cfg.prefix, "pod")
			return []*commonpb.KeyValue{
				kv("k8s.pod.uid", id),
				kv("k8s.pod.name", pod),
				kv("k8s.pod.start_time", fmt.Sprint(startedOn)),
				kv("k8s.namespace.name", host),
				kv("k8s.cluster.name", clusterFor(cfg, c)),
				kv("k8s.node.name", nodeFor(cfg, c)),
				kv("host.name", nodeFor(cfg, c)),
			}
		},
	},
	// Host memory utilisation for "High memory usage for host": raw fraction ×100 = percent,
	// pinned to state=used, grouped by host.name. Same shape as container CPU, host resource.
	"host.memory": {
		metric: "system.memory.utilization",
		ok:     0.20,
		bad:    0.92,
		warn:   0.87, // FRACTION, like the others here: 87%, inside the 85-90 warning band
		attrs:  hostAttrs,
		metrics: func(_ config, _ container, metric string, v float64, _ int, now time.Time) []*metricspb.Metric {
			return []*metricspb.Metric{gauge(metric, now, v, dpAttr("state", "used"))}
		},
	},
	// Host CPU utilisation for "High CPU usage for host": the rule is the FORMULA a+b+c+d over
	// four separate series (state=user/steal/wait/system), ×100 by the runtime. A formula
	// yields nothing for a host that lacks any of its inputs — not 0, no row — so every state
	// must be present. The whole busy fraction goes on state=user, the rest are 0. Grouped
	// by host.name.
	"host.cpu": {
		metric: "system.cpu.utilization",
		ok:     0.02,
		bad:    0.92,
		warn:   0.87, // FRACTION: 87% once the rule multiplies by 100, inside 85-90
		attrs:  hostAttrs,
		metrics: func(_ config, _ container, metric string, v float64, _ int, now time.Time) []*metricspb.Metric {
			return []*metricspb.Metric{
				gauge(metric, now, v, dpAttr("state", "user")),
				gauge(metric, now, 0, dpAttr("state", "steal")),
				gauge(metric, now, 0, dpAttr("state", "wait")),
				gauge(metric, now, 0, dpAttr("state", "system")),
			}
		},
	},
	// Node CPU for "High CPU utilization for node": the rule is a/b*100 where a =
	// k8s.node.cpu.utilization, b = k8s.node.allocatable_cpu. Emit a=value, b=100 so the ratio
	// is `value` percent. Grouped by k8s.cluster.name + k8s.node.name; here "host" is the node.
	"k8s.node": {
		metric: "k8s.node.cpu.utilization",
		ok:     10,
		bad:    95,
		warn:   87, // percent, because allocatable_cpu is emitted as 100; inside 85-90
		attrs: func(cfg config, c container, host string, startedOn int64) []*commonpb.KeyValue {
			node := c.name(cfg.prefix, "node")
			return []*commonpb.KeyValue{
				kv("k8s.node.uid", c.id(cfg.prefix)),
				kv("k8s.node.name", node),
				kv("k8s.cluster.name", clusterFor(cfg, c)),
				kv("host.id", node),
				kv("host.name", node),
			}
		},
		metrics: func(_ config, _ container, _ string, v float64, _ int, now time.Time) []*metricspb.Metric {
			return []*metricspb.Metric{
				gauge("k8s.node.cpu.utilization", now, v),
				gauge("k8s.node.allocatable_cpu", now, 100),
			}
		},
	},
	// Container restarts for "Pods are restarting" and "Pod is in CrashloopBackoff State":
	// k8s.container.restarts is a counter, so a breaching container RAMPS (value per tick) —
	// monotonic_difference over the window then exceeds the threshold. current_waiting_reason
	// = CrashLoopBackOff makes the CrashLoopBackOff rule match too. Grouped by
	// cluster/namespace/pod/container; here "host" is the namespace.
	"k8s.container": {
		metric: "k8s.container.restarts",
		ok:     0,
		bad:    2,
		// Restarts per tick, and the rule diffs over its window — so the warning cohort is
		// not a lower count but a SLOWER RAMP: 0.05/tick is one restart every twenty ticks.
		// The ramp is floored, so the counter still only ever reports whole restarts.
		warn: 0.05,
		attrs: func(cfg config, c container, host string, startedOn int64) []*commonpb.KeyValue {
			ctr := c.name(cfg.prefix, "container")
			return []*commonpb.KeyValue{
				kv("container.id", c.id(cfg.prefix)),
				kv("k8s.container.name", ctr),
				kv("k8s.pod.name", cfg.prefix+"-pod-"+fmt.Sprintf("%04d", c.idx+1)),
				kv("k8s.namespace.name", host),
				kv("k8s.cluster.name", clusterFor(cfg, c)),
				kv("k8s.node.name", nodeFor(cfg, c)),
				kv("host.name", nodeFor(cfg, c)),
			}
		},
		metrics: func(_ config, _ container, metric string, v float64, seq int, now time.Time) []*metricspb.Metric {
			// Cumulative restart count: a breaching container climbs by `v` each tick, a
			// healthy one stays flat at 0. Floored so a fractional ramp — how the warning
			// cohort restarts more slowly than the critical one — still reports whole
			// restarts rather than 2.5 of them.
			restarts := math.Floor(v * float64(seq))
			return []*metricspb.Metric{
				gauge(metric, now, restarts, dpAttr("current_waiting_reason", "CrashLoopBackOff")),
			}
		},
	},
	// A whole host, not one series: every metric the hostmetrics receiver collects by
	// default, so the host pages have something behind them. `value` is the CPU busy
	// FRACTION, same as host.cpu, so the breaching cohorts still drive that rule. The
	// metric set itself is in hostfull.go.
	"host.full": {
		metric:  "system.cpu.utilization",
		ok:      0.05, // an idle-ish box, not a dead one
		bad:     0.96, // FRACTION: 96%, over the 90% critical
		warn:    0.87, // FRACTION: 87%, inside the 85-90 warning band
		attrs:   hostAttrs,
		metrics: hostFullMetrics,
	},
	// Service latency for "Latency is higher than expected". This rule reads the APM spans
	// table, not a metric — a service's latency IS the duration of the requests it served —
	// so this profile emits TRACES. Every interval each service serves cfg.spans requests,
	// each a root SERVER span of `value` MILLISECONDS with a slow database CLIENT child
	// inside it, so the trace view shows a cause and not just a slow box.
	"trace.service": {
		metric: "trace.duration.ms",
		ok:     45,   // 45 ms — a request nobody would notice
		bad:    4000, // 4 s — MILLISECONDS, not a percent and not a fraction
		warn:   900,  // 0.9 s — slow enough to complain about, not slow enough to page
		attrs:  serviceAttrs,
		spans:  spansFor,
	},
	// Service errors for "Error rate is high" and "Errors detected in traces". Both rules
	// read the same spans table as the latency one — the first as a percentage, the second
	// as a count of error spans — so one profile serves both. `value` is the PERCENTAGE of
	// each service's requests that fail, and a failed request is a 5xx root span carrying an
	// ERROR status and an exception event, over a child that failed the same way.
	"trace.error": {
		metric: "trace.error_rate.pct",
		ok:     0,  // no failures at all — what a run sends on exit so the alert resolves
		bad:    85, // 85% of requests fail. A PERCENTAGE, not a fraction and not a count.
		warn:   12, // 12% of requests fail: a service in trouble, not a service that is down
		attrs:  serviceAttrs,
		spans:  errorSpansFor,
	},
}

// serviceAttrs builds one APM service resource, shared by both trace profiles. These rules
// group by service.name; here `host` is the machine the service runs on, spread over
// cfg.hosts so the service map is not one box with every service on it.
func serviceAttrs(cfg config, c container, host string, _ int64) []*commonpb.KeyValue {
	return []*commonpb.KeyValue{
		kv("service.name", c.name(cfg.prefix, "service")),
		kv("service.version", "1.0.0"),
		kv("service.instance.id", c.id(cfg.prefix)),
		kv("deployment.environment", "synthetic"),
		kv("telemetry.sdk.name", "opentelemetry"),
		kv("telemetry.sdk.language", "go"),
		kv("telemetry.sdk.version", "1.38.0"),
		kv("mw.app.lang", "go"),
		// The host the service runs on. No host.id: these are not hosts, and a host.id
		// would mint 200 phantom host entities carrying no host metrics.
		kv("host.name", host),
		kv("os.type", "linux"),
	}
}

// routes are the endpoints a synthetic service serves. Several of them, so the APM view
// has a resource breakdown to open up rather than one undifferentiated blob.
var routes = []struct{ name, method, route, path, table string }{
	{"GET /api/orders", "GET", "/api/orders", "/api/orders", "orders"},
	{"GET /api/orders/{id}", "GET", "/api/orders/{id}", "/api/orders/4711", "orders"},
	{"POST /api/checkout", "POST", "/api/checkout", "/api/checkout", "carts"},
	{"GET /api/inventory", "GET", "/api/inventory", "/api/inventory", "inventory"},
}

// spansFor builds one service's requests for this interval: cfg.spans root SERVER spans of
// `ms` milliseconds each, every one with a CLIENT child holding 80% of that time, and all
// of them successful.
//
// Two things here are deliberate. The duration jitters only ±8%, so avg, p50, p90 and p99
// all land on essentially the same number — whichever aggregation the rule actually uses,
// it sees a slow service. And the ids are hashed from prefix|idx|gen|seq|k rather than
// drawn from an RNG, so every request in a run has a unique trace without seeding anything.
func spansFor(cfg config, c container, ms float64, seq int, now time.Time) []*tracepb.Span {
	return buildSpans(cfg, c, ms, 0, seq, now)
}

// errorSpansFor is the same traffic with failures mixed in: `pct` is the PERCENTAGE of this
// service's requests that fail. Duration is held at errorLatencyMs so an error run does not
// also trip the latency rule and leave you unsure which one you were looking at.
func errorSpansFor(cfg config, c container, pct float64, seq int, now time.Time) []*tracepb.Span {
	return buildSpans(cfg, c, errorLatencyMs, pct, seq, now)
}

// errorLatencyMs is how long a request takes on the error profile: a mundane number, well
// under any latency threshold, whether it succeeded or failed.
const errorLatencyMs = 150

// failures are the ways a synthetic request can fail. Error tracking fingerprints on the
// exception type and message, so several kinds means the errors list has something to group
// by instead of one row repeated N times.
var failures = []struct {
	status        int64
	kind, message string
}{
	{500, "PostgresError", "connection reset by peer"},
	{500, "NullPointerException", "cannot read properties of undefined (reading 'id')"},
	{503, "UpstreamUnavailable", "no healthy upstream"},
	{500, "TimeoutError", "context deadline exceeded"},
	{500, "SerializationError", "invalid character 'x' looking for beginning of value"},
}

// failed decides whether request n of a service fails, spreading pct% of failures evenly
// across the run. Counting on a running index rather than rounding within each tick is what
// keeps a small percentage honest: at 4 requests an interval, rounding 5% gives 0% or 25%.
func failed(n int, pct float64) bool {
	switch {
	case pct <= 0:
		return false
	case pct >= 100:
		return true
	}
	return int(float64(n+1)*pct/100) > int(float64(n)*pct/100)
}

func okStatus() *tracepb.Status {
	return &tracepb.Status{Code: tracepb.Status_STATUS_CODE_OK}
}

// exceptionEvent is the span event OpenTelemetry records when a call throws, and what
// Middleware reads out of a trace to build an error.
func exceptionEvent(at uint64, kind, message, frame string) *tracepb.Span_Event {
	return &tracepb.Span_Event{
		TimeUnixNano: at,
		Name:         "exception",
		Attributes: []*commonpb.KeyValue{
			kv("exception.type", kind),
			kv("exception.message", message),
			kv("exception.stacktrace", kind+": "+message+"\n\tat "+frame+" (handler.go:118)\n\tat serve (server.go:642)"),
		},
	}
}

// buildSpans is the body behind both trace profiles: cfg.spans requests of `ms` each, of
// which errPct% fail.
func buildSpans(cfg config, c container, ms, errPct float64, seq int, now time.Time) []*tracepb.Span {
	out := make([]*tracepb.Span, 0, cfg.spans*2)
	for k := 0; k < cfg.spans; k++ {
		r := routes[(c.idx+k)%len(routes)]
		dur := time.Duration(ms * (0.92 + 0.16*float64((seq*7+k*13)%17)/16) * float64(time.Millisecond))
		// Stagger the requests back across the interval so a service's traffic is spread
		// through the bucket instead of arriving as one spike on the tick.
		end := now.Add(-time.Duration(k) * cfg.interval / time.Duration(cfg.spans))
		start := end.Add(-dur)
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%d|%d|%d", cfg.prefix, c.idx, c.gen, seq, k)))
		traceID, rootID, childID := sum[0:16], sum[16:24], sum[24:32]

		// Phase-shifted by c.idx so services fail on different requests rather than every
		// service failing in lockstep on the same tick.
		httpStatus := int64(200)
		rootStatus, childStatus := okStatus(), okStatus()
		var rootEvents, childEvents []*tracepb.Span_Event
		if failed((seq+c.idx)*cfg.spans+k, errPct) {
			f := failures[(c.idx+k)%len(failures)]
			httpStatus = f.status
			// The root and its child fail together, so the share of spans that are errors
			// is the same number whether the rule counts every span or only the SERVER
			// ones — the same reason the latency jitter is kept narrow.
			rootStatus = &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR, Message: f.message}
			childStatus = &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR, Message: f.message}
			at := uint64(start.Add(dur / 2).UnixNano())
			rootEvents = []*tracepb.Span_Event{exceptionEvent(at, f.kind, f.message, r.name)}
			childEvents = []*tracepb.Span_Event{exceptionEvent(at, f.kind, f.message, "SELECT "+r.table)}
		}
		out = append(out,
			&tracepb.Span{
				TraceId:           traceID,
				SpanId:            rootID,
				Name:              r.name,
				Kind:              tracepb.Span_SPAN_KIND_SERVER,
				StartTimeUnixNano: uint64(start.UnixNano()),
				EndTimeUnixNano:   uint64(end.UnixNano()),
				Attributes: []*commonpb.KeyValue{
					kv("http.request.method", r.method),
					kv("http.route", r.route),
					kv("url.path", r.path),
					kv("url.scheme", "http"),
					kvInt("http.response.status_code", httpStatus),
					kv("network.protocol.version", "1.1"),
				},
				Events: rootEvents,
				Status: rootStatus,
			},
			// The child is where the time actually goes. It is a CLIENT span, so a rule
			// that looks at root or SERVER spans only still sees the full `ms`.
			&tracepb.Span{
				TraceId:           traceID,
				SpanId:            childID,
				ParentSpanId:      rootID,
				Name:              "SELECT " + r.table,
				Kind:              tracepb.Span_SPAN_KIND_CLIENT,
				StartTimeUnixNano: uint64(start.Add(dur / 10).UnixNano()),
				EndTimeUnixNano:   uint64(end.Add(-dur / 10).UnixNano()),
				Attributes: []*commonpb.KeyValue{
					kv("db.system", "postgresql"),
					kv("db.name", "shop"),
					kv("db.operation", "SELECT"),
					kv("db.sql.table", r.table),
					kv("db.statement", "SELECT * FROM "+r.table+" WHERE tenant_id = $1"),
					kv("server.address", "shop-db-01"),
					kvInt("server.port", 5432),
				},
				Events: childEvents,
				Status: childStatus,
			},
		)
	}
	return out
}

// hostAttrs builds a host resource (type derived from host.id), grouped by host.name.
func hostAttrs(cfg config, c container, _ string, startedOn int64) []*commonpb.KeyValue {
	// For host-level metrics the entity IS the host, so each gets its own host.name and -n is
	// the host count (the -hosts bucket does not apply).
	host := c.name(cfg.prefix, "host")
	attrs := []*commonpb.KeyValue{
		kv("host.id", host),
		kv("host.name", host),
		kv("host.ip", "[\"Interface: eth0, IP: 10.0.0.1\"]"),
	}
	for k, v := range hostTemplate {
		attrs = append(attrs, kv(k, v))
	}
	return attrs
}

// clusterFor spreads k8s entities across cfg.clusters synthetic clusters. The k8s rules
// group by k8s.cluster.name alongside the pod/container name, so more clusters means more
// distinct group keys inside the one notification.
func clusterFor(cfg config, c container) string {
	if cfg.clusters <= 1 {
		return cfg.prefix + "-cluster"
	}
	return fmt.Sprintf("%s-cluster-%02d", cfg.prefix, (c.idx%cfg.clusters)+1)
}

// nodeFor keeps a node inside exactly one cluster — a node shared between clusters would be
// a shape the real pipeline never sees.
func nodeFor(cfg config, c container) string {
	return clusterFor(cfg, c) + "-node-01"
}

func resourceFor(cfg config, c container, startedOn int64) *resourcepb.Resource {
	// The parent bucket each entity belongs to (a host for containers, a namespace for pods),
	// spread across cfg.hosts.
	// For the trace profiles the bucket is the machine the service runs on, so "host" again.
	bucketWord := "host"
	if cfg.signal == "k8s.pod" || cfg.signal == "k8s.container" {
		bucketWord = "ns"
	}
	host := fmt.Sprintf("%s-%s-01", cfg.prefix, bucketWord)
	if cfg.hosts > 1 {
		// Divide out the cluster assignment first, otherwise idx%clusters and idx%hosts
		// move together and the cross-product collapses onto a diagonal (cluster 1 only
		// ever pairs with namespace 1, and so on).
		idx := c.idx
		if cfg.clusters > 1 {
			idx = c.idx / cfg.clusters
		}
		host = fmt.Sprintf("%s-%s-%02d", cfg.prefix, bucketWord, (idx%cfg.hosts)+1)
	}
	return &resourcepb.Resource{Attributes: signalProfiles[cfg.signal].attrs(cfg, c, host, startedOn)}
}

func gauge(name string, ts time.Time, v float64, attrs ...*commonpb.KeyValue) *metricspb.Metric {
	return &metricspb.Metric{
		Name: name,
		Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{DataPoints: []*metricspb.NumberDataPoint{{
			// The agent stamps millisecond precision; keep the same grain.
			TimeUnixNano: uint64(ts.UnixMilli()) * 1e6,
			Value:        &metricspb.NumberDataPoint_AsDouble{AsDouble: v},
			Attributes:   attrs,
		}}}},
	}
}

type generator struct {
	cfg        config
	seq        int
	containers []container
	breachAt   int // first index of the breaching window
	churnAt    int // next index to retire
	startedOn  int64
	nextFlap   time.Time
	wave       []float64 // -sev-wave, the multiples of -warn-n the split walks through
	sevStep    int       // where in that wave this tick is
	nextSev    time.Time
}

func newGenerator(cfg config, now time.Time) *generator {
	g := &generator{cfg: cfg, startedOn: now.UnixMilli()}
	for i := 0; i < cfg.n; i++ {
		g.containers = append(g.containers, container{idx: i})
	}
	if cfg.flap > 0 {
		g.nextFlap = now.Add(cfg.flap)
	}
	if cfg.sevFlap > 0 {
		g.wave = parseWave(cfg.sevWave)
		g.nextSev = now.Add(cfg.sevFlap)
	}
	return g
}

// parseWave reads -sev-wave: multiples of -warn-n, one per step of the severity cycle.
func parseWave(spec string) []float64 {
	var out []float64
	for _, f := range strings.Split(spec, ",") {
		v, err := strconv.ParseFloat(strings.TrimSpace(f), 64)
		if err != nil || v < 0 {
			log.Fatalf("-sev-wave %q: %q is not a multiplier >= 0", spec, f)
		}
		out = append(out, v)
	}
	return out
}

// cohorts sizes the two breaching cohorts for this tick. Without -sev-flap they are just
// -breach and -warn-n. With it the WARNING cohort walks the wave while the TOTAL breaching
// count stays put, so members escalate and de-escalate — the split the UI shows changes,
// but nobody enters or leaves the group and it stays one notification throughout.
func (g *generator) cohorts() (crit, warn int) {
	if g.cfg.sevFlap <= 0 {
		return g.cfg.breach, g.cfg.warnN
	}
	total := g.cfg.breach + g.cfg.warnN
	warn = int(math.Round(float64(g.cfg.warnN) * g.wave[g.sevStep]))
	warn = min(max(warn, 0), total)
	return total - warn, warn
}

// The three tiers an entity can be in on a given tick. A rule with two thresholds reports
// the middle one as WARNING and the top one as CRITICAL, so a run that wants both severities
// in one group sends some entities the -bad value and some the -warn value.
const (
	sevHealthy = iota
	sevWarning
	sevCritical
)

// severity places entity i in one of the three tiers. The critical and warning cohorts are
// adjacent windows starting at breachAt, so they rotate together on flap and a warning
// entity becomes a healthy one rather than being reshuffled into the critical cohort.
func (g *generator) severity(i, crit, warn int) int {
	pos := (i - g.breachAt + g.cfg.n) % g.cfg.n
	switch {
	case pos < crit:
		return sevCritical
	case pos < crit+warn:
		return sevWarning
	}
	return sevHealthy
}

// metricsFor builds the datapoints for one entity: the profile's builder, or a single gauge.
func (g *generator) metricsFor(c container, v float64, now time.Time) []*metricspb.Metric {
	if p := signalProfiles[g.cfg.signal]; p.metrics != nil {
		return p.metrics(g.cfg, c, g.cfg.metric, v, g.seq, now)
	}
	return []*metricspb.Metric{gauge(g.cfg.metric, now, v)}
}

// batch is one interval's worth of telemetry. A metric profile fills in metrics, a trace
// profile fills in traces; exactly one is ever set. Both are per-resource lists, so the
// chunking in send and the counts in the log line read the same either way.
type batch struct {
	metrics *collectormetrics.ExportMetricsServiceRequest
	traces  *collectortrace.ExportTraceServiceRequest
	bad     int // entities sent the critical value
	warn    int // entities sent the warning value
}

func (b *batch) resources() int {
	if b.traces != nil {
		return len(b.traces.ResourceSpans)
	}
	return len(b.metrics.ResourceMetrics)
}

func (b *batch) message() proto.Message {
	if b.traces != nil {
		return b.traces
	}
	return b.metrics
}

// tick advances the scenario clock: rotate the breaching window on flap, retire churn
// containers, then build the batch.
func (g *generator) tick(now time.Time, allOK bool) *batch {
	g.seq++
	if g.cfg.flap > 0 && !now.Before(g.nextFlap) {
		g.breachAt = (g.breachAt + g.cfg.breach + g.cfg.warnN) % g.cfg.n
		g.nextFlap = now.Add(g.cfg.flap)
	}
	if g.cfg.sevFlap > 0 && !now.Before(g.nextSev) {
		g.sevStep = (g.sevStep + 1) % len(g.wave)
		g.nextSev = now.Add(g.cfg.sevFlap)
	}
	for k := 0; k < g.cfg.churn && g.cfg.n > 0; k++ {
		g.containers[g.churnAt].gen++
		g.churnAt = (g.churnAt + 1) % g.cfg.n
	}
	profile := signalProfiles[g.cfg.signal]
	b := &batch{}
	if profile.spans != nil {
		b.traces = &collectortrace.ExportTraceServiceRequest{}
	} else {
		b.metrics = &collectormetrics.ExportMetricsServiceRequest{}
	}
	crit, warn := g.cohorts()
	for i, c := range g.containers {
		v := g.cfg.ok
		if !allOK {
			switch g.severity(i, crit, warn) {
			case sevCritical:
				v = g.cfg.bad
				b.bad++
			case sevWarning:
				v = g.cfg.warn
				b.warn++
			}
		}
		res := resourceFor(g.cfg, c, g.startedOn)
		if profile.spans != nil {
			b.traces.ResourceSpans = append(b.traces.ResourceSpans, &tracepb.ResourceSpans{
				Resource: res,
				ScopeSpans: []*tracepb.ScopeSpans{{
					Scope: &commonpb.InstrumentationScope{Name: "telemetrygen"},
					Spans: profile.spans(g.cfg, c, v, g.seq, now),
				}},
			})
			continue
		}
		b.metrics.ResourceMetrics = append(b.metrics.ResourceMetrics, &metricspb.ResourceMetrics{
			Resource: res,
			ScopeMetrics: []*metricspb.ScopeMetrics{{
				Scope:   &commonpb.InstrumentationScope{Name: "telemetrygen"},
				Metrics: g.metricsFor(c, v, now),
			}},
		})
	}
	return b
}

// clients holds both OTLP services on the one connection; a batch goes to whichever one
// its signal produced.
type clients struct {
	metrics collectormetrics.MetricsServiceClient
	traces  collectortrace.TraceServiceClient
}

// send splits a batch into requests of at most 1000 resources — well under gRPC's 4 MB
// default — so -n 50000 is one tick, not one giant message. A trace resource carries
// cfg.spans*2 spans rather than a handful of datapoints, so its chunk is sized to land on
// roughly the same 2000 sub-records instead.
func send(ctx context.Context, cl clients, cfg config, b *batch) (int, time.Duration, error) {
	ctx = metadata.AppendToOutgoingContext(ctx, cfg.authHeader, cfg.apiKey)
	bytes := 0
	start := time.Now()
	if b.traces != nil {
		per := max(1, 2000/max(1, cfg.spans*2))
		all := b.traces.ResourceSpans
		for len(all) > 0 {
			n := min(per, len(all))
			chunk := &collectortrace.ExportTraceServiceRequest{ResourceSpans: all[:n]}
			all = all[n:]
			bytes += proto.Size(chunk)
			if _, err := cl.traces.Export(ctx, chunk); err != nil {
				return bytes, time.Since(start), err
			}
		}
		return bytes, time.Since(start), nil
	}
	all := b.metrics.ResourceMetrics
	// A resource is one datapoint on most profiles and about ninety on host.full, so chunk
	// on datapoints rather than resources: 1000 host.full resources in one request is a
	// message gRPC will refuse to send.
	per := 1000
	if len(all) > 0 {
		if dp := len(all[0].ScopeMetrics[0].Metrics); dp > 2 {
			per = max(1, 2000/dp)
		}
	}
	for len(all) > 0 {
		n := min(per, len(all))
		chunk := &collectormetrics.ExportMetricsServiceRequest{ResourceMetrics: all[:n]}
		all = all[n:]
		bytes += proto.Size(chunk)
		if _, err := cl.metrics.Export(ctx, chunk); err != nil {
			return bytes, time.Since(start), err
		}
	}
	return bytes, time.Since(start), nil
}

// normalizeEndpoint turns "https://host:443", "http://host:4321" or "host:4321" into a bare
// host:port plus whether TLS is implied. A bare host:443 is assumed to be TLS too, since no
// ingest gateway serves plaintext gRPC there.
func normalizeEndpoint(ep string) (string, bool) {
	switch {
	case strings.HasPrefix(ep, "https://"):
		return defaultPort(strings.TrimSuffix(strings.TrimPrefix(ep, "https://"), "/"), "443"), true
	case strings.HasPrefix(ep, "http://"):
		return strings.TrimSuffix(strings.TrimPrefix(ep, "http://"), "/"), false
	}
	_, port, err := net.SplitHostPort(ep)
	return ep, err == nil && port == "443"
}

func defaultPort(hostport, port string) string {
	if _, _, err := net.SplitHostPort(hostport); err != nil {
		return net.JoinHostPort(hostport, port)
	}
	return hostport
}

func main() {
	var cfg config
	flag.StringVar(&cfg.endpoint, "endpoint", "127.0.0.1:4321", "OTLP gRPC endpoint; accepts host:port or a https://host:port URL")
	flag.StringVar(&cfg.apiKey, "api-key", os.Getenv("MW_API_KEY"), "project API key (or MW_API_KEY)")
	flag.StringVar(&cfg.authHeader, "auth-header", "authorization", "gRPC metadata key carrying the API key")
	flag.StringVar(&cfg.signal, "signal", "container", "what to synthesise: container | k8s.pod | host.memory | host.cpu | host.full | k8s.node | k8s.container | trace.service | trace.error")
	flag.StringVar(&cfg.metric, "metric", "container.cpu.utilization", "metric name to emit")
	flag.StringVar(&cfg.prefix, "prefix", "synth", "name prefix for containers and hosts")
	flag.StringVar(&cfg.hostName, "host-name", "synth-host-01", "host.name when -hosts is 1")
	flag.IntVar(&cfg.n, "n", 500, "number of synthetic containers (groups)")
	flag.IntVar(&cfg.breach, "breach", 0, "how many of them emit the -bad value (the CRITICAL cohort)")
	flag.IntVar(&cfg.warnN, "warn-n", 0, "how many of them emit the -warn value instead (the WARNING cohort, taken from the entities after the -breach ones)")
	flag.IntVar(&cfg.hosts, "hosts", 1, "spread entities across this many synthetic hosts (namespaces, for k8s.pod / k8s.container)")
	flag.IntVar(&cfg.clusters, "clusters", 1, "spread k8s entities across this many synthetic clusters")
	flag.IntVar(&cfg.spans, "spans", 4, "trace signals only: requests each service serves per interval (each is a root span plus a child)")
	flag.IntVar(&cfg.churn, "churn", 0, "retire and re-mint this many containers every interval (new group names)")
	flag.Float64Var(&cfg.ok, "ok", 2, "value for healthy containers (percent; container.cpu.utilization is already percent on the wire)")
	flag.Float64Var(&cfg.bad, "bad", 95, "value for breaching containers (percent)")
	// NaN rather than a magic number: -ok and -bad detect "not overridden" by comparing
	// against their own defaults, which is why `-ok 2` and `-bad 95` are silently replaced
	// by the profile's values. NaN cannot be typed on a command line, so -warn has no such
	// value to collide with.
	flag.Float64Var(&cfg.warn, "warn", math.NaN(), "value for the warning cohort (default NaN = use the signal profile's own warning value)")
	flag.DurationVar(&cfg.interval, "interval", 10*time.Second, "time between batches")
	flag.DurationVar(&cfg.duration, "duration", 0, "stop after this long (0 = until Ctrl-C)")
	flag.DurationVar(&cfg.flap, "flap", 0, "rotate which containers breach every this often (0 = never)")
	flag.DurationVar(&cfg.sevFlap, "sev-flap", 0, "move the critical/warning split every this often, keeping the same entities breaching (0 = a fixed split)")
	flag.StringVar(&cfg.sevWave, "sev-wave", "1,0.5,1,1.5", "-sev-flap cycle, as multiples of -warn-n: the warning cohort walks these and the critical one takes the rest")
	flag.BoolVar(&cfg.recoverOnExit, "recover-on-exit", true, "send a final all-healthy batch on exit so groups resolve")
	flag.BoolVar(&cfg.tls, "tls", false, "force TLS (auto-enabled for https:// endpoints and port 443)")
	flag.BoolVar(&cfg.plaintext, "plaintext", false, "force plaintext, overriding the TLS auto-detection")
	flag.BoolVar(&cfg.dryRun, "dry-run", false, "print the first batch as JSON and exit without sending")
	flag.Parse()

	profile, ok := signalProfiles[cfg.signal]
	if !ok {
		names := make([]string, 0, len(signalProfiles))
		for n := range signalProfiles {
			names = append(names, n)
		}
		log.Fatalf("unsupported -signal %q (have: %s)", cfg.signal, strings.Join(names, ", "))
	}
	// Fall back to the profile's metric/values when the user did not override the container
	// defaults, so `-signal k8s.pod` just works without also passing -metric/-ok/-bad.
	if cfg.metric == "container.cpu.utilization" {
		cfg.metric = profile.metric
	}
	if cfg.ok == 2 {
		cfg.ok = profile.ok
	}
	if cfg.bad == 95 {
		cfg.bad = profile.bad
	}
	if math.IsNaN(cfg.warn) {
		cfg.warn = profile.warn
	}
	if cfg.n <= 0 {
		log.Fatal("-n must be > 0")
	}
	if cfg.breach > cfg.n {
		log.Fatalf("-breach %d exceeds -n %d", cfg.breach, cfg.n)
	}
	if cfg.warnN < 0 {
		log.Fatal("-warn-n must be >= 0")
	}
	if cfg.breach+cfg.warnN > cfg.n {
		log.Fatalf("-breach %d + -warn-n %d exceeds -n %d", cfg.breach, cfg.warnN, cfg.n)
	}
	if cfg.sevFlap > 0 {
		if cfg.warnN <= 0 {
			log.Fatal("-sev-flap moves the critical/warning split, so it needs a warning cohort: pass -warn-n")
		}
		// A rule aggregates over its window, so a split that moves faster than the window
		// leaves every member averaging the two values and reading as neither severity.
		if cfg.sevFlap < 5*time.Minute {
			log.Printf("warning: -sev-flap %s is shorter than a typical 5m rule window; members will average the two values instead of settling on a severity", cfg.sevFlap)
		}
	}
	if cfg.warnN > 0 {
		// Shipping a "warning" cohort at a value that is really critical (or really
		// healthy) is worse than not having one: the group looks right in the UI and the
		// severities are wrong.
		if math.IsNaN(cfg.warn) {
			log.Fatalf("-signal %s has no warning value (%s is an enum with no middle state); pass -warn explicitly or drop -warn-n", cfg.signal, cfg.metric)
		}
		lo, hi := math.Min(cfg.ok, cfg.bad), math.Max(cfg.ok, cfg.bad)
		if cfg.warn <= lo || cfg.warn >= hi {
			log.Printf("warning: -warn %g is not between -ok %g and -bad %g; the warning cohort will read as %s", cfg.warn, cfg.ok, cfg.bad,
				map[bool]string{true: "critical", false: "healthy"}[cfg.warn >= hi])
		}
	}
	if cfg.spans <= 0 {
		log.Fatal("-spans must be > 0")
	}
	// -ok / -bad are a share of requests here, and 0.85 meaning "0.85% of requests fail" is
	// a mistake that looks like a working run until nothing ever breaches.
	if cfg.signal == "trace.error" {
		if cfg.ok < 0 || cfg.ok > 100 || cfg.bad < 0 || cfg.bad > 100 || cfg.warn < 0 || cfg.warn > 100 {
			log.Fatalf("-signal trace.error takes PERCENTAGES: -ok %g -warn %g -bad %g is outside 0..100", cfg.ok, cfg.warn, cfg.bad)
		}
		if cfg.bad > 0 && cfg.bad < 1 {
			log.Printf("warning: -bad %g means %g%% of requests fail; these are percentages, not fractions", cfg.bad, cfg.bad)
		}
	}
	g := newGenerator(cfg, time.Now())

	if cfg.dryRun {
		b := g.tick(time.Now(), false)
		out, _ := protojson.MarshalOptions{Multiline: true}.Marshal(b.message())
		fmt.Println(string(out))
		log.Printf("dry run: %d resources, %d critical, %d warning, %d bytes", b.resources(), b.bad, b.warn, proto.Size(b.message()))
		return
	}
	if cfg.apiKey == "" {
		log.Fatal("no API key: pass -api-key or set MW_API_KEY")
	}

	// A remote ingest endpoint is a URL on :443; the local capture-metrics is plain host:port.
	// Strip any scheme (grpc.NewClient wants host:port) and turn TLS on when the endpoint says
	// so, unless -tls / -plaintext overrides it.
	target, useTLS := normalizeEndpoint(cfg.endpoint)
	switch {
	case cfg.plaintext:
		useTLS = false
	case cfg.tls:
		useTLS = true
	}
	creds := insecure.NewCredentials()
	if useTLS {
		host := target
		if h, _, err := net.SplitHostPort(target); err == nil {
			host = h
		}
		creds = credentials.NewTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
	}
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(creds))
	if err != nil {
		log.Fatalf("grpc: %v", err)
	}
	defer conn.Close()
	cl := clients{
		metrics: collectormetrics.NewMetricsServiceClient(conn),
		traces:  collectortrace.NewTraceServiceClient(conn),
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if cfg.duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cfg.duration)
		defer cancel()
	}

	fields := map[string]any{"n": cfg.n, "breach": cfg.breach, "warn-n": cfg.warnN, "sev-flap": cfg.sevFlap.String(), "hosts": cfg.hosts, "clusters": cfg.clusters, "churn": cfg.churn, "flap": cfg.flap.String(), "interval": cfg.interval.String(), "metric": cfg.metric, "prefix": cfg.prefix}
	if profile.spans != nil {
		fields["spans"] = cfg.spans
	}
	summary, _ := json.Marshal(fields)
	scheme := "plaintext"
	if useTLS {
		scheme = "tls"
	}
	log.Printf("start %s -> %s (%s)", summary, target, scheme)

	ticker := time.NewTicker(cfg.interval)
	defer ticker.Stop()
	emit := func(now time.Time, allOK bool) {
		b := g.tick(now, allOK)
		bytes, took, err := send(context.Background(), cl, cfg, b)
		status := "ok"
		if err != nil {
			status = "ERROR " + strings.TrimSpace(err.Error())
		}
		log.Printf("batch resources=%d critical=%d warning=%d bytes=%d took=%s %s", b.resources(), b.bad, b.warn, bytes, took.Round(time.Millisecond), status)
	}
	emit(time.Now(), false)
	for {
		select {
		case <-ctx.Done():
			if cfg.recoverOnExit {
				log.Print("exiting: sending a final all-healthy batch")
				emit(time.Now(), true)
			}
			return
		case now := <-ticker.C:
			emit(now, false)
		}
	}
}
