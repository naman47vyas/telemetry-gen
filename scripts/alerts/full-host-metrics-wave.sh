#!/usr/bin/env bash
# The whole life of an alert, on hosts with real metrics behind them.
#
# full-host-metrics.sh holds its hosts at one level for the length of the run. This one
# walks them round a cycle instead — every SEV_FLAP the entire fleet moves to the next
# severity:
#
#   critical -> warning -> healthy -> warning -> (round again)
#
# so the rule fires critical, de-escalates to warning, RESOLVES, then climbs back up and
# fires again. That healthy step is the difference between this and mixed-severity-group.sh,
# whose wave only ever rearranges severities inside a group that stays open: here the alert
# actually closes and re-opens, which is what you want if you are looking at notification
# history, resolve behaviour, cooldown or re-arm.
#
# The hosts are the same full-fidelity machines as full-host-metrics.sh — all 29 hostmetrics
# metrics, about 100 datapoints each — so the host pages stay populated through every step
# of the cycle, including the healthy ones.
#
# TIMING IS THE WHOLE GAME HERE. Each step has to outlast the rule's evaluation window
# (5 minutes on the host rules) or the window straddles two steps, every host averages 96%
# and 5%, and nothing reads as anything. SEV_FLAP defaults to 10m, which leaves each step
# about half a window of settled state. The full cycle is then 4 x 10m = 40 minutes and
# DURATION defaults to 3 of them. Expect the UI to lag each step by up to a window: the
# resolve does not land the moment the values drop.
#
#   ./full-host-metrics-wave.sh                     # 25 hosts, 40m cycle, 3 times round
#   SEV_FLAP=15m ./full-host-metrics-wave.sh        # slower, if the window is longer
#   TIER_WAVE=crit,ok ./full-host-metrics-wave.sh   # just fire and resolve, no middle step
#   TIER_WAVE=crit,warn,ok ./full-host-metrics-wave.sh   # only ever de-escalate
#   TIER_PHASES=4 ./full-host-metrics-wave.sh       # see below
#
# TIER_PHASES splits the fleet into that many groups entering the cycle at different points,
# so at any moment some hosts are critical, some warning and some healthy, and each host
# still walks the whole cycle over time. The group then always holds a mix — but its
# aggregate counts barely move, and it never resolves, because there is always someone
# breaching. Lockstep (the default) is the one that shows an alert closing.
#
# Values are the CPU busy fraction, as in full-host-metrics.sh: 0.96 critical, 0.87 warning,
# 0.05 healthy. Memory and everything else stay plausible and untiered throughout, so the
# host pages look like machines under varying load rather than machines being toggled.
. "$(dirname "$0")/_common.sh"

ALERT="High CPU usage for host"
TAG=hostwave
SIGNAL=host.full
OK=${OK:-0.05}      # FRACTION: 5% — the step where the alert resolves
BAD=${BAD:-0.96}    # FRACTION: 96%, over the 90% critical
WARN=${WARN:-0.87}  # FRACTION: 87%, inside the 85-90 warning band
NOTE="OK/WARN/BAD are the CPU busy fraction; the fleet walks all three, healthy included"

DEFAULT_N=25
DEFAULT_DURATION=2h
SEV_FLAP=${SEV_FLAP:-10m}
TIER_WAVE=${TIER_WAVE:-crit,warn,ok,warn}
SPREAD="29 metrics, ~100 datapoints per host per $INTERVAL"

parse_args "$@"
run_alert
