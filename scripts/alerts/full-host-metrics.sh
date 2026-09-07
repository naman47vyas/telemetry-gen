#!/usr/bin/env bash
# Whole hosts, not one series  —  N synthetic machines emitting every metric the
# OpenTelemetry hostmetrics receiver collects by default.
#
# The rest of this directory emits the single series its rule evaluates, which fires the
# alert and leaves an empty host page behind it. This one emits all 29 metrics — about 100
# datapoints per host per interval — so the host list, the host detail pages and any widget
# built on host metrics have something real underneath:
#
#   cpu          system.cpu.utilization + system.cpu.time across all 8 states, logical.count
#   memory       system.memory.utilization + usage across all 7 states
#   disk         io, operations, merged, io_time, operation_time, weighted_io_time,
#                pending_operations — two devices (sda, sdb), read and write
#   filesystem   usage + inodes.usage (used/free/reserved) + utilization, on / and /var
#   load         load_average 1m / 5m / 15m, tracking the CPU
#   network      io, packets, errors, dropped on eth0, plus connections by TCP state
#   paging       usage + utilization on swap, faults and operations by type and direction
#   processes    count by status, created
#
# Names, instrument types, units and attribute values are the receiver's own, taken from its
# scrapers' documentation.md — not invented here. The counters are genuine cumulative sums
# that climb from the start of the run, so rate charts need a couple of intervals before
# they show anything.
#
# WHAT THE VALUES MEAN. OK / WARN / BAD are the CPU busy FRACTION, exactly as in
# high-cpu-usage-for-host.sh: 0.96 is a host at 96%. They drive the four states that rule
# sums (user + system + wait + steal), which are made to add up to exactly the value you
# ask for, and the load average follows along. Everything else — memory at 54-71%, disks,
# filesystems, network — sits at a plausible level that differs from host to host and does
# NOT follow the tiers. Memory in particular stays well under the 85% warning floor, so
# this run does not also trip the memory rule; use high-memory-usage-for-host.sh for that.
#
#   ./full-host-metrics.sh                  # 25 hosts, 17 critical + 8 warning on CPU
#   HEALTHY=1 ./full-host-metrics.sh        # 25 hosts that just exist, firing nothing
#   HEALTHY=1 ./full-host-metrics.sh 60 4h  # a 60-host fleet for the afternoon
#   WARN_N=0 ./full-host-metrics.sh         # all critical, as the other scripts ship
#
# HEALTHY=1 is the one to reach for when the point is populating pages rather than firing
# an alert: every host sits at OK and nothing breaches.
#
# ON SIZE. A host here is ~100 datapoints, where a host in the other scripts is one, so N
# costs a hundred times more: 25 hosts is 2500 datapoints an interval, 200 hosts is 20000.
# The sender chunks on datapoints rather than resources so the requests stay under gRPC's
# limit either way, but keep N modest unless you are deliberately load-testing.
. "$(dirname "$0")/_common.sh"

ALERT="High CPU usage for host"
TAG=hostall
SIGNAL=host.full
OK=${OK:-0.05}      # FRACTION: 5%, an idle-ish box rather than a dead one
BAD=${BAD:-0.96}    # FRACTION: 96%, over the 90% critical
WARN=${WARN:-0.87}  # FRACTION: 87%, inside the 85-90 warning band
NOTE="OK/WARN/BAD are the CPU busy fraction; every other metric is plausible, not tiered"

# Fewer hosts than the other scripts by default: each one is a hundred datapoints, and the
# point here is depth per host rather than a big group in one notification.
DEFAULT_N=25
DEFAULT_DURATION=60m
SPREAD="29 metrics, ~100 datapoints per host per $INTERVAL"

parse_args "$@"
run_alert
