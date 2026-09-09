# telemetrygen

Synthetic OTLP telemetry for exercising the alert pipeline at any cardinality, without real
workloads. Emits one resource per synthetic container straight into `capture-metrics`
(gRPC, `127.0.0.1:4321`), through the real Kafka → ClickHouse → entity-index path.

Mostly metrics. Two signals (`trace.service`, `trace.error`) emit spans instead, for the APM
latency and error rules, over the same connection to the OTLP traces service.

```
go build -o telemetrygen .
MW_API_KEY=… ./telemetrygen -n 500 -breach 37
```

| flag | default | meaning |
|---|---|---|
| `-n` | 500 | synthetic containers (= alert groups) |
| `-breach` | 0 | how many emit `-bad` instead of `-ok` — the CRITICAL cohort |
| `-warn-n` | 0 | how many emit `-warn` instead — the WARNING cohort, taken from the entities after the `-breach` ones |
| `-ok` / `-bad` | 2 / 95 | the two values, in percent — this metric is already percent on the wire |
| `-warn` | signal's own | value for the warning cohort: past the rule's warning threshold, short of its critical one |
| `-interval` | 10s | batch cadence |
| `-duration` | 0 | stop after; 0 = Ctrl-C |
| `-flap` | 0 | rotate the breaching window every so often (cooldown / re-arm tests) |
| `-sev-flap` | 0 | move the critical/warning split every so often, same entities breaching throughout — members escalate and de-escalate inside one group |
| `-sev-wave` | 1,0.5,1,1.5 | the cycle `-sev-flap` walks, as multiples of `-warn-n` |
| `-tier-wave` | | the cycle as SEVERITIES the whole cohort walks instead (`crit,warn,ok,warn`) — this one goes healthy, so the alert resolves and fires again |
| `-tier-phases` | 1 | split the cohort into this many groups entering `-tier-wave` at different points (1 = lockstep) |
| `-churn` | 0 | retire + re-mint this many per interval — new `container.id` and name, i.e. brand-new groups (ephemeral-pod behaviour) |
| `-hosts` | 1 | spread containers over N synthetic hosts (`notify_by: ["host.name"]` tests) |
| `-prefix` | synth | names: `synth-container-0001`, `synth-host-01` |
| `-recover-on-exit` | true | final all-healthy batch so groups resolve |
| `-spans` | 4 | trace signals only: requests each service serves per interval |
| `-dry-run` | false | print the first batch as JSON, send nothing |

The resource carries the same 21 attributes the real mw-agent sends for a Docker container.
`type=container` is derived from `container.id` (bifrost `ResourceWrapper.go`), and
`uniq_cpu_core` defaults to 1 for hosts without CPU metrics, so the synthetic hosts need
nothing else. Raw `container.cpu.utilization` values on this stack are ~0.004–0.04 for idle
containers; the rule's modifier divides by cores (=1 here).


## Signals

Each `-signal` is a profile: metric name, healthy/breaching defaults, resource shape. It feeds
the corresponding default alert (all now `notify_by: []`, so they collapse to One Notification).

| `-signal` | metric(s) | resource / group-by | default alert |
|---|---|---|---|
| `container` | container.cpu.utilization | host.name + container.name | High CPU for container |
| `k8s.pod` | k8s.pod.phase (4=Failed) | cluster/namespace + pod | Pods are failing |
| `host.memory` | system.memory.utilization (state=used) | host.name | High memory usage for host |
| `host.cpu` | system.cpu.utilization (state=user) | host.name | High CPU usage for host |
| `k8s.node` | k8s.node.cpu.utilization + allocatable_cpu | cluster + node | High CPU utilization for node |
| `k8s.container` | k8s.container.restarts (ramping, CrashLoopBackOff) | cluster/namespace/pod/container | Pods are restarting · CrashLoopBackOff |
| `host.full` | *every* hostmetrics metric — 29 of them, ~100 datapoints a host (cpu, memory, disk, filesystem, load, network, paging, processes) | host.name | High CPU usage for host |
| `trace.service` | *spans*, not a metric: root SERVER span + slow DB child, `-ok`/`-bad` in **ms** | service.name | Latency is higher than expected |
| `trace.error` | *spans*: the same traffic with 5xx + exception failures mixed in, `-ok`/`-bad` in **percent of requests that fail** | service.name | Error rate is high · Errors detected in traces |

For host-level signals `-n` is the host/node count (each entity IS the host); the `-hosts` bucket
applies to container/pod signals (spread across hosts / namespaces). For `trace.service` `-n` is
the service count and `-hosts` is the machines they run on. Not covered here: cloud defaults
(EC2/RDS — AWS integration, not agent metrics) and the log-based defaults. Adding a metric =
adding a profile.

`host.full` is the one profile that is not about a single rule. Every other one emits the
series its alert reads, which leaves a host that fires an alert and has an empty host page
behind it; `host.full` emits the whole default set the OpenTelemetry hostmetrics receiver
collects, with the receiver's own names, instrument types, units and attribute values (see
`hostfull.go`). Its `-ok`/`-warn`/`-bad` are the CPU busy fraction, so the cohorts still
drive the host CPU rule; memory and everything else sit at plausible per-host levels that do
not follow the tiers. Counters are real cumulative sums that climb from the start of the run.

`trace.service` is the one profile that is not a gauge. There is no latency metric to pin: the
rule reads the spans table, so the profile emits `-spans` requests per service per interval, each
a root SERVER span `-bad` milliseconds long with a CLIENT child holding 80% of that time. The
duration jitters ±8%, so avg / p50 / p90 / p99 all land on the same number and it does not matter
which aggregation the rule uses. `trace.error` is the same traffic held at 150 ms with `-bad`%
of the requests failing — a 5xx root carrying an ERROR status and an `exception` event, over a
child that failed the same way — so an error run does not also trip the latency rule.

A rule with two thresholds reports the middle band as WARNING, so `-warn-n` splits the breaching
cohort in two and a single grouped notification carries both severities. Each profile ships a
warning value (87% for the utilization signals, whose warning band is 85-90 and whose critical
starts above 90; 900 ms for latency, 12% for errors); enum signals ship
none, because there is nothing between Running and Failed, and asking for a warning cohort there
is refused rather than silently sent as critical. On the counter signal the warning cohort is a
slower ramp, not a lower number.

`-sev-flap` makes that split move rather than hold still, which is how you look at an
escalation: the warning cohort walks `-sev-wave` while `-breach + -warn-n` stays put, so a
member's severity changes without it joining or leaving the group. Keep the period longer
than the rule's evaluation window — a member that alternates faster than the window just
averages the two values and reads as neither severity.

`-tier-wave` is the other cycle, and the two are mutually exclusive. Where `-sev-wave`
rearranges severities inside a group that stays open, `-tier-wave` walks the whole cohort
through named tiers — `crit,warn,ok,warn` — including HEALTHY, so the rule fires,
de-escalates, resolves and fires again. `-tier-phases` splits the fleet into groups entering
that cycle at different points, which trades the resolve for a group that always holds a mix.

## Scenarios

| goal | command |
|---|---|
| 500 groups, 37 breaching, one tick | `-n 500 -breach 37` |
| deploy-time burst: all cross at once | `-n 500 -breach 500` |
| flap → cooldown must hold | `-n 50 -breach 10 -flap 90s` |
| ephemeral groups re-firing | `-n 100 -breach 100 -churn 10` |
| per-host notify_by | `-n 200 -breach 200 -hosts 10` |
| hit the 1000-group cap | `-n 1500 -breach 1500` |
| 200 slow services (APM latency) | `-signal trace.service -n 200 -breach 200 -hosts 4` |
| 200 failing services (APM errors) | `-signal trace.error -n 200 -breach 200 -hosts 4` |
| one group, both severities | `-n 200 -breach 134 -warn-n 66` |
| members escalating and de-escalating | `-n 40 -breach 20 -warn-n 20 -sev-flap 10m` |
| hosts with a populated host page | `-signal host.full -n 25 -breach 0` |
| an alert firing, resolving and firing again | `-signal host.full -n 25 -breach 25 -sev-flap 10m -tier-wave crit,warn,ok,warn` |

Pair with the threshold trick (lower the rule's threshold) when you want breaches
without caring about absolute values.
