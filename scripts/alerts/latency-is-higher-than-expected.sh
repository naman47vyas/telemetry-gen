#!/usr/bin/env bash
# "Latency is higher than expected"  —  N APM services whose every request takes ~4 s.
#
# The odd one out in this directory: this rule reads the APM SPANS table, not a metric.
# A service's latency is simply how long its requests took, so telemetrygen emits TRACES
# here — each service serves SPANS requests per interval, every one a root SERVER span of
# BAD milliseconds with a slow database child inside it.
#
# BAD is MILLISECONDS of request duration. Not a percent, not a fraction, not an enum.
#
# The durations jitter only ±8%, so avg, p50, p90 and p99 all land on the same number —
# whichever aggregation the rule is configured with, it sees the same slow service. The
# child CLIENT span holds 80% of the time, so a rule that counts root or SERVER spans
# only still measures the full BAD.
. "$(dirname "$0")/_common.sh"

ALERT="Latency is higher than expected"
TAG=lat
SIGNAL=trace.service
OK=45              # 45 ms — a request nobody would notice
BAD=4000           # MILLISECONDS: 4000 = a 4 second request
NOTE="values are MILLISECONDS of request duration; 4000 = a 4 s request"

# Traffic shape. The rule groups by service, so N is the number of services; SPANS is how
# many requests each one serves per interval and HOSTS spreads them over several machines
# so the service map is not one box with 200 services on it.
SPANS=${SPANS:-4}
HOSTS=${HOSTS:-4}
EXTRA=(-spans "$SPANS" -hosts "$HOSTS")
SPREAD="$HOSTS hosts, $SPANS requests per service per $INTERVAL, 4 endpoints each"

parse_args "$@"
run_alert
