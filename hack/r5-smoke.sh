#!/usr/bin/env bash
# Cluster smoke test of scaling stage R5 (delayed JMAP sending), run
# after a rollout with jmap.submission.delayed.max_delay_seconds set:
#
#   1. read the session of the throwaway account and check that
#      maxDelayedSend allows the delay and FUTURERELEASE is advertised,
#   2. create two drafts to the account itself and submit both with
#      HOLDFOR (about a minute), destroying the drafts on success (the
#      queue sends the copy taken at creation),
#   3. check that both are pending, in JMAP and in shardpostctl
#      submission list, then cancel the second one,
#   4. wait until the first is final and has arrived in the INBOX, and
#      check that the second did not arrive (30 s past its release time),
#   5. remove the arrived test message and the submission records, and
#      print the delayed submission metrics of every pod.
#
#   hack/r5-smoke.sh [<throwaway-account>]
#
# The credentials come from the secret imaptest-creds (keys user and
# pass); the password is read inside this script, handed to curl as a
# config on stdin and never printed or passed as an argument. The
# secret's user must be the throwaway account (default
# rbdpoc2@exampleserver.de); the mail goes from it to itself, through
# the relay. JMAP runs from the mailtest pod straight to the in-cluster
# JMAP service, not through the Director.
set -euo pipefail

CONTEXT=${CONTEXT:-croessner}
NS=${NS:-shardpost}
DEPLOY=${DEPLOY:-deploy/shardpost-server}
TOOLS=${TOOLS:-deploy/mailtest}
SELECTOR=${SELECTOR:-app.kubernetes.io/name=shardpost-server}
JMAP_URL=${JMAP_URL:-https://shardpost-jmap:9443}
ACCOUNT=${1:-rbdpoc2@exampleserver.de}
# HOLD is the delay asked for in seconds; ARRIVAL how long the first
# message may take after its release time to reach the INBOX.
HOLD=${HOLD:-60}
ARRIVAL=${ARRIVAL:-300}

K="kubectl --context $CONTEXT -n $NS"
USING='["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail","urn:ietf:params:jmap:submission"]'
TOKEN=r5-smoke-$(date +%s)-$$

fail() { echo "FAIL: $*" >&2; exit 1; }
ctl() { $K exec -i "$DEPLOY" -c server -- shardpostctl "$@"; }

# py <python expression over j and the environment> evaluates the
# expression on the JSON document on stdin.
py() { python3 -c "import json,os,sys; j=json.load(sys.stdin); e=os.environ; print($1)"; }

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

# get <path> fetches a JMAP resource.
get() {
	{
		curlcfg
		printf 'url = "%s%s"\n' "$JMAP_URL" "$1"
	} | $K exec -i "$TOOLS" -- curl -sS -K - --insecure --fail-with-body
}

# api <method calls as JSON> posts one API request and prints the
# response; the body goes in the config, escaped for it.
api() {
	local body
	body=$(printf '{"using":%s,"methodCalls":%s}' "$USING" "$1")
	{
		curlcfg
		printf 'url = "%s/jmap/api/"\n' "$JMAP_URL"
		printf 'header = "Content-Type: application/json"\n'
		printf 'data-binary = "%s"\n' "$(printf '%s' "$body" | sed 's/[\\"]/\\&/g')"
	} | $K exec -i "$TOOLS" -- curl -sS -K - --insecure --fail-with-body
}

# pods lists the serving pods.
pods() {
	$K get pods -l "$SELECTOR" --field-selector=status.phase=Running \
		-o go-template='{{range .items}}{{if not .metadata.deletionTimestamp}}pod/{{.metadata.name}}{{"\n"}}{{end}}{{end}}'
}

echo "== session of $ACCOUNT =="
session=$(get /.well-known/jmap) || fail "session"
export ACCOUNT HOLD TOKEN
read -r acct max ext < <(echo "$session" | py '" ".join([j["primaryAccounts"]["urn:ietf:params:jmap:submission"],
	str(j["accounts"][j["primaryAccounts"]["urn:ietf:params:jmap:submission"]]["accountCapabilities"]["urn:ietf:params:jmap:submission"]["maxDelayedSend"]),
	",".join(j["accounts"][j["primaryAccounts"]["urn:ietf:params:jmap:submission"]]["accountCapabilities"]["urn:ietf:params:jmap:submission"]["submissionExtensions"].get("FUTURERELEASE", ["none"]))])')
echo "account $acct, maxDelayedSend $max, FUTURERELEASE $ext"
[ "$max" -ge "$HOLD" ] || fail "maxDelayedSend $max is below $HOLD (jmap.submission.delayed.max_delay_seconds not set?)"
[ "$ext" != none ] || fail "FUTURERELEASE is not advertised"
export ACCT=$acct

echo; echo "== identity and mailboxes =="
res=$(api "$(py '"[[\"Identity/get\",{\"accountId\":%s},\"i\"],[\"Mailbox/get\",{\"accountId\":%s,\"properties\":[\"role\",\"name\"]},\"m\"]]" % (json.dumps(e["ACCT"]), json.dumps(e["ACCT"]))' <<<'{}')")
ident=$(echo "$res" | py '[i["id"] for i in j["methodResponses"][0][1]["list"] if i["email"].lower() == e["ACCOUNT"].lower()][0]') ||
	fail "no identity for $ACCOUNT"
inbox=$(echo "$res" | py '[m["id"] for m in j["methodResponses"][1][1]["list"] if m.get("role") == "inbox"][0]')
drafts=$(echo "$res" | py '([m["id"] for m in j["methodResponses"][1][1]["list"] if m.get("role") == "drafts"] or [""])[0]')
[ -n "$drafts" ] || drafts=$inbox
export IDENT=$ident INBOX=$inbox DRAFTS=$drafts
echo "identity $ident, inbox $inbox, drafts $drafts"

echo; echo "== two drafts, submitted with HOLDFOR=$HOLD =="
# shellcheck disable=SC2016 # "$draft" is a JMAP keyword, not a variable
calls=$(py '"[" + ",".join([
	json.dumps(["Email/set", {"accountId": e["ACCT"], "create": {
		k: {"mailboxIds": {e["DRAFTS"]: True}, "keywords": {"$draft": True},
			"from": [{"email": e["ACCOUNT"]}], "to": [{"email": e["ACCOUNT"]}], "subject": e["TOKEN"] + "-" + k,
			"bodyValues": {"b": {"value": "Delayed sending smoke test (" + k + "), removed again."}},
			"textBody": [{"partId": "b", "type": "text/plain"}]} for k in ("sent", "canceled")}}, "e"]),
	json.dumps(["EmailSubmission/set", {"accountId": e["ACCT"], "create": {
		k: {"identityId": e["IDENT"], "emailId": "#" + k,
			"envelope": {"mailFrom": {"email": e["ACCOUNT"], "parameters": {"HOLDFOR": e["HOLD"]}},
				"rcptTo": [{"email": e["ACCOUNT"]}]}} for k in ("sent", "canceled")},
		"onSuccessDestroyEmail": ["#sent", "#canceled"]}, "s"])]) + "]"' <<<'{}')
submitted=$(date +%s)
res=$(api "$calls") || fail "EmailSubmission/set: $res"
read -r sent canceled < <(echo "$res" | py '" ".join(next(r[1]["created"][k]["id"] for r in j["methodResponses"] if r[0] == "EmailSubmission/set") for k in ("sent", "canceled"))') ||
	fail "the submissions were not created: $res"
export SENT=$sent CANCELED=$canceled
echo "submissions $sent (to be sent) and $canceled (to be canceled)"

# status <id> prints the undoStatus of a submission ("gone" if none).
status() {
	api "[[\"EmailSubmission/get\",{\"accountId\":\"$ACCT\",\"ids\":[\"$1\"]},\"g\"]]" |
		py '(j["methodResponses"][0][1]["list"] or [{"undoStatus": "gone"}])[0]["undoStatus"]'
}

for id in "$sent" "$canceled"; do
	[ "$(status "$id")" = pending ] || fail "submission $id is not pending"
done

pending=$(ctl submission list -json "$ACCOUNT" | py 'sum(1 for s in j["items"] if s["id"] in (e["SENT"], e["CANCELED"]) and s["state"] == "pending")')
[ "$pending" = 2 ] || fail "shardpostctl submission list shows $pending of the two as pending"
echo "both pending (JMAP and shardpostctl)"

echo; echo "== cancel $canceled =="
res=$(api "[[\"EmailSubmission/set\",{\"accountId\":\"$ACCT\",\"update\":{\"$canceled\":{\"undoStatus\":\"canceled\"}}},\"c\"]]")
echo "$res" | py 'j["methodResponses"][0][1]["updated"]' | grep -q "$canceled" || fail "the cancel was refused: $res"
[ "$(status "$canceled")" = canceled ] || fail "submission $canceled is not canceled"
echo "canceled"

# arrived <key> prints the ids of INBOX messages with the token's subject.
arrived() {
	api "[[\"Email/query\",{\"accountId\":\"$ACCT\",\"filter\":{\"inMailbox\":\"$INBOX\",\"subject\":\"$TOKEN-$1\"}},\"q\"]]" |
		py '" ".join(j["methodResponses"][0][1]["ids"])'
}

echo; echo "== waiting for the release (${HOLD}s) and the arrival (up to ${ARRIVAL}s) =="
deadline=$(($(date +%s) + HOLD + ARRIVAL))
got=""

while [ "$(date +%s)" -lt "$deadline" ]; do
	if [ "$(status "$sent")" = final ]; then
		got=$(arrived sent)
		[ -n "$got" ] && break
	fi

	sleep 10
done

[ "$(status "$sent")" = final ] || fail "submission $sent is not final after $((HOLD + ARRIVAL))s"
[ -n "$got" ] || fail "the message of $sent did not arrive in the INBOX"
api "[[\"EmailSubmission/get\",{\"accountId\":\"$ACCT\",\"ids\":[\"$sent\"]},\"g\"]]" |
	py 'json.dumps(j["methodResponses"][0][1]["list"][0]["deliveryStatus"])'
echo "sent and arrived: $got"

# The canceled one would have been released with the first; give it 30 s
# past that before taking its absence for good.
while [ "$(date +%s)" -lt $((submitted + HOLD + 30)) ]; do
	sleep 5
done

[ -z "$(arrived canceled)" ] || fail "the canceled submission arrived"
echo "the canceled one did not arrive"

echo; echo "== clean-up =="
export GOT=$got
api "$(py '"[" + json.dumps(["Email/set", {"accountId": e["ACCT"], "destroy": e["GOT"].split()}, "d"]) + "," +
	json.dumps(["EmailSubmission/set", {"accountId": e["ACCT"], "destroy": [e["SENT"], e["CANCELED"]]}, "x"]) + "]"' <<<'{}')" >/dev/null ||
	echo "note: clean-up failed; remove the messages with subject $TOKEN by hand"
echo "removed the test message and the submission records"

echo; echo "== delayed submission metrics =="
for pod in $(pods); do
	echo "$pod:"
	$K exec "$pod" -c server -- sh -c 'wget -qO- http://127.0.0.1:8080/metrics 2>/dev/null || curl -s http://127.0.0.1:8080/metrics' |
		grep -E '^imapceph_jmap_(delayed_submissions_total|submission_queue_entries)' || echo "  (none yet)"
done

echo; echo "OK: R5 smoke test passed"
