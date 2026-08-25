# telemetrygen

Synthetic OTLP metrics for exercising the alert pipeline at any cardinality, without real
workloads. Emits one resource per synthetic container straight into `capture-metrics`
(gRPC, `127.0.0.1:4321`), through the real Kafka → ClickHouse → entity-index path.

```
go build -o telemetrygen .
MW_API_KEY=… ./telemetrygen -n 500 -breach 37
```

| flag | default | meaning |
|---|---|---|
| `-n` | 500 | synthetic containers (= alert groups) |
| `-breach` | 0 | how many emit `-bad` instead of `-ok` |
| `-ok` / `-bad` | 2 / 95 | the two values, in percent — this metric is already percent on the wire |
| `-interval` | 10s | batch cadence |
| `-duration` | 0 | stop after; 0 = Ctrl-C |
| `-flap` | 0 | rotate the breaching window every so often (cooldown / re-arm tests) |
| `-churn` | 0 | retire + re-mint this many per interval — new `container.id` and name, i.e. brand-new groups (ephemeral-pod behaviour) |
| `-hosts` | 1 | spread containers over N synthetic hosts (`notify_by: ["host.name"]` tests) |
| `-prefix` | synth | names: `synth-container-0001`, `synth-host-01` |
| `-recover-on-exit` | true | final all-healthy batch so groups resolve |
| `-dry-run` | false | print the first batch as JSON, send nothing |

The resource carries the same 21 attributes the real mw-agent sends for a Docker container.
`type=container` is derived from `container.id` (bifrost `ResourceWrapper.go`), and
`uniq_cpu_core` defaults to 1 for hosts without CPU metrics, so the synthetic hosts need
nothing else. Raw `container.cpu.utilization` values on this stack are ~0.004–0.04 for idle
containers; the rule's modifier divides by cores (=1 here).


## Signals (metric-based defaults)

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

For host-level signals `-n` is the host/node count (each entity IS the host); the `-hosts` bucket
applies to container/pod signals (spread across hosts / namespaces). Not covered here: cloud
defaults (EC2/RDS — AWS integration, not agent metrics) and trace/log defaults (Latency, errors —
beta Quickwit, a different pipeline). Adding a metric = adding a profile.

## Scenarios

| goal | command |
|---|---|
| 500 groups, 37 breaching, one tick | `-n 500 -breach 37` |
| deploy-time burst: all cross at once | `-n 500 -breach 500` |
| flap → cooldown must hold | `-n 50 -breach 10 -flap 90s` |
| ephemeral groups re-firing | `-n 100 -breach 100 -churn 10` |
| per-host notify_by | `-n 200 -breach 200 -hosts 10` |
| hit the 1000-group cap | `-n 1500 -breach 1500` |

Pair with the threshold trick (lower the rule's threshold) when you want breaches
without caring about absolute values.
