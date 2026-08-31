# One script per alert

Each script drives `telemetrygen` to make one Middleware alert rule fire with a large
group of breaching resources — the "Multi Alert" case, where a single notification
stands for many resources.

```sh
./high-cpu-usage-for-host.sh                # 200 hosts, 45 minutes
./high-cpu-usage-for-host.sh 500 20m        # 500 hosts, 20 minutes
./high-cpu-usage-for-host.sh --dry-run      # print the first batch, send nothing
./high-cpu-usage-for-host.sh --bg           # run detached, log to .multi-alert-runs/
```

`N` and `DURATION` may also be passed as environment variables.

| Script | Rule it fires | Grouped by |
|---|---|---|
| `high-cpu-usage-for-host.sh` | High CPU usage for host | `host.name` |
| `high-memory-usage-for-host.sh` | High memory usage for host | `host.name` |
| `high-cpu-utilization-for-node.sh` | High CPU utilization for node | cluster + node |
| `pods-are-failing.sh` | Pods are failing | cluster + pod |
| `pods-are-restarting.sh` | Pods are restarting (and CrashloopBackoff) | cluster + ns + pod + container |
| `high-container-cpu.sh` | High Container CPU | `host.name` + `container.name` |
| `container-is-not-running.sh` | Container is not running | `container.name` |

## Credentials

Put these in `../../.env.beta` (gitignored), or export them:

```sh
MW_API_KEY=…
MW_OTLP_ENDPOINT=https://<tenant>.middleware.io:443
```

The endpoint accepts `https://host:443` or a bare `host:port`. TLS turns on automatically
for `https://` and for port 443; the local `127.0.0.1:4321` capture stays plaintext.

## Things that will waste your afternoon if you forget them

**Check the rule is unmuted and has Notification enabled.** A muted rule shows the
breaching series on its chart and writes nothing to the history table. This looks exactly
like a data problem and is not one.

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
