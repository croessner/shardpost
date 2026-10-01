#!/usr/bin/env bash
# Cluster smoke test of scaling stage R2b, run after a rollout. Uses only
# shardpostctl inside a server pod (the socket caller has the scope of
# all accounts) and parses the JSON answers locally with python3:
#
#   1. page through the account directory (account list) and check that
#      the pages join up and hold the throwaway account,
#   2. run a read-only index_check over all accounts as one batch job
#      (plan -all, apply -wait) and check its counters,
#   3. run the batch scrub with skip_idle twice: the first scrubs every
#      account and records its state, the second must skip the accounts
#      that did not change in between (SCRUB=0 leaves this out),
#   4. print the batch and watch metrics of the pod (the watch gauge
#      appears with the first IDLE or NOTIFY session of the pod).
#
#   hack/r2b-smoke.sh [<throwaway-account>]
#
# The index_check only reads. The batch scrub is the scrub that runs every
# night (the same repairs, guarded by scrub.grace_seconds and the two-run
# rule); a second run on the same day only brings its work forward. The
# account directory must have completed its backfill, or the batches are
# refused (exit 65). The agreed throwaway account (default
# rbdpoc2@exampleserver.de) must be in the directory.
set -euo pipefail

CONTEXT=${CONTEXT:-croessner}
NS=${NS:-shardpost}
DEPLOY=${DEPLOY:-deploy/shardpost-server}
ACCOUNT=${1:-rbdpoc2@exampleserver.de}
SCRUB=${SCRUB:-1}
PAGE=${PAGE:-3}

inpod() { kubectl --context "$CONTEXT" -n "$NS" exec -i "$DEPLOY" -c server -- "$@"; }
ctl() { inpod shardpostctl "$@"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

# json <python expression over j> evaluates the expression on the JSON
# document on stdin.
json() { python3 -c "import json,sys; j=json.load(sys.stdin); print($1)"; }

metrics() {
	inpod sh -c 'wget -qO- http://127.0.0.1:8080/metrics 2>/dev/null || curl -s http://127.0.0.1:8080/metrics'
}

# batch <plan args...> plans a batch, applies it, waits and prints the
# job JSON (an exit status 1, partially failed, is reported, not fatal).
batch() {
	local plan id job
	plan=$(ctl plan --json -all "$@") || fail "plan -all $*"
	id=$(echo "$plan" | json 'j["id"]')
	echo "plan $id: $(echo "$plan" | json 'j["summary"]')" >&2
	job=$(ctl apply --json -wait -poll 5s "$id") || [ $? -eq 1 ] || fail "batch $* failed: $job"
	echo "$job"
}

# counters prints the counters of a batch result.
counters() {
	json '" ".join("%s=%s" % (k, j["result"].get(k, 0)) for k in ("accounts", "done", "skipped", "gone", "failed", "findings"))'
}

echo "== account list (pages of $PAGE) =="
first=$(ctl account list --json -limit "$PAGE")
n1=$(echo "$first" | json 'len(j["items"])')
next=$(echo "$first" | json 'j.get("next", "")')
[ "$n1" -gt 0 ] || fail "the directory is empty"
echo "page 1: $n1 accounts, next ${next:-(none)}"

if [ -n "$next" ]; then
	second=$(ctl account list --json -limit "$PAGE" -after "$next")
	overlap=$(python3 -c 'import json,sys; a=json.loads(sys.argv[1]); b=json.loads(sys.argv[2]);
print(len({i["account"] for i in a["items"]} & {i["account"] for i in b["items"]}))' "$first" "$second")
	[ "$overlap" -eq 0 ] || fail "page 2 repeats $overlap accounts of page 1"
	echo "page 2: $(echo "$second" | json 'len(j["items"])') accounts"
fi

all=$(ctl account list --json -all -limit 500)
total=$(echo "$all" | grep -c '"account"' || true)
echo "$all" | grep -q "\"account\":\"$ACCOUNT\"" || fail "$ACCOUNT is not in the directory"
purging=$(echo "$all" | grep -c '"state":"purging"' || true)
echo "directory: $total accounts, $purging purging"

echo; echo "== index_check over all accounts =="
job=$(batch index_check)
echo "$job" | json 'j["kind"], j["account"] or "(all accounts)", j["state"]'
echo "$job" | counters
[ "$(echo "$job" | json 'j["result"]["accounts"]')" -ge "$((total - purging))" ] || fail "the batch looked at fewer accounts than the directory holds"
echo "$job" | json '"\n".join("finding: " + a for a in j["result"].get("finding_accounts", []))'
echo "$job" | json '"\n".join("error: %s: %s" % (e["account"], e["error"]) for e in j["result"].get("errors", []))'

if [ "$SCRUB" = 1 ]; then
	echo; echo "== batch scrub with skip_idle, twice =="
	one=$(batch -skip-idle scrub)
	echo "first:  $(echo "$one" | counters)"
	two=$(batch -skip-idle scrub)
	echo "second: $(echo "$two" | counters)"
	skipped=$(echo "$two" | json 'j["result"]["skipped"]')
	[ "$skipped" -gt 0 ] || fail "the second batch scrub skipped no account"
	ctl account directory "$ACCOUNT" | grep -E 'last scrub|entry'
fi

echo; echo "== batch and watch metrics =="
metrics | grep -E '^imapceph_(batch_accounts_total|imap_watches|imap_watch_subscribers|imap_watch_rearms_total|imap_watch_refused_total|rados_listings_total)' ||
	echo "(no such series yet, or the metrics endpoint is not reachable from the pod)"

echo; echo "OK: R2b smoke test passed"
