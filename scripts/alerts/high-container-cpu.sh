#!/usr/bin/env bash
# "High Container CPU"  —  N containers pinned at 96% CPU.
#
# container.cpu.utilization is ALREADY a percent on the wire (unlike the host metrics,
# which are fractions), so 96 means 96%. Grouped by host.name + container.name, which is
# why this one reaches the highest member counts of any rule here.
. "$(dirname "$0")/_common.sh"

ALERT="High Container CPU"
TAG=ctr
SIGNAL=container
OK=3               # 3%
BAD=96             # PERCENT already — do not divide by 100
NOTE="container.cpu.utilization is already a percent on the wire; 96 = 96%"

parse_args "$@"
run_alert
