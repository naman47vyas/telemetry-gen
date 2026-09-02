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
# .env.beta (gitignored) is the convenient place to keep MW_API_KEY and MW_OTLP_ENDPOINT.
# Anything already exported wins, so `MW_API_KEY=… ./pods-are-failing.sh` still works.
if [[ -f $REPO/.env.beta ]]; then
  # shellcheck disable=SC1091
  { set -a; . "$REPO/.env.beta"; set +a; }
fi

ENDPOINT="${MW_OTLP_ENDPOINT:-127.0.0.1:4321}"
INTERVAL="${MA_INTERVAL:-10s}"

if [[ -z ${MW_API_KEY:-} ]]; then
  echo "ERROR: MW_API_KEY is not set." >&2
  echo "  export it, or put it in $REPO/.env.beta:" >&2
  echo "    MW_API_KEY=…" >&2
  echo "    MW_OTLP_ENDPOINT=https://<tenant>.middleware.io:443" >&2
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
# Optional: METRIC, EXTRA (array of extra flags).
# Overridable per run by env or by the two positional args: N and DURATION.
run_alert() {
  # Precedence for both knobs: env var, then positional arg, then the script's default.
  # ARGS is filled by parse_args; reading it directly avoids the empty-array expansion
  # traps you hit when forwarding "${ARGS[@]}" into another function's positionals.
  local n=${N:-${ARGS[0]:-${DEFAULT_N:-200}}}
  local duration=${DURATION:-${ARGS[1]:-${DEFAULT_DURATION:-45m}}}
  local prefix; prefix=$(mk_prefix "$TAG")
  local cmd=("$BIN" -signal "$SIGNAL" -prefix "$prefix" -n "$n" -breach "$n"
             -ok "$OK" -bad "$BAD" -interval "$INTERVAL" -duration "$duration"
             -endpoint "$ENDPOINT")
  [[ -n ${METRIC:-} ]] && cmd+=(-metric "$METRIC")
  [[ -n ${EXTRA:-} ]] && cmd+=("${EXTRA[@]}")

  build_bin
  cat >&2 <<INFO
------------------------------------------------------------------------------
  WATCH     : rule "$ALERT"  ->  Alerts > History
  entities  : $n, all breaching
  prefix    : $prefix
  values    : healthy $OK, breaching $BAD${SPREAD:+
  spread    : $SPREAD}
  note      : $NOTE
  duration  : $duration  (interval $INTERVAL)
  endpoint  : $ENDPOINT
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
        exit 0;;
      *) ARGS+=("$1");;
    esac
    shift
  done
}
