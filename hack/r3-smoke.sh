#!/usr/bin/env bash
# Cluster smoke test of scaling stage R3 (intents, scrub without
# listings), run after a rollout:
#
#   1. on the throwaway account, over IMAP: create a temporary mailbox,
#      append messages, expunge one (removal intents) and delete the
#      mailbox (deletion tombstone, retired header),
#   2. scrub the account as a single job with every repair class and read
#      the result: with scrub.orphan_source "shadow" (the default of
#      0.42.0) the listing and the intents must agree — orphan_extra must
#      be 0 at once; differences the listing finds that no intent names
#      (debris from before 0.42.0) must be gone after at most three runs,
#      since the listing removes them (orphan blobs on its second run);
#      with "intents" no intent may stay open and nothing may fail,
#   3. print the scrub and listing metrics of the pod.
#
#   hack/r3-smoke.sh [<throwaway-account>]
#
# The credentials come from the secret imaptest-creds (keys user and
# pass); the password is read inside this script, handed to curl as a
# config on stdin and never printed or passed as an argument. The
# secret's user must be the throwaway account (default
# rbdpoc2@exampleserver.de). IMAP runs from the mailtest pod straight to
# the service; the scrub runs through shardpostctl in a server pod.
#
# Purge is not tested: a purge needs an account that exists only for the
# test, and the admin API creates data only for accounts Nauthilus knows
# (deliver and import resolve the account first). The purge without a
# listing is covered by the store tests (make test-docker) instead.
set -euo pipefail

CONTEXT=${CONTEXT:-croessner}
NS=${NS:-shardpost}
DEPLOY=${DEPLOY:-deploy/shardpost-server}
TOOLS=${TOOLS:-deploy/mailtest}
IMAP_URL=${IMAP_URL:-imap://shardpost:143/}
ACCOUNT=${1:-rbdpoc2@exampleserver.de}
BOX=${BOX:-r3-smoke-$(date +%s)}

K="kubectl --context $CONTEXT -n $NS"

fail() { echo "FAIL: $*" >&2; exit 1; }
inpod() { $K exec -i "$DEPLOY" -c server -- "$@"; }
ctl() { inpod shardpostctl "$@"; }

# json <python expression over j> evaluates the expression on the JSON
# document on stdin.
json() { python3 -c "import json,sys; j=json.load(sys.stdin); print($1)"; }

secret() { $K get secret imaptest-creds -o "jsonpath={.data.$1}" | base64 -d; }

login=$(secret user)
[ "$login" = "$ACCOUNT" ] || fail "the secret imaptest-creds holds another account than $ACCOUNT"

# curlcfg prints a curl config with the credentials (to stdin of curl
# only).
curlcfg() {
	printf 'user = "%s:' "$login"
	secret pass | sed 's/[\\"]/\\&/g' | tr -d '\n'
	printf '"\n'
}

# imap <url path> <curl args...> runs one IMAP request from the tools
# pod. A message to append is written by the pod itself (-T /tmp/r3.eml).
imap() {
	# shellcheck disable=SC2016 # expanded by the shell in the tools pod
	curlcfg | $K exec -i "$TOOLS" -- sh -c '
		cfg=$(cat)
		url=$1
		shift
		printf "From: r3-smoke@example.org\r\nTo: r3-smoke@example.org\r\nSubject: r3 smoke %s\r\n\r\nIntents smoke test, removed again.\r\n" "$(date +%s)-$$" >/tmp/r3.eml
		printf "%s\n" "$cfg" | curl -sS -K - --ssl-reqd --insecure "$url" "$@"' imap "$IMAP_URL$1" "${@:2}"
}

echo "== temporary mailbox $BOX on $ACCOUNT =="
imap "" -X "CREATE \"$BOX\"" >/dev/null || fail "CREATE $BOX"

for _ in 1 2 3; do
	imap "$BOX" -T /tmp/r3.eml >/dev/null || fail "APPEND to $BOX"
done

imap "$BOX" -X 'STORE 1 +FLAGS (\Deleted)' >/dev/null || fail "STORE in $BOX"
imap "$BOX" -X 'EXPUNGE' >/dev/null || fail "EXPUNGE in $BOX"
imap "" -X "DELETE \"$BOX\"" >/dev/null || fail "DELETE $BOX"
echo "created, appended 3, expunged 1, deleted"

# scrub runs one scrub job on the account and prints its result.
scrub() {
	local plan id job
	plan=$(ctl plan --json scrub "$ACCOUNT") || fail "plan scrub"
	id=$(echo "$plan" | json 'j["id"]')
	job=$(ctl apply --json -wait -poll 2s "$id") || fail "scrub job failed: $job"
	echo "$job" | json 'json.dumps(j["result"])'
}

# summary prints the counters of a scrub result that matter here.
summary() {
	json '" ".join("%s=%s" % (k, j.get(k, 0)) for k in ("OrphanSource", "OrphanBlobs", "OrphanCandidates", "UnknownShards",
		"IntentsOpen", "IntentsDropped", "DeletionsFinished", "CreationsUndone", "StaleMarkObjects", "StaleMetadata")) +
		" shadow=" + json.dumps(j.get("Shadow")) + " errors=" + json.dumps(j.get("Errors"))'
}

echo; echo "== scrub of $ACCOUNT =="
clean=0

for run in 1 2 3; do
	res=$(scrub)
	echo "run $run: $(echo "$res" | summary)"

	[ "$(echo "$res" | json 'len(j.get("Errors") or [])')" = 0 ] || fail "the scrub reported errors"

	source=$(echo "$res" | json 'j.get("OrphanSource", "")')
	extra=$(echo "$res" | json '(j.get("Shadow") or {}).get("OrphansExtra", 0)')
	[ "$extra" = 0 ] || fail "the intents take $extra blobs for orphans that the listing does not see"

	case "$source" in
	shadow)
		diff=$(echo "$res" | json 'sum((j.get("Shadow") or {}).get(k, 0) for k in ("OrphansMissed", "UnknownMissed", "MarksMissed"))')
		if [ "$diff" = 0 ]; then
			clean=1
			break
		fi

		echo "  $diff objects without an intent (debris from before 0.42.0 is removed by the listing; looking again)"
		;;
	intents)
		if [ "$(echo "$res" | json 'j.get("IntentsOpen", 0)')" = 0 ]; then
			clean=1
			break
		fi
		;;
	*)
		echo "note: scrub.orphan_source is \"$source\"; nothing to compare"
		clean=1
		break
		;;
	esac
done

[ "$clean" = 1 ] || fail "listing and intents still differ after three runs"

echo; echo "== scrub and listing metrics =="
inpod sh -c 'wget -qO- http://127.0.0.1:8080/metrics 2>/dev/null || curl -s http://127.0.0.1:8080/metrics' |
	grep -E '^imapceph_(scrub_shadow_differences_total|scrub_findings_total|rados_listings_total)' ||
	echo "(no such series yet, or the metrics endpoint is not reachable from the pod)"

echo; echo "OK: R3 smoke test passed (orphan source $source; purge not tested, see the header)"
