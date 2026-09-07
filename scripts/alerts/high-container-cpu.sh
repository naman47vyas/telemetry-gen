#!/usr/bin/env bash
# "High Container CPU"  —  N containers pinned at 96% CPU.
#
# container.cpu.utilization is ALREADY a percent on the wire (unlike the host metrics,
# which are fractions), so 96 means 96%. Grouped by host.name + container.name, which is
# why this one reaches the highest member counts of any rule here.
#
# The group is split by severity: two thirds of the entities sit at BAD (critical) and the
# rest at WARN, which is over the rule's warning threshold but under its critical one, so
# one notification carries both severities. WARN_N moves the line; WARN_N=0 makes the whole
# cohort critical again.
. "$(dirname "$0")/_common.sh"

ALERT="High Container CPU"
TAG=ctr
SIGNAL=container
OK=${OK:-3}         # 3%
BAD=${BAD:-96}      # PERCENT already — do not divide by 100
WARN=${WARN:-87}    # PERCENT: inside the 85-90 warning band
NOTE="container.cpu.utilization is already a percent on the wire; 96 = 96%"

parse_args "$@"
run_alert
