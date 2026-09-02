#!/usr/bin/env bash
# "Pods are failing"  —  N pods reporting phase = Failed.
#
# k8s.pod.phase is a raw enum, NOT a percentage: 2 = Running, 4 = Failed. The rule is
# critical above 3, so 4 breaches and 2 is healthy. Never scale these values.
. "$(dirname "$0")/_common.sh"

ALERT="Pods are failing"
TAG=pod
SIGNAL=k8s.pod
OK=2               # Running
BAD=4              # Failed
NOTE="k8s.pod.phase is an enum (2=Running, 4=Failed), not a percent — crit is > 3"

# Spread the entities over a cluster x namespace grid. The rule groups by
# cluster + namespace + pod + container, so this widens the group keys inside the single
# notification rather than adding more notifications.
CLUSTERS=${CLUSTERS:-3}
NAMESPACES=${NAMESPACES:-5}
EXTRA=(-clusters "$CLUSTERS" -hosts "$NAMESPACES")
SPREAD="$CLUSTERS clusters x $NAMESPACES namespaces"

parse_args "$@"
run_alert
