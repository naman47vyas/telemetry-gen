#!/usr/bin/env bash
# "Error rate is high"  —  N APM services where 85 of every 100 requests fail.
#
# Like the latency script, this rule reads the APM SPANS table rather than a metric: a
# service's error rate is the share of its requests that came back an error, so telemetrygen
# emits TRACES here. Each service serves SPANS requests per interval and BAD% of them fail —
# a 5xx root SERVER span with an ERROR status and an `exception` event, over a database child
# that failed the same way.
#
# BAD is a PERCENTAGE of requests. Not a fraction (0.85 would be 0.85% and never fire — the
# generator warns if you pass one) and not a count of errors.
#
# The root and its child fail together, so the share of spans that are errors is the same
# number whether the rule counts every span or only the SERVER ones. Failures are dealt out
# on a running index rather than rounded per tick, so 85% is exactly 85% over the window,
# and each service is phase-shifted so they do not all fail on the same requests.
#
# Request duration stays at a mundane 150 ms throughout, so this run does not also trip
# "Latency is higher than expected" and leave you unsure which rule you were watching.
#
# The group is split by severity: two thirds of the entities sit at BAD (critical) and the
# rest at WARN, which is over the rule's warning threshold but under its critical one, so
# one notification carries both severities. WARN_N moves the line; WARN_N=0 makes the whole
# cohort critical again.
. "$(dirname "$0")/_common.sh"

ALERT="Error rate is high"
TAG=errate
SIGNAL=trace.error
OK=${OK:-0}         # healthy services fail nothing at all
BAD=${BAD:-85}      # PERCENT of requests that fail
WARN=${WARN:-12}    # PERCENT: a service in trouble, not a service that is down
NOTE="values are PERCENT of requests that fail; 85 = 85 of every 100 requests return 5xx"

# Traffic shape. The rule groups by service, so N is the number of services; SPANS is how
# many requests each one serves per interval — a bigger denominator makes the percentage
# steady — and HOSTS spreads them over several machines.
SPANS=${SPANS:-6}
HOSTS=${HOSTS:-4}
EXTRA=(-spans "$SPANS" -hosts "$HOSTS")
SPREAD="$HOSTS hosts, $SPANS requests per service per $INTERVAL, 4 endpoints each"

parse_args "$@"
run_alert
