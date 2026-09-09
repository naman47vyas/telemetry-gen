# One script per alert

Each script drives `telemetrygen` to make one Middleware alert rule fire with a large
group of breaching resources — the "Multi Alert" case, where a single notification
stands for many resources. Every resource breaches, but not all at the same severity:
two thirds sit above the rule's critical threshold and the rest only above its warning
one, so a single notification carries both.

```sh
./high-cpu-usage-for-host.sh                # 200 hosts, 45 minutes
./high-cpu-usage-for-host.sh 500 20m        # 500 hosts, 20 minutes
./high-cpu-usage-for-host.sh --dry-run      # print the first batch, send nothing
./high-cpu-usage-for-host.sh --bg           # run detached, log to .multi-alert-runs/
```

`N` and `DURATION` may also be passed as environment variables.

`WARN_N` decides how many of the `N` are only a warning (`WARN_PCT` says the same thing as
a percentage), and `WARN` is the value that cohort emits. `OK` and `BAD` are env knobs too,
for tuning to a tenant whose thresholds sit somewhere else:

```sh
WARN_N=0 ./high-container-cpu.sh                # back to all-critical
WARN_N=150 ./high-container-cpu.sh 200          # 50 critical, 150 warning
WARN=88 ./high-container-cpu.sh                 # a different warning value
SEV_FLAP=10m ./high-container-cpu.sh            # make the split move (see below)
```

## One group, both severities

`mixed-severity-group.sh` is not there to make a rule fire — every other script does that.
It is there to look at how a *mixed* group renders: 20 critical and 20 warning hosts in one
notification, one history row, members listed criticals-first.

```sh
./mixed-severity-group.sh                       # 40 hosts: 20 critical, 20 warning
WARN_PCT=88 ./mixed-severity-group.sh           # 5 critical, 35 warning
SEV_WAVE=1,0,2 ./mixed-severity-group.sh        # mixed -> all critical -> all warning
```

Warning and critical members land in the same notification — the engine buckets both as
"firing" and the group's status is its worst member — so the interesting thing is what
happens *inside* one unchanging group. `SEV_FLAP` is what makes that visible: every
`SEV_FLAP` the split walks `SEV_WAVE` (`20c/20w -> 30c/10w -> 20c/20w -> 10c/30w` by
default) while the same entities keep breaching throughout, so members escalate and
de-escalate without anything joining or leaving the group. The banner prints the cycle as
actual counts before it sends anything.

**`SEV_FLAP` must be longer than the rule's evaluation window.** A member that spends three
minutes at 96% and three at 87% averages 91% over a 5-minute window and reads as neither
severity. The default is 10m against a 5m window, which leaves each step half a window of
settled state; the generator warns below five minutes. Every script here takes `SEV_FLAP`,
not just this one.

Two size limits shape what you see, which is why the default is 40 hosts split down the
middle: the collapsed row previews 10 members worst-first, so at 20/20 the preview is *all*
critical and the row claims 20 warnings it does not show you until you expand it. Expanded
pages hold 30. `WARN_PCT=88` gives 5 critical and 35 warning, which is the split that puts
both badges in the preview.

Three scripts are APM rather than infrastructure, so `N` is a number of *services* and
`SPANS` is how many requests each one serves per interval:

```sh
./latency-is-higher-than-expected.sh            # 200 services, every request ~4 s
./error-rate-is-high.sh                         # 200 services, 85% of requests fail
./errors-detected-in-traces.sh                  # 200 services, every request fails
SPANS=10 ./error-rate-is-high.sh                # 10 requests per service per interval
```

`OK`/`BAD`/`WARN` mean something different on each: **milliseconds** of request duration
for latency, **percent of requests that fail** for the two error scripts.

The three Kubernetes scripts spread their entities over several clusters, and the two pod
scripts over several namespaces too, so one notification carries group keys from across
the estate rather than from a single cluster:

```sh
CLUSTERS=4 NAMESPACES=6 ./pods-are-restarting.sh 240
```

Entities are dealt across the full cluster x namespace grid, not down a diagonal, and each
synthetic node belongs to exactly one cluster. Defaults are 3 clusters and 5 namespaces.

| Script | Rule it fires | Grouped by | Critical / warning |
|---|---|---|---|
| `high-cpu-usage-for-host.sh` | High CPU usage for host | `host.name` | 120% / 87% |
| `high-memory-usage-for-host.sh` | High memory usage for host | `host.name` | 95% / 87% |
| `high-cpu-utilization-for-node.sh` | High CPU utilization for node | cluster + node | 96% / 87% |
| `pods-are-failing.sh` | Pods are failing | cluster + pod | Failed / — |
| `pods-are-restarting.sh` | Pods are restarting (and CrashloopBackoff) | cluster + ns + pod + container | +3/tick / +0.05/tick |
| `high-container-cpu.sh` | High Container CPU | `host.name` + `container.name` | 96% / 87% |
| `container-is-not-running.sh` | Container is not running | `container.name` | not running / — |
| `latency-is-higher-than-expected.sh` | Latency is higher than expected | `service.name` | 4 s / 0.9 s |
| `error-rate-is-high.sh` | Error rate is high | `service.name` | 85% / 12% |
| `errors-detected-in-traces.sh` | Errors detected in traces | `service.name` | 100% / 15% |
| `mixed-severity-group.sh` | High CPU usage for host (severity mix, on the move) | `host.name` | 96% / 87% |
| `full-host-metrics.sh` | High CPU usage for host (or nothing, with `HEALTHY=1`) | `host.name` | 96% / 87% |
| `full-host-metrics-wave.sh` | High CPU usage for host, firing and resolving on a cycle | `host.name` | 96% / 87% / 5% |

The two enum rules have no warning column: `k8s.pod.phase` and `container.status` have no
value between Running and Failed, so every member of those groups is critical.
`mixed-severity-group.sh` fires the same rule as `high-cpu-usage-for-host.sh`; run one or
the other, not both at once, or you will be looking at two cohorts in one notification.

## Hosts with something behind them

Every other script emits the one series its rule reads, so the alert fires and the host page
behind it is empty. `full-host-metrics.sh` emits all 29 metrics the OpenTelemetry
hostmetrics receiver collects by default — cpu, memory, disk, filesystem, load, network,
paging, processes, about 100 datapoints per host per interval — with the receiver's own
names, units, instrument types and attribute values.

```sh
./full-host-metrics.sh                  # 25 hosts, 17 critical + 8 warning on CPU
HEALTHY=1 ./full-host-metrics.sh        # 25 hosts that just exist, firing nothing
HEALTHY=1 ./full-host-metrics.sh 60 4h  # a 60-host fleet for the afternoon
```

`HEALTHY=1` works on any script here: nothing breaches, everything sits at `OK`.

`OK`/`WARN`/`BAD` are the CPU busy fraction, so the cohorts drive the host CPU rule exactly
as `high-cpu-usage-for-host.sh` does. Everything else is plausible rather than tiered:
memory lands at 54-71% per host, deliberately under the 85% warning floor, so a run does not
also trip the memory rule. The counters are genuine cumulative sums climbing from the start
of the run, so rate charts need a couple of intervals before they show anything.

One host here is ~100 datapoints where a host elsewhere is one, so `N` costs a hundred times
as much — 25 hosts is 2500 datapoints an interval, 200 hosts is 20000. The sender chunks on
datapoints rather than resources so requests stay under gRPC's limit either way.

## An alert's whole life, on a cycle

`full-host-metrics-wave.sh` is `full-host-metrics.sh` with the fleet walking a cycle instead
of holding one level. Every `SEV_FLAP` all 25 hosts move to the next severity:

```
critical -> warning -> healthy -> warning -> (round again)
```

so the rule fires critical, de-escalates to warning, **resolves**, then climbs back and
fires again. That healthy step is the whole difference from `mixed-severity-group.sh`, whose
wave only rearranges severities inside a group that stays open. Use this one for
notification history, resolve behaviour, cooldown and re-arm.

```sh
./full-host-metrics-wave.sh                          # 25 hosts, 40m cycle, 3 times round
TIER_WAVE=crit,ok ./full-host-metrics-wave.sh        # just fire and resolve
TIER_PHASES=4 ./full-host-metrics-wave.sh            # always mixed, never resolves
```

The hosts are the same full-fidelity machines, so the host pages stay populated through
every step, healthy ones included.

**Each step has to outlast the rule's window.** At anything under 5 minutes the window
straddles two steps, every host averages 96% and 5%, and nothing reads as anything.
`SEV_FLAP` defaults to 10m, so the cycle is 40 minutes and `DURATION` defaults to three of
them. Expect the UI to lag each step by up to a window — the resolve does not land the
moment the values drop.

`TIER_PHASES=4` splits the fleet into four groups entering the cycle at different points, so
some hosts are critical, some warning and some healthy at any moment while each still walks
the whole cycle. The trade is that the aggregate counts barely move and the alert never
resolves, because somebody is always breaching. Lockstep is the one that shows a close.

## Credentials — i.e. which project a run lands in

These two decide the destination. Both come from the Middleware UI of the project you want
to hit: the key from Settings → API Keys (the *project* key), the endpoint from the agent
install screen — your tenant's own URL on :443, the value the agent docs call `MW_TARGET`.

```sh
MW_API_KEY=…
MW_OTLP_ENDPOINT=https://<tenant>.middleware.io:443
```

Keep one file per project at the repo root. `.env.beta` is the default; `MW_ENV_FILE`
picks another, and everything matching `.env.*` is gitignored:

```sh
cp .env.beta.example ../../.env.prod   # then fill in that project's key + endpoint
MW_ENV_FILE=.env.prod ./pods-are-failing.sh
```

A one-off without a file at all — an exported value always beats the file:

```sh
MW_API_KEY=… MW_OTLP_ENDPOINT=https://other.middleware.io:443 ./pods-are-failing.sh
```

Every script prints the file it used on the `creds:` line of its banner, next to the
`endpoint:` it is about to send to. Read those two lines before you let a run go — they are
the only warning you get that 200 breaching hosts are heading for the wrong project.

The endpoint accepts `https://host:443` or a bare `host:port`. TLS turns on automatically
for `https://` and for port 443; the local `127.0.0.1:4321` capture stays plaintext.

## Things that will waste your afternoon if you forget them

**Check the rule is unmuted and has Notification enabled.** A muted rule shows the
breaching series on its chart and writes nothing to the history table. This looks exactly
like a data problem and is not one.

**Three of these are traces, not metrics.** Latency and errors both read the APM spans
table — a service's latency is how long its requests took, its error rate is how many of
them failed — so there is no gauge to pin. Those three scripts emit root SERVER spans, each
with a database child, and their `OK`/`BAD` are **milliseconds** (latency) or **percent of
requests that fail** (both error scripts). Everything else in this directory emits metrics.

**"Errors detected in traces" also fires "Error rate is high".** They read the same table,
and a service where every request fails is breaching both. Run `error-rate-is-high.sh` when
you want only the rate one.

**A value only gets a badge if it lands in the right band.** On the utilization rules the
bands are known — **warning is 85-90, critical is above 90** — which is why those scripts
warn at 87% and go critical at 95% or more. 82% would have been *healthy*, not a warning.
The APM and counter values (0.9 s, 12%, 15%, +0.05 restarts/tick) are still estimates: those
rules are on different scales and nothing in the telemetry says where their thresholds sit.
If a cohort shows up at the wrong severity, `WARN=` is the fix, not an edit. `BAD` is
deliberately far past critical, so it is not a judgement call. On a counter rule the warning cohort is a *slower ramp*, not a lower number: the rule
diffs over its window, so what makes a pod merely "restarting" rather than crashlooping is
restarting less often. The generator refuses a warning cohort on a signal with no middle
value, and warns when `WARN` is not between `OK` and `BAD` — a cohort that silently reads
as critical is worse than no cohort at all.

**Values are not all the same scale.** `system.cpu.utilization` and
`system.memory.utilization` are *fractions* (0.95 = 95%). `container.cpu.utilization` is
already a percent. `k8s.pod.phase` and `container.status` are enums. Each script says
which in its header and in the `note:` line it prints.

**A formula rule needs every input series.** "High CPU usage for host" is
`user + steal + wait + system`. A host missing any one of those produces no row at all —
not a zero — so it is never evaluated and never fires. That is why the host CPU script
emits all four states.

**A counter rule needs the value to climb.** "Pods are restarting" diffs
`k8s.container.restarts` over its window, so a flat high number never breaches. The
script ramps it every tick.

**An APM rule needs requests, not just bad ones.** A service that emits nothing is not a
healthy service, it is an absent one, and the rule has no row to evaluate. All three APM
scripts keep every service sending `SPANS` requests per interval throughout the run, and on
exit switch them to healthy ones — 45 ms, or a 0% error rate — rather than stopping. That is
what makes the alert resolve instead of going stale. The warning cohort keeps serving too:
at 12% of 6 requests an interval it fails no more than one request per tick, which is exact
over the rule's window but lumpy inside a single one.

**An error percentage is a percentage.** `BAD=0.85` on the error scripts means 0.85% of
requests fail, which over a 5 minute window at 6 requests an interval is zero of them. The
generator refuses a value outside 0–100 and warns on anything under 1. Failures are dealt
out on a running index rather than rounded within each tick, so a small percentage still
comes out exact; a request fails at its root *and* in its database child, so the share of
spans that are errors reads the same whether the rule counts every span or only the SERVER
ones.

**Breach for longer than the rule's window.** The window is on the rule
(5 minutes for the host rules, longer for restarts). The defaults here are comfortably
above it; if you shorten `DURATION`, keep it above window + a couple of minutes.

**Let it exit cleanly.** Ctrl-C, or `kill -INT` for a `--bg` run, sends a final
all-healthy batch so the alert resolves. Killing with `-9` leaves the alert stuck
breaching until the data ages out.

## Confirming it actually grouped

1. Note the `prefix:` the script prints — every entity it creates is named `<prefix>-…`.
2. Open the rule, confirm the chart shows the cohort above the threshold.
3. Wait out the rule's window, then look at the history table: one row, "> 1 resource",
   expanding to the members. A brand-new cohort may need an entity sync before the query
   runtime resolves it — that is an operator step, not something these scripts control.
4. On exit, the same group should resolve.
