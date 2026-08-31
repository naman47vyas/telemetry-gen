#!/usr/bin/env bash
# =============================================================================
# multi-alert-scenarios.sh — drive telemetrygen to exercise Middleware's
# notification-grouping ("Multi Alert") feature end to end.
#
#   ./scripts/multi-alert-scenarios.sh <scenario> [--bg] [--duration 15m]
#   ./scripts/multi-alert-scenarios.sh stop            # recover + clean up bg runs
#   ./scripts/multi-alert-scenarios.sh dry-run <scenario>
#
# A grouped default alert (group-by host/pod/node/container) evaluates its
# threshold per resource but sends ONE notification per rule evaluation listing
# every breaching resource. Every scenario here shapes the per-resource metric
# data so that notification is a multi-member group, a singleton, an over-cap
# group, a mixed-severity set, or a re-forming (flapping) set.
#
# -----------------------------------------------------------------------------
# HOW TO VERIFY A RUN ACTUALLY GROUPED (operator steps, outside this script)
# -----------------------------------------------------------------------------
#  1. Note the prefix the script prints ("prefix: ma-bigg-1724...") — every
#     synthetic resource name starts with it, so it is easy to pick out.
#  2. Freshly-minted entity batches may NOT be visible to query-runtime until the
#     otel-data entity sync has run. If the rule history stays empty for longer
#     than (window + 2 evaluation cycles), trigger/wait for the entity sync and
#     check again. The script cannot do this for you.
#  3. Wait at least the rule window (10 min for host/node CPU, 5 min for memory/
#     pod phase, 30 min for restarts). These rules AVERAGE over the window and
#     backfill missing buckets with 0, so a breach only crosses the threshold
#     once most of the window is full of breaching samples.
#  4. Alerts -> Rules -> <rule name printed by the script> -> History.
#     - big_group / over_cap / multi_signal: ONE notification row whose member
#       count equals -breach ("40 resources", expandable). For over_cap the API
#       attaches only the top 10 members, so the row must show "+N more" and
#       the expanded view must page through all of them.
#     - singleton: ONE row, resource name + value rendered INLINE, no expander.
#     - mixed_severity: one row containing both Warning and Critical members
#       (the two cohorts share a prefix stem: <prefix>-warn-* / <prefix>-crit-*).
#     - flap: successive notifications whose member lists rotate every -flap.
#  5. After the run ends (or `stop`), telemetrygen sends one all-healthy batch;
#     the group should RESOLVE within one more window. Never leave a scenario
#     breaching: if a background run dies, run `stop` or re-run the scenario
#     with --duration 1m so the recovery batch goes out.
#
# -----------------------------------------------------------------------------
# VALUE / TIMING GOTCHAS (read before changing any number)
# -----------------------------------------------------------------------------
#  * telemetrygen's -ok/-bad are PER-PROFILE. Beware: main.go treats "-bad 95"
#    and "-ok 2" as "not overridden" and substitutes the profile default, so
#    passing -bad 95 to host.memory silently becomes 0.92. This script always
#    passes explicit values that are NOT 95/2 so what you see is what is sent.
#  * system.cpu.utilization (host.cpu) and system.memory.utilization
#    (host.memory) are FRACTIONS on the wire (the rule query does x100).
#    host.cpu is the FORMULA user+steal+wait+system (x100). A formula yields
#    NO row (not 0) for a host missing any input series, so telemetrygen
#    emits all four states (busy fraction on user, 0 on the rest); -bad 1.2
#    lands ~120%, comfortably over crit 90%. Verified 2026-08-26: emitting
#    only state=user never evaluated at all.
#    host.memory state=used: -bad 0.95 -> 95%.
#  * k8s.node: rule = cpu.utilization / allocatable_cpu * 100; telemetrygen
#    emits allocatable_cpu=100, so -bad is plain percent (95 -> 95%).
#  * container.cpu.utilization is already percent on the wire (95 -> 95%).
#  * k8s.pod.phase is a raw enum: 2=Running, 4=Failed (crit is > 3).
#  * k8s.container.restarts is a counter that RAMPS by -bad per tick; the rule
#    takes max/monotonic difference over 30 min, so any -bad >= 1 crosses.
#  * Windows: the rule averages the window with 0-backfill, so a breach must
#    persist for the whole window plus a buffer. MIN_DURATION below enforces
#    window + 2 min; the script refuses to run shorter.
# =============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
BIN="$REPO_DIR/telemetrygen"
RUN_DIR="${MA_RUN_DIR:-$REPO_DIR/.multi-alert-runs}"   # pids + logs for --bg
ENDPOINT="${MW_OTLP_ENDPOINT:-127.0.0.1:4321}"
INTERVAL="${MA_INTERVAL:-10s}"
KIND_NS="${MW_SECRET_NS:-mw-agent-ns}"
KIND_SECRET="${MW_SECRET_NAME:-middleware-secret}"
KIND_SECRET_KEY="${MW_SECRET_KEY:-rqpft}"

# ----------------------------------------------------------------------------- signals
# signal -> "alert name|healthy value|breach value|warn-only value|min duration"
# warn-only = a value between the Warning and Critical thresholds ("-" if the
# rule has no Warning tier or it is not reachable with a flat value).
declare -A SIG_ALERT SIG_OK SIG_BAD SIG_WARN SIG_MIN
sig() { SIG_ALERT[$1]=$2; SIG_OK[$1]=$3; SIG_BAD[$1]=$4; SIG_WARN[$1]=$5; SIG_MIN[$1]=$6; }
#   signal          alert name                        ok    bad   warn   min
sig host.cpu       "High CPU usage for host"          0.03  1.2   0.87   12m   # fraction; warn>85 crit>90
sig host.memory    "High memory usage for host"       0.20  0.95  0.87   7m    # fraction; warn>85 crit>90
sig k8s.node       "High CPU utilization for node"    10    96    85     12m   # percent;  warn>80 crit>90
sig k8s.pod        "Pods are failing"                 2     4     -      7m    # phase enum; crit>3 only
sig k8s.container  "Pods are restarting"              0     3     1      32m   # restarts/tick; warn>2 crit>4
sig container      "High Container CPU (custom)"      3     96    87     12m   # percent; assumes warn>85 crit>90
# NOTE k8s.container warn: restarts ramp by -bad per tick, so a "warn" cohort
# only stays under crit for a few ticks — mixed_severity therefore excludes it.

# ----------------------------------------------------------------------------- helpers
log()  { printf '\033[1;34m[%s]\033[0m %s\n' "$(date +%H:%M:%S)" "$*"; }
warn() { printf '\033[1;33m[%s] WARN\033[0m %s\n' "$(date +%H:%M:%S)" "$*" >&2; }
die()  { printf '\033[1;31mERROR:\033[0m %s\n' "$*" >&2; exit 1; }

usage() {
  cat <<USAGE
Usage: $(basename "$0") <scenario|command> [options]

Scenarios (each prints the rule to watch and the entity prefix it uses):
  big_group       one signal, all N breach          -> one large multi-member group
  singleton       exactly one resource breaches     -> inline singleton row
  over_cap        > 10 breaching members            -> "+N more" / paging
  mixed_severity  Warning + Critical cohorts in one rule evaluation
  multi_signal    host.cpu + host.memory + k8s.pod + k8s.container + k8s.node at once
  flap            rotate which resources breach     -> groups re-form / renotify
  all             big_group, singleton, over_cap, mixed_severity in parallel, then flap

Commands:
  dry-run <scenario>   print the first batch (JSON summary) for every run the
                       scenario would start, send nothing
  stop                 SIGINT every background run (each sends its healthy
                       recovery batch) and clean up pid files
  status               list background runs and their logs
  help

Options:
  --bg               run in the background, log to $RUN_DIR/<name>.log
  --duration D       override run length (refused if below the signal minimum)
  --signal S         big_group/singleton/over_cap/flap only: which signal to use
  --n N              override entity count
  --metric NAME      emit this metric instead of the signal's default, for custom
                     rules built on another metric of the same resource (e.g.
                     container.status for "Container is not running"). Pair it
                     with --ok/--bad-appropriate values via the signal table.
  --endpoint HOST:PORT   OTLP gRPC endpoint (default $ENDPOINT)

Env: MW_API_KEY (else read from kind secret $KIND_NS/$KIND_SECRET key $KIND_SECRET_KEY),
     MW_OTLP_ENDPOINT, MA_INTERVAL (default $INTERVAL), MA_RUN_DIR.
USAGE
}

# Duration string (e.g. 12m, 1h30m, 90s) -> seconds, for min-duration checks.
to_secs() {
  local d=$1 total=0 num unit
  [[ $d =~ ^([0-9]+[hms])+$ ]] || die "bad duration '$d' (use e.g. 12m, 90s, 1h5m)"
  while [[ $d =~ ^([0-9]+)([hms])(.*)$ ]]; do
    num=${BASH_REMATCH[1]}; unit=${BASH_REMATCH[2]}; d=${BASH_REMATCH[3]}
    case $unit in h) total=$((total+num*3600));; m) total=$((total+num*60));; s) total=$((total+num));; esac
  done
  echo "$total"
}

# Resolve the run length into DURATION: the signal minimum, or the operator
# override if it is at least that long. Sets a global (not echo) so that `die`
# aborts the whole script instead of just a $(...) subshell.
effective_duration() {
  local signal=$1 override=${2:-}
  local min=${SIG_MIN[$signal]}
  if [[ -z $override ]]; then DURATION=$min; return; fi
  (( $(to_secs "$override") < $(to_secs "$min") )) &&
    die "--duration $override is shorter than the $signal minimum $min (window + buffer; see header)"
  DURATION=$override
}

# Unique prefix per run: ma-<tag>-<signal-abbrev>-<epoch-mod-1e6><2 random>, e.g.
# ma-bigg-hcpu-812345a7. The signal abbrev + random suffix keep runs launched
# in the same second (e.g. six big_groups in a loop) from sharing a prefix,
# which would also make their pid/log files collide. Short enough to read.
mk_prefix() {
  local sig=$2 ab
  case $sig in host.cpu) ab=hcpu;; host.memory) ab=hmem;; k8s.node) ab=node;;
               k8s.pod) ab=pod;; k8s.container) ab=kctr;; container) ab=ctr;; esac
  printf 'ma-%s-%s-%d%02d' "$1" "$ab" "$(( $(date +%s) % 1000000 ))" "$(( RANDOM % 100 ))"
}

resolve_api_key() {
  if [[ -n ${MW_API_KEY:-} ]]; then log "MW_API_KEY: from environment"; return; fi
  command -v kubectl >/dev/null || die "MW_API_KEY not set and kubectl not found; export MW_API_KEY=<project key>"
  local ns=$KIND_NS
  if ! kubectl -n "$ns" get secret "$KIND_SECRET" >/dev/null 2>&1; then
    # Fall back to wherever the secret lives.
    ns=$(kubectl get secret -A 2>/dev/null | awk -v s="$KIND_SECRET" '$2==s{print $1; exit}') || true
    [[ -n $ns ]] || die "MW_API_KEY not set and secret $KIND_SECRET not found in any namespace; export MW_API_KEY=<project key>"
  fi
  # The local kind secret has been seen with the project key under "rqpft" and
  # under "api-key"; try the configured name first, then the other.
  local k
  for k in "$KIND_SECRET_KEY" api-key rqpft; do
    MW_API_KEY=$(kubectl -n "$ns" get secret "$KIND_SECRET" -o "jsonpath={.data.$k}" 2>/dev/null | base64 -d) || true
    [[ -n $MW_API_KEY ]] && break
  done
  [[ -n $MW_API_KEY ]] || die "secret $ns/$KIND_SECRET has none of keys '$KIND_SECRET_KEY', api-key, rqpft; export MW_API_KEY=<project key>"
  export MW_API_KEY
  log "MW_API_KEY: from kind secret $ns/$KIND_SECRET (key $k)"
}

build_bin() {
  # Rebuild when the binary is missing or older than any Go source / go.mod.
  if [[ ! -x $BIN ]] || [[ -n $(find "$REPO_DIR" -maxdepth 1 \( -name '*.go' -o -name 'go.mod' -o -name 'go.sum' \) -newer "$BIN" -print -quit) ]]; then
    log "building telemetrygen (missing or stale)"
    (cd "$REPO_DIR" && go build -o "$BIN" .)
  fi
}

# ----------------------------------------------------------------------------- runner
# run_signal <tag> <signal> <n> <breach> <duration-override|""> [extra flags...]
# The one-liner every scenario is built from. In --bg mode it detaches and
# records pid+log; otherwise it runs in the foreground with SIGINT forwarded so
# Ctrl-C still lets telemetrygen send its recovery batch.
run_signal() {
  local tag=$1 signal=$2 n=$3 breach=$4 override=$5; shift 5
  local extra=("$@")
  effective_duration "$signal" "$override"; local duration=$DURATION
  local bad=${SIG_BAD[$signal]} ok=${SIG_OK[$signal]}
  # Allow callers (mixed_severity) to override the breach value via BAD_OVERRIDE.
  [[ -n ${BAD_OVERRIDE:-} ]] && bad=$BAD_OVERRIDE
  [[ -n ${OK_OVERRIDE:-} ]] && ok=$OK_OVERRIDE
  local prefix; prefix=$(mk_prefix "$tag" "$signal")
  local args=(-signal "$signal" -prefix "$prefix" -n "$n" -breach "$breach"
              -ok "$ok" -bad "$bad" -interval "$INTERVAL" -duration "$duration"
              -endpoint "$ENDPOINT" "${extra[@]}")
  # A custom rule may sit on a different metric of the same resource shape.
  [[ -n ${OPT_METRIC:-} ]] && args+=(-metric "$OPT_METRIC")

  cat <<INFO
------------------------------------------------------------------------------
  run      : $tag
  signal   : $signal  (metric: $(metric_of "$signal"))
  WATCH    : rule "${SIG_ALERT[$signal]}"  -> History
  entities : $n, breaching: $breach  (value $bad vs healthy $ok)
  prefix   : $prefix
  duration : $duration  (min for $signal is ${SIG_MIN[$signal]}; interval $INTERVAL)
  cmd      : telemetrygen ${args[*]}
------------------------------------------------------------------------------
INFO

  if [[ $DRY_RUN == 1 ]]; then
    # Print a compact summary of the first batch; full JSON is huge for -n 600.
    "$BIN" "${args[@]}" -dry-run 2>&1 | summarize_dry_run
    return
  fi

  if [[ $BG == 1 ]]; then
    mkdir -p "$RUN_DIR"
    local logf="$RUN_DIR/$prefix.log"
    nohup "$BIN" "${args[@]}" >"$logf" 2>&1 &
    echo $! >"$RUN_DIR/$prefix.pid"
    log "started in background pid $! -> $logf"
  else
    "$BIN" "${args[@]}" &
    local pid=$!
    FG_PIDS+=("$pid")
    wait "$pid" || true
  fi
}

metric_of() {
  case $1 in
    host.cpu) echo system.cpu.utilization;; host.memory) echo system.memory.utilization;;
    k8s.node) echo "k8s.node.cpu.utilization / k8s.node.allocatable_cpu";;
    k8s.pod) echo k8s.pod.phase;; k8s.container) echo k8s.container.restarts;;
    container) echo container.cpu.utilization;;
  esac
}

# Reduce a -dry-run dump to: resource count, group-by attrs of the first
# resource, metric names, and the distinct values emitted (so the operator can
# eyeball breach vs healthy). Uses jq if present, else greps.
summarize_dry_run() {
  local tmp; tmp=$(mktemp)
  cat >"$tmp"
  grep '^[0-9/: ]*dry run' "$tmp" || true
  if command -v jq >/dev/null; then
    sed '/^[0-9]\{4\}\/[0-9]\{2\}\/[0-9]\{2\} /d' "$tmp" | jq -r '
      .resourceMetrics as $r
      | "  resources      : \($r|length)",
        "  group-by attrs : \($r[0].resource.attributes
            | map(select(.key|test("^(host\\.name|k8s\\.(cluster|node|namespace|pod|container)\\.name|container\\.name)$")))
            | map("\(.key)=\(.value.stringValue)") | join(", "))",
        "  metrics        : \([ $r[].scopeMetrics[].metrics[].name ] | unique | join(", "))",
        "  values seen    : \([ $r[].scopeMetrics[].metrics[] | select(.name|test("allocatable")|not) | .gauge.dataPoints[].asDouble ]
                               | group_by(.) | map("\(.[0]) x\(length)") | join("  "))"'
  else
    echo "  (install jq for a structured summary; raw values:)"
    grep -o '"asDouble": [0-9.]*' "$tmp" | sort | uniq -c | sed 's/^/  /'
  fi
  rm -f "$tmp"
}

# ----------------------------------------------------------------------------- scenarios
# Each scenario is a one-liner (or a few) over run_signal. OPT_* come from the CLI.

scenario_big_group() {
  # 40 hosts all >90% CPU for >= 12 min (10 min window + buffer) -> ONE
  # notification with 40 members on "High CPU usage for host".
  local sig=${OPT_SIGNAL:-host.cpu} n=${OPT_N:-40}
  run_signal bigg "$sig" "$n" "$n" "$OPT_DURATION"
}

scenario_singleton() {
  # 20 hosts, exactly 1 breaching memory (>= 7 min for the 5 min window) ->
  # a singleton row with host + value inline.
  local sig=${OPT_SIGNAL:-host.memory} n=${OPT_N:-20}
  run_signal single "$sig" "$n" 1 "$OPT_DURATION"
}

scenario_over_cap() {
  # 600 containers all breaching: far more than the 10 members the API attaches
  # per group, so the history row must show "+590 more" and page. Spread over
  # 6 synthetic hosts so host.name is not one value for all.
  local sig=${OPT_SIGNAL:-container} n=${OPT_N:-600}
  run_signal ocap "$sig" "$n" "$n" "$OPT_DURATION" -hosts 6
}

scenario_mixed_severity() {
  # Two cohorts of the SAME signal, started together with the same duration so
  # they land in the same evaluations: one cohort between Warning and Critical,
  # one above Critical. Distinct prefixes (-warn-/-crit- tags) keep them apart
  # in the member list. Both are forced into the background so they overlap;
  # the foreground then waits for them (unless --bg was asked for).
  local sig=${OPT_SIGNAL:-host.cpu} n=${OPT_N:-8}
  [[ ${SIG_WARN[$sig]} != - ]] || die "$sig has no reachable Warning tier; use host.cpu, host.memory, k8s.node or container"
  local dur=$OPT_DURATION
  local was_bg=$BG; BG=1
  BAD_OVERRIDE=${SIG_WARN[$sig]} run_signal mixwarn "$sig" "$n" "$n" "$dur"
  run_signal mixcrit "$sig" "$n" "$n" "$dur"
  BG=$was_bg
  [[ $DRY_RUN == 1 || $was_bg == 1 ]] || wait_bg
}

scenario_multi_signal() {
  # Five grouped rules firing at once, each with healthy resources plus
  # breachers, to populate the whole history page. All run in the background
  # for the longest minimum (restarts: 32 min) unless --duration says more.
  local dur=${OPT_DURATION:-32m}
  local was_bg=$BG; BG=1
  run_signal msig-hcpu host.cpu      6  4 "$dur"
  run_signal msig-hmem host.memory   6  3 "$dur"
  run_signal msig-node k8s.node      5  3 "$dur"
  run_signal msig-pod  k8s.pod      12  5 "$dur" -hosts 3
  run_signal msig-ctr  k8s.container 8  4 "$dur" -hosts 2
  BG=$was_bg
  [[ $DRY_RUN == 1 || $was_bg == 1 ]] || wait_bg
}

scenario_flap() {
  # 30 hosts, 10 breaching, and the breaching window rotates every 12 min
  # (>= one full 10 min window, so each cohort actually crosses before the next
  # takes over). Run 3 rotations (36 min) so history shows groups re-forming
  # with different members and renotify behaviour.
  local sig=${OPT_SIGNAL:-host.cpu} n=${OPT_N:-30}
  local dur=${OPT_DURATION:-36m}
  run_signal flap "$sig" "$n" $((n/3)) "$dur" -flap 12m
}

scenario_all() {
  # Representative set. The first four are independent signals/prefixes, so
  # they can safely run in parallel; flap reuses host.cpu and is run afterwards
  # so its rotating members are not confused with big_group's static ones.
  local was_bg=$BG; BG=1
  OPT_SIGNAL='' OPT_N='' scenario_big_group
  OPT_SIGNAL='' OPT_N='' scenario_singleton
  OPT_SIGNAL='' OPT_N='' scenario_over_cap
  OPT_SIGNAL='' OPT_N='' scenario_mixed_severity
  BG=$was_bg
  [[ $DRY_RUN == 1 ]] && { OPT_SIGNAL='' OPT_N='' scenario_flap; return; }
  if [[ $was_bg == 1 ]]; then
    warn "flap is skipped in --bg 'all' (it must follow big_group); run 'flap --bg' after those finish"
    return
  fi
  wait_bg
  log "parallel set finished; starting flap"
  OPT_SIGNAL='' OPT_N='' scenario_flap
}

# ----------------------------------------------------------------------------- bg mgmt
bg_pids() { [[ -d $RUN_DIR ]] && cat "$RUN_DIR"/*.pid 2>/dev/null || true; }

wait_bg() {
  local pids; pids=$(bg_pids)
  [[ -n $pids ]] || return 0
  log "waiting for background runs: $pids (Ctrl-C = recover + stop them)"
  for p in $pids; do while kill -0 "$p" 2>/dev/null; do sleep 5; done; done
  cleanup_pidfiles
}

cleanup_pidfiles() {
  [[ -d $RUN_DIR ]] || return 0
  for f in "$RUN_DIR"/*.pid; do
    [[ -f $f ]] || continue
    kill -0 "$(cat "$f")" 2>/dev/null || rm -f "$f"
  done
}

cmd_stop() {
  local pids; pids=$(bg_pids)
  [[ -n $pids ]] || { log "no background runs"; return; }
  # SIGINT -> telemetrygen's NotifyContext -> final all-healthy batch, then exit.
  for p in $pids; do kill -INT "$p" 2>/dev/null && log "sent SIGINT to $p (recovering)"; done
  for p in $pids; do
    for _ in $(seq 1 30); do kill -0 "$p" 2>/dev/null || break; sleep 1; done
    kill -0 "$p" 2>/dev/null && { warn "pid $p did not exit; killing (NO recovery batch sent)"; kill -KILL "$p"; }
  done
  rm -f "$RUN_DIR"/*.pid
  log "stopped. logs kept in $RUN_DIR"
}

cmd_status() {
  cleanup_pidfiles
  local any=0
  for f in "$RUN_DIR"/*.pid; do
    [[ -f $f ]] || continue; any=1
    local name; name=$(basename "$f" .pid)
    printf '%-28s pid %-7s %s\n' "$name" "$(cat "$f")" "$(tail -n1 "$RUN_DIR/$name.log" 2>/dev/null)"
  done
  (( any )) || log "no background runs"
}

# Ctrl-C in the foreground: forward SIGINT to running children so they send
# their recovery batch, and also to any background runs this invocation started.
FG_PIDS=()
on_int() {
  echo
  warn "interrupted — asking telemetrygen to recover (healthy batch) and exit"
  for p in "${FG_PIDS[@]:-}"; do [[ -n $p ]] && kill -INT "$p" 2>/dev/null || true; done
  cmd_stop
  exit 130
}

# ----------------------------------------------------------------------------- main
BG=0 DRY_RUN=0 OPT_DURATION="" OPT_SIGNAL="" OPT_N="" OPT_METRIC=""
[[ $# -ge 1 ]] || { usage; exit 1; }
cmd=$1; shift
[[ $cmd == dry-run ]] && { DRY_RUN=1; [[ $# -ge 1 ]] || die "dry-run needs a scenario"; cmd=$1; shift; }

while [[ $# -gt 0 ]]; do
  case $1 in
    --bg) BG=1;;
    --duration) OPT_DURATION=$2; shift;;
    --signal) OPT_SIGNAL=$2; [[ -n ${SIG_ALERT[$2]:-} ]] || die "unknown signal $2"; shift;;
    --n) OPT_N=$2; shift;;
    --metric) OPT_METRIC=$2; shift;;
    --endpoint) ENDPOINT=$2; shift;;
    -h|--help) usage; exit 0;;
    *) die "unknown option $1";;
  esac
  shift
done

case $cmd in
  help|-h|--help) usage; exit 0;;
  stop) cmd_stop; exit 0;;
  status) cmd_status; exit 0;;
  big_group|singleton|over_cap|mixed_severity|multi_signal|flap|all) ;;
  *) usage; die "unknown scenario '$cmd'";;
esac

build_bin
if [[ $DRY_RUN == 0 ]]; then
  resolve_api_key
  trap on_int INT TERM
fi
"scenario_$cmd"
[[ $DRY_RUN == 1 ]] && log "dry run complete — nothing was sent" || log "scenario $cmd done"
