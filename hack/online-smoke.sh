#!/usr/bin/env bash
# Cluster smoke test of the online runs of the full-text index (stage
# B14i), run after a rollout of 0.51.0 with the mark fts/online set:
#
#   1. keep an IMAP session of the throwaway account logged in (SELECT
#      INBOX, then a NOOP every few seconds) through a port-forward,
#   2. plan and apply an online fts_rebuild of the account,
#   3. while the job runs: deliver to the account at a low rate over
#      LMTP (shardpost-load in a server pod, a few messages per exec) and
#      try a POP3 login and a JMAP session request again and again from
#      the mailtest pod,
#   4. afterwards check that the job succeeded online, that no delivery
#      failed (no 451), no login was refused, the IMAP session got no BYE,
#      and that the server logs show no "account locked" line for the
#      account during the run.
#
#   hack/online-smoke.sh [<throwaway-account>]
#
# Only the throwaway account (default rbdpoc2@exampleserver.de) is used.
# The credentials come from the secret imaptest-creds (keys user and
# pass); the password is read inside this script (for curl as a config on
# stdin, for the IMAP session by the Python process from kubectl) and
# never printed or passed as an argument. The secret's user must be the
# throwaway account. Needs kubectl, python3 and curl locally.
set -euo pipefail

CONTEXT=${CONTEXT:-croessner}
NS=${NS:-shardpost}
DEPLOY=${DEPLOY:-deploy/shardpost-server}
TOOLS=${TOOLS:-deploy/mailtest}
SELECTOR=${SELECTOR:-app.kubernetes.io/name=shardpost-server}
SERVICE=${SERVICE:-svc/shardpost}
POP3_URL=${POP3_URL:-pop3://shardpost:110/}
JMAP_URL=${JMAP_URL:-https://shardpost-jmap:9443}
# The LMTP listener of the server pod itself: the NetworkPolicy of the
# service does not admit the server pods as clients of port 24.
LMTP=${LMTP:-127.0.0.1:2424}
LOCAL_IMAP=${LOCAL_IMAP:-14300}
ACCOUNT=${1:-rbdpoc2@exampleserver.de}
# RATE deliveries per second, BATCH messages per shardpost-load exec,
# PAUSE seconds between the POP3/JMAP login attempts.
RATE=${RATE:-1}
BATCH=${BATCH:-5}
PAUSE=${PAUSE:-5}

K="kubectl --context $CONTEXT -n $NS"
TMP=$(mktemp -d)
PIDS=()

cleanup() {
	touch "$TMP/stop"
	for p in "${PIDS[@]:-}"; do [ -n "$p" ] && kill "$p" 2>/dev/null || true; done
	wait 2>/dev/null || true
	rm -rf "$TMP"
}
trap cleanup EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }
ctl() { $K exec -i "$DEPLOY" -c server -- shardpostctl "$@"; }
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

[ "$(ctl fts online status -json | json 'j.get("enabled", False)')" = True ] || fail "the mark fts/online is not set (shardpostctl fts online enable)"
start=$(date -u +%Y-%m-%dT%H:%M:%SZ)

echo "== IMAP session of $ACCOUNT (port-forward to $SERVICE) =="
$K port-forward "$SERVICE" "$LOCAL_IMAP:143" >"$TMP/pf.log" 2>&1 &
PIDS+=($!)
sleep 3

# The session reads the password itself; it reports "ok <noops>" or the
# error (a BYE ends imaplib with an abort) to $TMP/imap.result.
CONTEXT=$CONTEXT NS=$NS PORT=$LOCAL_IMAP STOP="$TMP/stop" RESULT="$TMP/imap.result" python3 - <<'PY' &
import base64, imaplib, os, ssl, subprocess, time

def secret(key):
    out = subprocess.run(["kubectl", "--context", os.environ["CONTEXT"], "-n", os.environ["NS"], "get", "secret",
                          "imaptest-creds", "-o", "jsonpath={.data.%s}" % key], check=True, capture_output=True).stdout
    return base64.b64decode(out).decode()

result = os.environ["RESULT"]
noops = 0
try:
    ctx = ssl.create_default_context()
    ctx.check_hostname, ctx.verify_mode = False, ssl.CERT_NONE
    c = imaplib.IMAP4("127.0.0.1", int(os.environ["PORT"]))
    c.starttls(ssl_context=ctx)
    c.login(secret("user"), secret("pass"))
    c.select("INBOX")
    while not os.path.exists(os.environ["STOP"]):
        typ, data = c.noop()
        if typ != "OK":
            raise RuntimeError("NOOP answered %s %r" % (typ, data))
        noops += 1
        time.sleep(3)
    c.logout()
    open(result, "w").write("ok %d\n" % noops)
except Exception as e:  # BYE, NO, a broken connection
    open(result, "w").write("error after %d NOOPs: %s: %s\n" % (noops, type(e).__name__, e))
PY
PIDS+=($!)
sleep 5
[ ! -s "$TMP/imap.result" ] || fail "IMAP session: $(cat "$TMP/imap.result")"

echo; echo "== online fts_rebuild of $ACCOUNT =="
plan=$(ctl plan --json -online fts_rebuild "$ACCOUNT") || fail "plan fts_rebuild -online (is the mark fts/online set?)"
id=$(echo "$plan" | json 'j["id"]')
job=$(ctl apply --json "$id") || fail "apply $id"
jobid=$(echo "$job" | json 'j["id"]')
echo "job $jobid"

loads=0 lfail=0 pops=0 popfail=0 jmaps=0 jmapfail=0 first=$(date +%s)000
state=running

while :; do
	state=$(ctl job show --json "$jobid" | json 'j["state"]')
	case "$state" in
	queued | running | cancel_requested) ;;
	*) break ;;
	esac

	out=$($K exec "$DEPLOY" -c server -- shardpost-load -lmtp "$LMTP" -corpus plain -rcpt "$ACCOUNT" -count "$BATCH" \
		-first "$first" -rate "$RATE" -parallel 1 2>&1 | grep -E '^shardpost-load: .* delivered') || true
	first=$((first + BATCH))
	loads=$((loads + BATCH))
	f=$(echo "$out" | sed -n 's/.* \([0-9][0-9]*\) failed.*/\1/p')
	[ -n "$f" ] || { echo "shardpost-load: $out"; f=$BATCH; }
	lfail=$((lfail + f))

	pops=$((pops + 1))
	curlcfg | $K exec -i "$TOOLS" -- curl -sS -K - --ssl-reqd --insecure "$POP3_URL" -X NOOP >/dev/null 2>&1 || popfail=$((popfail + 1))

	jmaps=$((jmaps + 1))
	code=$({
		curlcfg
		printf 'url = "%s/.well-known/jmap"\nwrite-out = "%%{http_code}"\noutput = "/dev/null"\n' "$JMAP_URL"
	} | $K exec -i "$TOOLS" -- curl -sS -K - --insecure 2>/dev/null || true)
	[ "$code" = 200 ] || jmapfail=$((jmapfail + 1))

	sleep "$PAUSE"
done

end=$(date -u +%Y-%m-%dT%H:%M:%SZ)
touch "$TMP/stop"
sleep 5

echo; echo "== results ($start .. $end) =="
result=$(ctl job show --json "$jobid")
echo "job state $state: $(echo "$result" | json 'json.dumps({k: (j.get("result") or {}).get(k) for k in ("online","mode","deferred","rounds","caught_up","hold_ms","indexed","unreadable","orphan_ids")})')"
echo "deliveries $loads, failed $lfail; POP3 logins $pops, refused $popfail; JMAP sessions $jmaps, not 200 $jmapfail"
echo "IMAP session: $(cat "$TMP/imap.result" 2>/dev/null || echo 'no result')"

locked=$(for pod in $($K get pods -l "$SELECTOR" -o name); do
	$K logs "$pod" -c server --since-time="$start" 2>/dev/null || true
done | grep -F "$ACCOUNT" | grep -cE 'account locked|login refused|session closed' || true)
echo "log lines of $ACCOUNT with a lock refusal: $locked"

[ "$state" = succeeded ] || fail "the job ended $state"
[ "$(echo "$result" | json '(j.get("result") or {}).get("online")')" = True ] || fail "the job did not run online"
[ "$(echo "$result" | json '(j.get("result") or {}).get("deferred") or ""')" = "" ] || fail "the run ended without a change (deferred)"
[ "$lfail" -eq 0 ] || fail "$lfail deliveries failed during the run"
[ "$popfail" -eq 0 ] || fail "$popfail POP3 logins refused during the run"
[ "$jmapfail" -eq 0 ] || fail "$jmapfail JMAP session requests failed during the run"
grep -q '^ok ' "$TMP/imap.result" 2>/dev/null || fail "the IMAP session did not stay logged in"
[ "$locked" -eq 0 ] || fail "$locked log lines show the account locked during the run"

echo; echo "OK: online rebuild without interruption"
