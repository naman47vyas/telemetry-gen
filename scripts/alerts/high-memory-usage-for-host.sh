#!/usr/bin/env bash
# "High memory usage for host"  —  N hosts pinned at 95% memory.
#
# Single series, pinned to state=used, grouped by host.name. If this rule ever turns out
# to be a formula over several states (the way the host CPU rule is), this would go quiet
# and the fix would be to emit cached/free/used the way host.cpu emits its four states.
#
# The group is split by severity: two thirds of the entities sit at BAD (critical) and the
# rest at WARN, which is over the rule's warning threshold but under its critical one, so
# one notification carries both severities. WARN_N moves the line; WARN_N=0 makes the whole
# cohort critical again.
. "$(dirname "$0")/_common.sh"

ALERT="High memory usage for host"
TAG=hmem
SIGNAL=host.memory
OK=${OK:-0.20}      # 20%
BAD=${BAD:-0.95}    # FRACTION on the wire: 0.95 -> 95%, over the 90% critical
WARN=${WARN:-0.87}  # FRACTION: 87%, inside the 85-90 warning band
NOTE="system.memory.utilization is a fraction; 0.95 = 95% after the rule's x100"

parse_args "$@"
run_alert
