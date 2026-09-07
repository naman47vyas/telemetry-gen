#!/usr/bin/env bash
# "Container is not running"  —  N containers reporting a non-running status.
#
# container.status is an enum, not a percent. 1 = running (healthy), 3 = not running
# (breaching). Same container resource shape as the CPU rule, different metric, so
# METRIC is overridden here.
#
# No warning tier here either: container.status is an enum, and there is no value between
# running and not running. Every member of the group is critical.
. "$(dirname "$0")/_common.sh"

ALERT="Container is not running"
TAG=cstat
SIGNAL=container
METRIC=container.status
OK=${OK:-1}         # running
BAD=${BAD:-3}       # not running
NOTE="container.status is an enum (1=running, 3=not running), not a percent"

parse_args "$@"
run_alert
