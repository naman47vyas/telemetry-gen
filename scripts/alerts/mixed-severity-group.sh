#!/usr/bin/env bash
# "High CPU usage for host"  —  one group, two severities, and members that move between
# them. This is the script for looking at how the UI renders a MIXED group, not for
# checking that a rule fires at all; every other script in here does the latter.
#
# The shape it makes, by default 20 critical + 20 warning hosts:
#
#   10:12  CRITICAL   96%  <group>  [40 resources]      <- one row, one notification
#     10:12  CRITICAL   96%  ma-sevmix-…-host-01        <- criticals first, worst value first
#     …                                                 (20 of them)
#     10:12  WARNING    87%  ma-sevmix-…-host-21        <- then the warnings
#     …                                                 (20 of them)
#
# Both severities live in ONE notification because the engine buckets warning and critical
# together as "firing" and the group's status is its worst member. That is the whole point
# of the run: the 40 hosts never split into a Critical notification and a separate Warning
# one, and an escalation is the same row changing rather than a second incident.
#
# WHAT MOVES. Every SEV_FLAP the split walks SEV_WAVE — 20c/20w -> 30c/10w -> 20c/20w ->
# 10c/30w, and round again. The same 40 hosts breach the whole time; only their severity
# changes, so members escalate and de-escalate inside a group whose membership never moves.
# SEV_FLAP must be longer than the rule's evaluation window (5 minutes on the host rules)
# or each host just averages 96% and 87% over the window and settles on neither; the
# generator says so if you go below five minutes. Allow a window after each step for the
# badges to catch up.
#
# THINGS WORTH SETTING WHEN YOU ARE LOOKING AT THE LIST:
#
#   WARN_PCT=88 ./mixed-severity-group.sh      # 5 critical + 35 warning
#
# The collapsed row previews its first 10 members, ordered worst-first. At the default
# 20/20 those ten are ALL critical, so the row claims 20 warning and shows you none of
# them until you expand it — worth seeing, since it is what a real 20/20 group looks like.
# At 5/35 the preview holds both badges. Expanded pages hold 30, so the default run is
# 20 critical + 10 warning on page 1 and the last 10 warnings on page 2.
#
#   N=80 ./mixed-severity-group.sh             # 40/40, two full pages
#   SEV_FLAP=0 ./mixed-severity-group.sh       # hold the split still
#   SEV_WAVE=1,0,2 ./mixed-severity-group.sh   # mixed -> all critical -> all warning
#
# SEV_WAVE=1,0,2 is the one to run if you want to watch the GROUP's own status change:
# with no critical members left the row itself drops to WARNING, then climbs back, without
# ever becoming a second notification. The banner spells the whole cycle out as counts
# before anything is sent — read it, because a large WARN_PCT pushes a step of the default
# wave past the end of the cohort and drops that step to zero criticals on its own.
#
# The values are the usual host CPU fractions, sat either side of the rule's bands: warning
# is 85-90, critical is above 90, so 87% reads WARNING and 96% reads CRITICAL. A member only
# gets a badge if its value lands in the right band — if yours are set somewhere else, BAD
# and WARN are env knobs, not an edit.
. "$(dirname "$0")/_common.sh"

ALERT="High CPU usage for host"
TAG=sevmix
SIGNAL=host.cpu
OK=${OK:-0.03}      # ~3%, for anything left outside the breaching cohort
BAD=${BAD:-0.96}    # FRACTION on the wire: 0.96 -> 96%, over the 90% critical
WARN=${WARN:-0.87}  # FRACTION: 87%, inside the 85-90 warning band
NOTE="system.cpu.utilization is a fraction; 0.96 = 96% after the rule's x100"

# 40 hosts split down the middle is the smallest cohort that shows every wrinkle: more
# criticals than the 10-member preview, and more members than one 30-row page.
DEFAULT_N=40
DEFAULT_DURATION=90m
WARN_PCT=${WARN_PCT:-50}
SEV_FLAP=${SEV_FLAP:-10m}
SEV_WAVE=${SEV_WAVE:-1,0.5,1,1.5}
SPREAD="one notification, both severities inside it"

parse_args "$@"
run_alert
