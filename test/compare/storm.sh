#!/usr/bin/env bash
# Branch switch storm test: start a watcher, check out a commit far away and
# back again, and record every redis command the watcher sends.
#
# usage: storm.sh <magento-dir> <redis-socket> <out-dir> <rev> <tool-cmd...>
set -u
WT=$1; SOCK=$2; OUT=$3; REV=$4; shift 4
WAIT=${WAIT:-45}
R() { redis-cli -s "$SOCK" "$@" >/dev/null; }
mkdir -p "$OUT"
HEAD=$(git -C "$WT" rev-parse HEAD)

redis-cli -s "$SOCK" monitor > "$OUT/monitor.txt" &
MON=$!
sleep 0.5
"$@" > "$OUT/tool.log" 2>&1 < /dev/null &
TOOL=$!
sleep 8
R echo "MARK checkout-away"
start=$(date +%s)
git -C "$WT" checkout -q --detach "$REV"
echo "checkout away took $(( $(date +%s) - start ))s, $(git -C "$WT" diff --name-only "$REV" "$HEAD" | wc -l) files changed" > "$OUT/info.txt"
sleep "$WAIT"
R echo "MARK checkout-back"
git -C "$WT" checkout -q --detach "$HEAD"
sleep "$WAIT"
R echo "MARK end"
cpu=$(ps -o time= -p $TOOL | tr -d ' ')
echo "tool cpu time: $cpu" >> "$OUT/info.txt"
kill -INT $TOOL 2>/dev/null; sleep 1; kill -9 $TOOL 2>/dev/null
kill $MON 2>/dev/null
wait 2>/dev/null
cat "$OUT/info.txt"
