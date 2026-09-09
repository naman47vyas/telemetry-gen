# shellcheck shell=bash
# Shared plumbing for the per-alert scripts in this directory.
#
# Each alert script sets a handful of variables and calls run_alert. Everything that is
# the same for every alert — finding the API key, building the binary, minting a unique
# prefix, printing what is about to happen — lives here.
#
# Sourced, never executed directly.

set -euo pipefail

REPO=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
BIN="$REPO/telemetrygen"
RUN_DIR="${MA_RUN_DIR:-$REPO/.multi-alert-runs}"

# ---------------------------------------------------------------- credentials / target
# MW_API_KEY and MW_OTLP_ENDPOINT decide WHICH PROJECT a run lands in. They come from an
# env file at the repo root — .env.beta by default, or whichever one MW_ENV_FILE names, so
# a second project is a second file and not an edit:
#
#   cp .env.beta .env.prod && $EDITOR .env.prod
#   MW_ENV_FILE=.env.prod ./pods-are-failing.sh
#
# Anything already exported wins over the file, so a one-off
# `MW_API_KEY=… MW_OTLP_ENDPOINT=… ./pods-are-failing.sh` works too. That is what the
# save/restore below is for: the env files say `export MW_API_KEY=…`, which plain sourcing
# would slam straight over the top of whatever the caller set.
ENV_FILE="${MW_ENV_FILE:-.env.beta}"
[[ $ENV_FILE == /* ]] || ENV_FILE="$REPO/$ENV_FILE"
shell_key=${MW_API_KEY:-}
shell_endpoint=${MW_OTLP_ENDPOINT:-}
if [[ -f $ENV_FILE ]]; then
  # shellcheck disable=SC1090,SC1091
  { set -a; . "$ENV_FILE"; set +a; }
elif [[ -n ${MW_ENV_FILE:-} ]]; then
  # Named a file explicitly and it is not there: almost certainly a typo, and silently
  # falling back would send the run to whatever project the shell happened to hold.
  echo "ERROR: MW_ENV_FILE=$MW_ENV_FILE not found (looked for $ENV_FILE)" >&2
  exit 1
fi
[[ -n $shell_key ]] && MW_API_KEY=$shell_key
[[ -n $shell_endpoint ]] && MW_OTLP_ENDPOINT=$shell_endpoint
export MW_API_KEY

ENDPOINT="${MW_OTLP_ENDPOINT:-127.0.0.1:4321}"
INTERVAL="${MA_INTERVAL:-10s}"

if [[ -z ${MW_API_KEY:-} ]]; then
  echo "ERROR: MW_API_KEY is not set." >&2
  echo "  export it, or put it in $ENV_FILE:" >&2
  echo "    MW_API_KEY=…" >&2
  echo "    MW_OTLP_ENDPOINT=https://<tenant>.middleware.io:443" >&2
  echo "  another project's file: MW_ENV_FILE=.env.prod $(basename "$0")" >&2
  exit 1
fi

# ---------------------------------------------------------------------------- the binary
build_bin() {
  # Rebuild when the binary is missing or older than any source file, so a script run
  # never silently uses a stale generator.
  if [[ ! -x $BIN ]] || [[ -n $(find "$REPO" -maxdepth 1 -name '*.go' -newer "$BIN" -print -quit) ]]; then
    echo "building telemetrygen…" >&2
    (cd "$REPO" && go build -o telemetrygen .)
  fi
}

# Unique per run so each launch is a fresh cohort of entities and two runs can never be
# confused for one another in the alert history.
mk_prefix() { printf 'ma-%s-%d%02d' "$1" "$(( $(date +%s) % 1000000 ))" "$(( RANDOM % 100 ))"; }

# --------------------------------------------------------------------------- run_alert
# Expects these to be set by the calling script:
#   ALERT     human name of the rule to watch in the UI
#   TAG       short slug used in the entity prefix
#   SIGNAL    telemetrygen -signal
#   OK BAD    healthy / breaching values
#   NOTE      one line explaining the value scale (percent vs fraction vs enum)
# Optional: METRIC, EXTRA (array of extra flags), and WARN.
#
# WARN is the value for the cohort that should come out WARNING rather than critical —
# over the rule's warning threshold, under its critical one. A script that sets it gets a
# split group by default: WARN_PCT of the entities warning (33 unless the script says
# otherwise), the rest critical. Every knob is env-overridable per run — WARN_N or WARN_PCT
# move the line (WARN_N=0 is the old all-critical behaviour), WARN moves the value — and a
# script that sets no WARN at all has no warning tier, because its signal has no middle
# value: a pod is Running or it is Failed.
#
# SEV_FLAP makes that split MOVE instead of holding still. What it walks depends on which
# cycle you give it, and they are mutually exclusive:
#
#   SEV_WAVE   multipliers of the warning count. The split moves, everything keeps
#              breaching, and the group stays open the whole time — members escalate and
#              de-escalate inside one notification.
#   TIER_WAVE  severities the whole cohort walks: crit,warn,ok,warn. This one goes HEALTHY,
#              so the alert resolves and fires again — the whole life of an alert rather
#              than a rearrangement inside one open group. TIER_PHASES splits the fleet
#              into groups entering the cycle at different points, if you would rather the
#              group always held a mix than have everything move in lockstep.
#
# Any script here accepts them; mixed-severity-group.sh sets SEV_WAVE and
# full-host-metrics-wave.sh sets TIER_WAVE. Keep SEV_FLAP longer than the rule's evaluation
# window either way, or every member just averages its values and settles on nothing.
#
# The values here are educated guesses at where each rule's two thresholds sit — that is
# not something the data can tell you. If a cohort lands on the wrong severity in the UI,
# WARN= is the knob, not a code change.
# Overridable per run by env or by the two positional args: N and DURATION.
run_alert() {
  # Precedence for both knobs: env var, then positional arg, then the script's default.
  # ARGS is filled by parse_args; reading it directly avoids the empty-array expansion
  # traps you hit when forwarding "${ARGS[@]}" into another function's positionals.
  local n=${N:-${ARGS[0]:-${DEFAULT_N:-200}}}
  local duration=${DURATION:-${ARGS[1]:-${DEFAULT_DURATION:-45m}}}
  local prefix; prefix=$(mk_prefix "$TAG")

  # Severity split. Every entity still breaches; WARN_N of them breach only as far as the
  # warning threshold. A third is enough to be unmissable in the group without making the
  # critical count look thin.
  local warn_n=0 spread_sev="all breaching"
  if [[ -n ${WARN:-} && ${HEALTHY:-0} != 1 ]]; then
    # Rounded, so the documented "a third" is a third rather than a floor.
    warn_n=${WARN_N:-$(( (n * ${WARN_PCT:-33} + 50) / 100 ))}
    if (( warn_n < 0 || warn_n > n )); then
      echo "ERROR: WARN_N=$warn_n is not between 0 and N=$n" >&2
      exit 1
    fi
    spread_sev="all breaching: $(( n - warn_n )) critical, $warn_n warning"
  fi
  # HEALTHY=1 sends the whole fleet at OK and fires nothing — for the profiles that emit a
  # host's full metric set, where the point is populating the pages rather than alerting.
  local crit=$(( n - warn_n ))
  local values_desc="healthy $OK${WARN:+, warning $WARN}, critical $BAD"
  if [[ ${HEALTHY:-0} == 1 ]]; then
    crit=0 warn_n=0 spread_sev="none breaching (HEALTHY=1)"
    values_desc="healthy $OK, and nothing else is sent"
  fi

  # The severity wave, spelled out as the counts it will actually produce — the multipliers
  # on their own ("1,0.5,1,1.5") tell you nothing about what the group will look like.
  # SEV_FLAP=0 (or 0s) is how a caller turns the movement off, so treat it as unset rather
  # than announcing a wave that will never move.
  local sev_flap=${SEV_FLAP:-} wave_desc="" wave_label="split moves"
  [[ $sev_flap =~ ^0[a-z]*$ ]] && sev_flap=""
  if [[ -n $sev_flap && ${HEALTHY:-0} != 1 ]]; then
    if [[ -n ${TIER_WAVE:-} ]]; then
      # In this mode the critical/warning split is not what moves — the whole cohort does,
      # so say that rather than leaving the entities line claiming a split that never holds.
      wave_label="fleet moves"
      spread_sev="all on one cycle"
      (( ${TIER_PHASES:-1} > 1 )) && spread_sev="all on one cycle, in ${TIER_PHASES} phase groups"
      # Spell the tiers out in full: "crit" in a banner is too easy to read past.
      wave_desc=$(awk -v spec="$TIER_WAVE" -v p="${TIER_PHASES:-1}" 'BEGIN{
        k = split(spec, t, ",")
        for (i = 1; i <= k; i++) {
          gsub(/^ +| +$/, "", t[i])
          n = (t[i] ~ /^c/) ? "all critical" : (t[i] ~ /^w/) ? "all warning" : "all healthy"
          out = out (i > 1 ? " -> " : "") n
        }
        print out (p > 1 ? " (in " p " phase groups, so the fleet is never all one thing)" : "") }')
    elif (( warn_n > 0 )); then
      wave_desc=$(awk -v w="$warn_n" -v t="$n" -v spec="${SEV_WAVE:-1,0.5,1,1.5}" 'BEGIN{
        k = split(spec, m, ",")
        for (i = 1; i <= k; i++) {
          x = int(w * m[i] + 0.5); if (x > t) x = t
          out = out (i > 1 ? " -> " : "") (t - x) "c/" x "w"
        }
        print out }')
    fi
  fi

  local cmd=("$BIN" -signal "$SIGNAL" -prefix "$prefix" -n "$n" -breach "$crit"
             -ok "$OK" -bad "$BAD" -interval "$INTERVAL" -duration "$duration"
             -endpoint "$ENDPOINT")
  [[ -n ${WARN:-} ]] && cmd+=(-warn "$WARN" -warn-n "$warn_n")
  [[ -n $sev_flap ]] && cmd+=(-sev-flap "$sev_flap")
  [[ -n ${SEV_WAVE:-} ]] && cmd+=(-sev-wave "$SEV_WAVE")
  [[ -n ${TIER_WAVE:-} ]] && cmd+=(-tier-wave "$TIER_WAVE")
  [[ -n ${TIER_PHASES:-} ]] && cmd+=(-tier-phases "$TIER_PHASES")
  [[ -n ${METRIC:-} ]] && cmd+=(-metric "$METRIC")
  [[ -n ${EXTRA:-} ]] && cmd+=("${EXTRA[@]}")

  build_bin
  cat >&2 <<INFO
------------------------------------------------------------------------------
  WATCH     : rule "$ALERT"  ->  Alerts > History
  entities  : $n, $spread_sev
  prefix    : $prefix
  values    : $values_desc${wave_desc:+
  wave      : $wave_label every $sev_flap: $wave_desc}${SPREAD:+
  spread    : $SPREAD}
  note      : $NOTE
  duration  : $duration  (interval $INTERVAL)
  endpoint  : $ENDPOINT
  creds     : ${ENV_FILE#"$REPO/"}${shell_key:+ (MW_API_KEY overridden in the shell)}
------------------------------------------------------------------------------
  Ctrl-C at any time: a final all-healthy batch is sent so the alert resolves
  instead of being left permanently breaching.
INFO

  if [[ ${DRY_RUN:-0} == 1 ]]; then
    "${cmd[@]}" -dry-run
    return
  fi
  if [[ ${BG:-0} == 1 ]]; then
    mkdir -p "$RUN_DIR"
    nohup "${cmd[@]}" >"$RUN_DIR/$prefix.log" 2>&1 &
    echo $! >"$RUN_DIR/$prefix.pid"
    echo >&2 "  started in background pid $! -> $RUN_DIR/$prefix.log"
    echo >&2 "  stop it with: kill -INT $!"
    return
  fi
  "${cmd[@]}"
}

# Flags common to every alert script; positional args stay as N and DURATION.
parse_args() {
  ARGS=()
  : "${DEFAULT_N:=200}" "${DEFAULT_DURATION:=45m}"
  while [[ $# -gt 0 ]]; do
    case $1 in
      --dry-run) DRY_RUN=1;;
      --bg)      BG=1;;
      -h|--help)
        echo "usage: $(basename "$0") [N] [DURATION] [--bg] [--dry-run]"
        echo "  N         how many entities breach   (default $DEFAULT_N)"
        echo "  DURATION  how long to keep breaching (default $DEFAULT_DURATION)"
        if [[ -n ${WARN:-} ]]; then
          echo "env:"
          echo "  WARN_N    how many of the N are only WARNING, not critical (default ${WARN_PCT:-33}% of N)"
          echo "  WARN_PCT  the same as a percentage of N (default ${WARN_PCT:-33})"
          echo "  WARN      the value that cohort emits (default $WARN)"
          echo "  SEV_FLAP  move the split every this often${SEV_FLAP:+ (default $SEV_FLAP)}, so members escalate and de-escalate"
          echo "  SEV_WAVE  the cycle it walks, as multiples of WARN_N (default ${SEV_WAVE:-1,0.5,1,1.5})"
          if [[ -n ${TIER_WAVE:-} ]]; then
            echo "  TIER_WAVE severities the whole cohort walks, healthy included (default $TIER_WAVE)"
            echo "  TIER_PHASES  split the fleet into this many groups at different points (default ${TIER_PHASES:-1})"
          fi
          echo "  HEALTHY=1 send the whole fleet at OK and fire nothing"
        fi
        exit 0;;
      *) ARGS+=("$1");;
    esac
    shift
  done
}
