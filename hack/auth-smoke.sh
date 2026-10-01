#!/usr/bin/env bash
# Cluster smoke test of the auth cache and the JMAP load limits, run
# after a rollout:
#
#   1. log in over IMAP several times with the throwaway account and
#      check that the auth cache answers (the hits of all server pods
#      grow),
#   2. flush the account's entries with shardpostctl auth flush and check
#      that the next login is a miss again (the flush reaches every pod
#      through the flush log; the script waits for the slowest path, the
#      poll, unless WAIT says otherwise),
#   3. fire N parallel JMAP API requests (HTTP Basic) at the in-cluster
#      JMAP service and check that none is answered 503 or 429,
#   4. print the auth cache and JMAP limit metrics of every pod.
#
#   hack/auth-smoke.sh [<throwaway-account>]
#
# The credentials come from the secret imaptest-creds (keys user and
# pass) in the namespace; the password is read inside this script, handed
# to curl as a config on stdin and never printed or passed as an
# argument. The secret's user must be the throwaway account (default
# rbdpoc2@exampleserver.de). Logins and requests run from the mailtest
# pod, straight to the services (not through the Director).
#
# The metrics are the sums over all server pods. Real traffic on the
# same pods adds to them, so the checks are "at least" checks: a busy
# cluster can hide a missing hit but not invent a miss that did not
# happen. N must stay within what one account may have running and
# queued (jmap.max_concurrent_requests and twice that waiting: 24 with
# the defaults), or the excess is answered 429 by design.
set -euo pipefail

CONTEXT=${CONTEXT:-croessner}
NS=${NS:-shardpost}
DEPLOY=${DEPLOY:-deploy/shardpost-server}
TOOLS=${TOOLS:-deploy/mailtest}
SELECTOR=${SELECTOR:-app.kubernetes.io/name=shardpost-server}
IMAP_URL=${IMAP_URL:-imap://shardpost:143/}
JMAP_URL=${JMAP_URL:-https://shardpost-jmap:9443}
ACCOUNT=${1:-rbdpoc2@exampleserver.de}
LOGINS=${LOGINS:-}
N=${N:-16}
# WAIT is how long the flush may take to reach the other pods: the
# notification is immediate, the poll that backs it up runs every 60 s.
WAIT=${WAIT:-65}

K="kubectl --context $CONTEXT -n $NS"

fail() { echo "FAIL: $*" >&2; exit 1; }
ctl() { $K exec -i "$DEPLOY" -c server -- shardpostctl "$@"; }

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

pods() { $K get pods -l "$SELECTOR" -o name; }

# metric <name> <labels> sums a series over all server pods.
metric() {
	local pod
	for pod in $(pods); do
		$K exec "$pod" -c server -- sh -c 'wget -qO- http://127.0.0.1:8080/metrics 2>/dev/null || curl -s http://127.0.0.1:8080/metrics'
	done | awk -v series="$1{$2}" '$1 == series { sum += $2 } END { printf "%d\n", sum }'
}

hits() { metric imapceph_auth_cache_total 'op="authenticate",result="hit"'; }
misses() { metric imapceph_auth_cache_total 'op="authenticate",result="miss"'; }

imap_login() {
	curlcfg | $K exec -i "$TOOLS" -- curl -sS -K - --ssl-reqd --insecure "$IMAP_URL" -X NOOP >/dev/null 2>&1 ||
		fail "IMAP login of $ACCOUNT failed"
}

npods=$(pods | wc -l | tr -d ' ')
[ "$npods" -gt 0 ] || fail "no server pods ($SELECTOR)"
# Enough logins that, however the service spreads them, some pod sees
# the same credentials twice.
LOGINS=${LOGINS:-$((3 * npods + 1))}

echo "== $LOGINS IMAP logins of $ACCOUNT over $npods pods =="
h0=$(hits)
m0=$(misses)
for _ in $(seq "$LOGINS"); do imap_login; done
h1=$(hits)
m1=$(misses)
echo "hits +$((h1 - h0)), misses +$((m1 - m0))"
[ $((h1 - h0)) -ge $((LOGINS - npods)) ] || fail "expected at least $((LOGINS - npods)) cache hits"

echo; echo "== shardpostctl auth flush $ACCOUNT =="
ctl auth flush "$ACCOUNT"
flushes=$(metric imapceph_auth_cache_flushes_total 'scope="account",source="local"')
echo "local account flushes (all pods): $flushes"
echo "waiting ${WAIT}s for the other pods (notify, poll fallback)"
sleep "$WAIT"

m2=$(misses)
h2=$(hits)
# One login per pod at most can be a miss; the first must be one.
imap_login
m3=$(misses)
h3=$(hits)
echo "after the flush: hits +$((h3 - h2)), misses +$((m3 - m2))"
[ $((m3 - m2)) -ge 1 ] || fail "the first login after the flush was not a miss"

echo; echo "== $N parallel JMAP requests =="
# One curl, N transfers at once; the options before the URLs apply to
# all of them.
codes=$({
	curlcfg
	printf 'request = "POST"\nheader = "Content-Type: application/json"\n'
	printf 'data = "{\\"using\\":[\\"urn:ietf:params:jmap:core\\"],\\"methodCalls\\":[[\\"Core/echo\\",{},\\"c1\\"]]}"\n'
	printf 'write-out = "%%{http_code}\\n"\n'
	for _ in $(seq "$N"); do printf 'url = "%s/jmap/api/"\noutput = "/dev/null"\n' "$JMAP_URL"; done
} | $K exec -i "$TOOLS" -- curl -sS -K - --insecure --parallel --parallel-max "$N" || true)
echo "$codes" | sort | uniq -c
ok=$(echo "$codes" | grep -c '^200$' || true)
bad=$(echo "$codes" | grep -cE '^(429|503)$' || true)
[ "$bad" -eq 0 ] || fail "$bad of $N JMAP requests answered 429 or 503"
[ "$ok" -eq "$N" ] || fail "only $ok of $N JMAP requests answered 200"

echo; echo "== auth cache and JMAP limit metrics =="
for pod in $(pods); do
	echo "-- $pod"
	$K exec "$pod" -c server -- sh -c 'wget -qO- http://127.0.0.1:8080/metrics 2>/dev/null || curl -s http://127.0.0.1:8080/metrics' |
		grep -E '^imapceph_(auth_cache_total|auth_cache_entries|auth_cache_flushes_total|jmap_limit_total)' || echo "(none yet)"
done

echo; echo "OK: auth cache smoke test passed"
