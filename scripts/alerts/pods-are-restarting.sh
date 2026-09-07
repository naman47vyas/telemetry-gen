#!/usr/bin/env bash
# "Pods are restarting"  —  N containers whose restart counter climbs every tick.
#
# k8s.container.restarts is a CUMULATIVE counter and the rule takes the difference over
# its window, so a flat high number does not breach — the value has to keep climbing.
# telemetrygen ramps it by BAD per tick. Each container also carries
# current_waiting_reason=CrashLoopBackOff, which makes the "Pod is in CrashloopBackoff
# State" rule match at the same time.
#
# This rule's window is the long one (30m locally), so give it a generous duration.
#
# The group is split by severity: two thirds of the entities sit at BAD (critical) and the
# rest at WARN, which is over the rule's warning threshold but under its critical one, so
# one notification carries both severities. WARN_N moves the line; WARN_N=0 makes the whole
# cohort critical again.
#
# On a counter rule the warning cohort is not a lower number but a SLOWER RAMP: WARN is
# restarts per tick too, so 0.05 is one restart every twenty ticks against the critical
# cohort's three per tick. The ramp is floored, so the counter still reports whole restarts.
. "$(dirname "$0")/_common.sh"

ALERT="Pods are restarting"
TAG=kctr
SIGNAL=k8s.container
OK=${OK:-0}         # never restarts
BAD=${BAD:-3}       # +3 restarts per tick, so the windowed difference climbs fast
WARN=${WARN:-0.05}  # +1 restart every 20 ticks: restarting, but not crashlooping
NOTE="restarts is a counter; the rule diffs over its window, so the value must RAMP"

# Spread the entities over a cluster x namespace grid. The rule groups by
# cluster + namespace + pod + container, so this widens the group keys inside the single
# notification rather than adding more notifications.
CLUSTERS=${CLUSTERS:-3}
NAMESPACES=${NAMESPACES:-5}
EXTRA=(-clusters "$CLUSTERS" -hosts "$NAMESPACES")
SPREAD="$CLUSTERS clusters x $NAMESPACES namespaces"

parse_args "$@"
run_alert
