#!/usr/bin/env bash
# "High CPU utilization for node"  —  N k8s nodes pinned at 96% CPU.
#
# The rule divides: k8s.node.cpu.utilization / k8s.node.allocatable_cpu * 100. Emitting
# allocatable = 100 makes BAD a plain percent, so 96 means 96%. Both series must be
# present per node or the ratio has no row.
. "$(dirname "$0")/_common.sh"

ALERT="High CPU utilization for node"
TAG=node
SIGNAL=k8s.node
OK=10              # 10%
BAD=96             # PERCENT, because allocatable_cpu is emitted as 100
NOTE="cpu.utilization / allocatable_cpu * 100, with allocatable pinned to 100"

# The rule groups by cluster + node, so spreading the nodes over several clusters widens
# the group keys. Namespaces do not apply to a node-level metric.
CLUSTERS=${CLUSTERS:-3}
EXTRA=(-clusters "$CLUSTERS")
SPREAD="$CLUSTERS clusters"

parse_args "$@"
run_alert
