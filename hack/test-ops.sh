#!/usr/bin/env bash
# Tests for the operations features: sharded index, incremental poll,
# POP3, COMPRESS, charset SEARCH, connection limits, scrubber, metrics.
set -euo pipefail
CONTEXT=${CONTEXT:-croessner}
NS=${NS:-shardpost}
kx() { kubectl --context "$CONTEXT" -n "$NS" exec -i deploy/mailtest -- "$@"; }
KR="kubectl --context $CONTEXT -n rook-ceph exec deploy/rook-ceph-tools --"
imapsession() { kx bash -c "$(cat "$(dirname "$0")/lib/imapsession.sh")"; }
pop3session() { kx bash -c "$(cat "$(dirname "$0")/lib/pop3session.sh")"; }
deliver() { kx swaks --protocol LMTP --server shardpost:24 --from "$1" --to test@example.org --header "Subject: $2" --body "$3" --silent 2 >/dev/null; }

echo "== deliver 45 mails (shard size in this overlay is 20 -> 3 shards) =="
kx sh -c 'for i in $(seq 1 45); do swaks --protocol LMTP --server shardpost:24 --from s$i@example.org --to test@example.org --header "Subject: shard test $i" --body "message $i" --silent 2 >/dev/null & done; wait'
$KR sh -c 'rados -p shardpost-index -N test getomapval mailboxes m:INBOX /tmp/rec >/dev/null 2>&1; id=$(sed "s/.*\"id\":\"\([0-9a-f]*\)\".*/\1/" /tmp/rec); echo "index objects of INBOX:"; rados -p shardpost-index -N test ls | grep "^mbox/$id" | sort | while read o; do printf "  %-45s entries=%s\n" "$o" "$(rados -p shardpost-index -N test listomapkeys $o | grep -c "^m:")"; done; echo -n "change log records: "; rados -p shardpost-index -N test listomapkeys mbox/$id | grep -c "^c:"'

echo; echo "== incremental poll: session A sees B's changes via change log =="
( sleep 3; deliver late@example.org "late arrival" "delivered while session A is idle" ) &
{ echo "LOGIN test test"; echo "SELECT INBOX"; sleep 6; echo "NOOP"; echo "LOGOUT"; } | imapsession | grep -E 'EXISTS|EXPUNGE|FETCH \(UID|C: '
wait
echo "(server-side: imapceph_commands_total{command=\"poll\"} below distinguishes incremental from reload)"

echo; echo "== POP3 =="
pop3session <<'EOF2'
CAPA
USER test
PASS test
STAT
UIDL 1
TOP 1 0
RETR 1
DELE 2
QUIT
EOF2
echo "after POP3 QUIT (message 2 expunged, message 1 seen):"
imapsession <<'EOF2' | grep -E 'STATUS|SEARCH'
LOGIN test test
STATUS INBOX (MESSAGES UNSEEN)
SELECT INBOX
UID SEARCH SEEN
LOGOUT
EOF2

echo; echo "== COMPRESS=DEFLATE =="
kx bash -c "$(cat "$(dirname "$0")/lib/compress.sh")" </dev/null

echo; echo "== SEARCH with CHARSET ISO-8859-1 (Rechnungen, umlaut in query) =="
deliver ute@example.org "Bestellung Müller" "Die Rechnung für Müller ist bezahlt."
kx bash -c 'exec 3<>/dev/tcp/imap/143; read -r -u3 g; printf "a1 LOGIN test test\r\na2 SELECT INBOX\r\n" >&3; printf "a3 SEARCH CHARSET ISO-8859-1 SUBJECT {6}\r\nM\xfcller\r\na4 SEARCH CHARSET KOI8-R SUBJECT x\r\na5 LOGOUT\r\n" >&3; while IFS= read -r -u3 -t 5 l; do l=${l%$'"'"'\r'"'"'}; case "$l" in "* SEARCH"*|a3*|a4*) echo "S: $l";; esac; [[ $l == a5* ]] && break; done'

echo; echo "== scrubber: plant an orphan blob and a wrong refcount, then run a scrub Job =="
$KR sh -c 'echo "orphan" | rados -p shardpost-data -N test put msg/deadbeefdeadbeefdeadbeefdeadbeef/0 - ; rados -p shardpost-data -N test setxattr msg/deadbeefdeadbeefdeadbeefdeadbeef/0 refs 1; first=$(rados -p shardpost-data -N test ls | grep "/0$" | grep -v deadbeef | head -1); rados -p shardpost-data -N test setxattr $first refs 7; echo "planted orphan and refs=7 on $first"'
K="kubectl --context $CONTEXT -n $NS"
$K delete job shardpost-scrub-manual --ignore-not-found >/dev/null
$K create job shardpost-scrub-manual --from=cronjob/shardpost-scrub --dry-run=client -o yaml | sed 's/value: "3600"/value: "0"/' | $K apply -f - >/dev/null
$K wait --for=condition=complete job/shardpost-scrub-manual --timeout=180s >/dev/null && echo "job complete"
$K logs job/shardpost-scrub-manual | grep -E 'orphan|refcount|scrub done|quota' | sed 's/{"time[^,]*,//' | cut -c1-220
$KR sh -c 'rados -p shardpost-data -N test ls | grep -c deadbeef || true'

echo; echo "== metrics =="
POD=$($K get pods -l app.kubernetes.io/name=shardpost-server -o jsonpath='{.items[0].metadata.name}')
$K exec $POD -- sh -c 'exec 3<>/dev/tcp/127.0.0.1/8080; printf "GET /metrics HTTP/1.0\r\n\r\n" >&3; cat <&3' | grep -E '^imapceph_(commands_total\{command="poll"|rados_cas_retries_total|connections|lmtp_deliveries_total|rados_op_seconds_count\{op="header_cas"|command_seconds_count\{command="(FETCH|SEARCH)")' | head -12
