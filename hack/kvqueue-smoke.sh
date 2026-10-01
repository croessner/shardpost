#!/usr/bin/env bash
# Cluster smoke test of the admin store indexes and due queues (scaling
# stage R1b), run after a rollout. Uses only shardpostctl and shardpost
# inside a server pod:
#
#   1. run the admin store backfill and print its completion marker,
#   2. submit a read-only index check of a throwaway account with an
#      idempotency key, wait for it, and submit it again (same job),
#   3. show the job's record, hint index entry and queue entries: no
#      claimable entry (it ended), expiry entries of the job and of its
#      idempotency key at generation 1 (Finished + retention),
#   4. print the queue and index metrics.
#
#   hack/kvqueue-smoke.sh <throwaway-account>
#
# The job only reads the account (index_check), so any account with a
# mailbox will do; the agreed throwaway account is the safe choice.
# `shardpost index …` runs without --config and relies on SHARDPOST_CONFIG
# being set in the server container (as the overlays do).
set -euo pipefail

CONTEXT=${CONTEXT:-croessner}
NS=${NS:-shardpost}
DEPLOY=${DEPLOY:-deploy/shardpost-server}
ACCOUNT=${1:?usage: $0 <throwaway-account>}
KEY="kvqueue-smoke-$(date +%s)"

inpod() { kubectl --context "$CONTEXT" -n "$NS" exec -i "$DEPLOY" -c server -- "$@"; }
ctl() { inpod shardpostctl "$@"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

# field <name> reads "name": value from indented JSON on stdin.
field() { sed -n "s/^ *\"$1\": \"\{0,1\}\([^\",]*\)\"\{0,1\},\{0,1\}$/\1/p" | head -1; }

echo "== admin store backfill =="
inpod shardpost index backfill kv
marker=$(inpod shardpost index show kv)
echo "$marker"
[ "$(echo "$marker" | field mode)" != off ] || fail "admin.kv_index is off"
echo "$marker" | grep -q '"finished"' || fail "no completion marker after the backfill"

echo; echo "== read-only job with an idempotency key =="
job=$(ctl check --json -wait -idempotency-key "$KEY" "$ACCOUNT") || fail "index check did not succeed"
id=$(echo "$job" | field id)
[ -n "$id" ] || fail "no job id"
echo "job $id: $(echo "$job" | field state)"

again=$(ctl check --json -idempotency-key "$KEY" "$ACCOUNT" | field id)
[ "$again" = "$id" ] || fail "the idempotency key started a second job ($again)"

echo; echo "== record, index and queue entries of the job =="
shown=$(inpod shardpost index show kv "job/$id")
echo "$shown"
echo "$shown" | grep -q '"record": true' || fail "job record not found"
echo "$shown" | grep -q '"index": {' || fail "no hint index entry"
if echo "$shown" | grep -q '"queue": "jobs/claimable"'; then
	fail "an ended job still has a claimable entry"
fi
[ "$(echo "$shown" | grep -c '"queue": "jobs/expiry"')" -ge 2 ] || fail "expiry entries of job and idempotency key missing"
[ "$(echo "$shown" | grep -c '"gen": 1$')" -ge 2 ] || fail "expiry entries not moved to Finished + retention"

echo; echo "== queue and index metrics =="
inpod sh -c 'wget -qO- http://127.0.0.1:8080/metrics 2>/dev/null || curl -s http://127.0.0.1:8080/metrics' |
	grep -E '^imapceph_(queue_ops_total|queue_stale_entries_total|kv_index_errors_total|job_wakeups_total|rados_listings_total)' ||
	echo "(metrics endpoint not reachable from the pod)"

echo; echo "OK: admin store queue smoke test passed (job $id on $ACCOUNT)"
