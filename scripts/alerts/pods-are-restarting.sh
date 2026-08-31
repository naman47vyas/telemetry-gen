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
. "$(dirname "$0")/_common.sh"

ALERT="Pods are restarting"
TAG=kctr
SIGNAL=k8s.container
OK=0               # never restarts
BAD=3              # +3 restarts per tick, so the windowed difference climbs fast
NOTE="restarts is a counter; the rule diffs over its window, so the value must RAMP"

parse_args "$@"
run_alert
