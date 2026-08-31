#!/usr/bin/env bash
# "High CPU usage for host"  —  N hosts pinned at 120% CPU.
#
# The rule is a FORMULA: avg(user) + avg(steal) + avg(wait) + avg(system), x100.
# A formula produces NO ROW for a host missing any one of its four input series — not a
# zero, no row at all — so telemetrygen emits all four states, the busy fraction on
# `user` and 0 on the rest. Emitting only state=user makes the hosts invisible to the
# rule and nothing ever fires. Verified the hard way, twice.
. "$(dirname "$0")/_common.sh"

ALERT="High CPU usage for host"
TAG=hcpu
SIGNAL=host.cpu
OK=0.03            # ~3%
BAD=1.2            # FRACTION on the wire: 1.2 -> 120%, well over the 90% critical
NOTE="system.cpu.utilization is a fraction; 1.2 = 120% after the rule's x100"

parse_args "$@"
run_alert
