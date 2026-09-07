#!/usr/bin/env bash
# "Errors detected in traces"  —  N APM services whose every request fails.
#
# Same spans and the same profile as error-rate-is-high.sh; this rule counts error spans
# rather than taking their share, so what matters here is volume, not ratio. Every request
# fails (BAD=100) and each service serves more of them per interval.
#
# Every failure carries an `exception` span event with a type, a message and a stack trace —
# that is what Middleware reads out of a trace to build an error — and the five failure kinds
# rotate across services and endpoints, so the errors list has something to group by instead
# of one row repeated N times.
#
# BAD is a PERCENTAGE of requests, so 100 means "all of them". Errors per interval are
# therefore N x SPANS x 2 (each failed request fails at its root and in its database child).
#
# Note this necessarily fires "Error rate is high" too: a service where every request fails
# is breaching both rules, and they read the same table. Run error-rate-is-high.sh instead
# if you want only that one.
#
# The group is split by severity: two thirds of the entities sit at BAD (critical) and the
# rest at WARN, which is over the rule's warning threshold but under its critical one, so
# one notification carries both severities. WARN_N moves the line; WARN_N=0 makes the whole
# cohort critical again.
. "$(dirname "$0")/_common.sh"

ALERT="Errors detected in traces"
TAG=errtrc
SIGNAL=trace.error
OK=${OK:-0}         # healthy services fail nothing at all
BAD=${BAD:-100}     # PERCENT of requests that fail: every one of them
WARN=${WARN:-15}    # PERCENT: still a steady stream of error spans, a smaller one
NOTE="values are PERCENT of requests that fail; 100 = every request errors"

# Traffic shape. N is the number of services; SPANS is how many requests each serves per
# interval, and every one of them becomes two error spans.
SPANS=${SPANS:-8}
HOSTS=${HOSTS:-4}
EXTRA=(-spans "$SPANS" -hosts "$HOSTS")
SPREAD="$HOSTS hosts, $SPANS requests per service per $INTERVAL, all failing"

parse_args "$@"
run_alert
