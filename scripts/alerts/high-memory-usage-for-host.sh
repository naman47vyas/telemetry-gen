#!/usr/bin/env bash
# "High memory usage for host"  —  N hosts pinned at 95% memory.
#
# Single series, pinned to state=used, grouped by host.name. If this rule ever turns out
# to be a formula over several states (the way the host CPU rule is), this would go quiet
# and the fix would be to emit cached/free/used the way host.cpu emits its four states.
. "$(dirname "$0")/_common.sh"

ALERT="High memory usage for host"
TAG=hmem
SIGNAL=host.memory
OK=0.20            # 20%
BAD=0.95           # FRACTION on the wire: 0.95 -> 95%, over the 90% critical
NOTE="system.memory.utilization is a fraction; 0.95 = 95% after the rule's x100"

parse_args "$@"
run_alert
