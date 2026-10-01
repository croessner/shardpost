#!/usr/bin/env bash
# Cluster smoke test of the learning queue indexes (scaling stage R2a),
# run after a rollout. Uses only shardpostctl and shardpost inside a
# server pod:
#
#   1. run the learning queue backfill and print its completion marker,
#   2. print the queue summary (entries, due, dead, owner entries) and
#      check it against the admin API's learning list,
#   3. list the jobs of a throwaway account through the admin API,
#   4. print the queue, index and listing metrics; with
#      imapsieve.learning.index=read the learning sites must not grow
#      while the worker runs.
#
#   hack/learning-smoke.sh [<throwaway-account>]
#
# It enqueues nothing and retries nothing: a learning job runs the
# configured learning program (rspamd learn) on a real message, so a
# test job would train the live classifier. The agreed throwaway account
# (default rbdpoc2@exampleserver.de) only scopes the list.
# `shardpost index …` runs without --config and relies on SHARDPOST_CONFIG
# being set in the server container (as the overlays do).
set -euo pipefail

CONTEXT=${CONTEXT:-croessner}
NS=${NS:-shardpost}
DEPLOY=${DEPLOY:-deploy/shardpost-server}
ACCOUNT=${1:-rbdpoc2@exampleserver.de}

inpod() { kubectl --context "$CONTEXT" -n "$NS" exec -i "$DEPLOY" -c server -- "$@"; }
ctl() { inpod shardpostctl "$@"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

# field <name> reads "name": value from indented JSON on stdin.
field() { sed -n "s/^ *\"$1\": \"\{0,1\}\([^\",]*\)\"\{0,1\},\{0,1\}$/\1/p" | head -1; }

metrics() {
	inpod sh -c 'wget -qO- http://127.0.0.1:8080/metrics 2>/dev/null || curl -s http://127.0.0.1:8080/metrics'
}

# listings <site> prints imapceph_rados_listings_total{site} of the pod (0
# when absent).
listings() { metrics | sed -n "s/^imapceph_rados_listings_total{site=\"$1\"} \(.*\)$/\1/p" | head -1 | grep . || echo 0; }

echo "== learning queue backfill =="
inpod shardpost index backfill learning
shown=$(inpod shardpost index show learning)
echo "$shown"
[ "$(echo "$shown" | field configured_mode)" != off ] || fail "imapsieve.learning.index is off"
echo "$shown" | grep -q '"finished"' || fail "no completion marker after the backfill"

entries=$(echo "$shown" | sed -n '/"queue": {/,/}/p' | field entries)
owners=$(echo "$shown" | field owners)
[ "$entries" = "$owners" ] || echo "note: $entries queue entries, $owners owner entries (a job may be in flight)"

echo; echo "== admin API learning list =="
listed=$(ctl learning list --json -limit 500 | grep -c '"id":' || true)
echo "jobs listed: $listed, queue entries: $entries"
[ "$listed" -le "$((entries + 5))" ] || fail "the admin list shows more jobs than the queue holds"

echo; echo "== jobs of $ACCOUNT =="
ctl learning list -account "$ACCOUNT" || fail "account-scoped list failed"

mode=$(echo "$shown" | field mode)
if [ "$mode" = read ]; then
	echo; echo "== no learning listings in read mode =="
	before=$(listings learning_all)
	sleep 5 # the worker passes five times
	after=$(listings learning_all)
	[ "$before" = "$after" ] || fail "learning_all listings grew in read mode ($before -> $after)"
	echo "learning_all listings: $after (unchanged)"
fi

echo; echo "== queue, index and listing metrics =="
metrics | grep -E '^(imapceph_(queue_ops_total|queue_stale_entries_total|kv_index_errors_total|rados_listings_total)|imap_imapsieve_learning_(total|wakeups_total))' ||
	echo "(metrics endpoint not reachable from the pod)"

echo; echo "OK: learning queue smoke test passed (mode $mode)"
