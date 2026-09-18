#!/usr/bin/env bash
# Functional test of the IMAP feature set: hierarchy, RENAME, subscriptions,
# special-use, EXAMINE, keywords, SEARCHRES, ENVELOPE cache, IDLE through
# RADOS watch/notify, TLS. Uses a scripted IMAP session over bash /dev/tcp
# inside the mailtest pod.
set -euo pipefail
CONTEXT=${CONTEXT:-croessner}
NS=${NS:-shardpost}
kx() { kubectl --context "$CONTEXT" -n "$NS" exec -i deploy/mailtest -- "$@"; }
deliver() { kx swaks --protocol LMTP --server shardpost:24 --from "$1" --to test@example.org --header "Subject: $2" --body "$3" --silent 2 >/dev/null; }

# imapsession: reads IMAP commands from stdin (one per line), tags them,
# waits for each tagged reply and prints the dialogue.
imapsession() { kx bash -c "$(cat "$(dirname "$0")/lib/imapsession.sh")"; }

echo "== deliver =="
deliver alice@example.org "Invoice 4711" "Please find the invoice attached."
deliver bob@example.org   "Lunch" "Pizza or sushi?"

echo; echo "== capabilities, LIST with special-use, hierarchy, RENAME, subscriptions =="
imapsession <<'EOF2' | grep -v '^S: \* CAPABILITY' 
CAPABILITY
LOGIN test test
LIST "" "*" RETURN (SPECIAL-USE)
CREATE Projects/2026/Q3
CREATE Projects/2026/Q4
LIST "" "Projects*"
RENAME Projects Work
LIST "" "Work*"
SUBSCRIBE Work/2026/Q3
LIST (SUBSCRIBED) "" "*"
LSUB "" "Work*"
UNSUBSCRIBE Work/2026/Q3
DELETE Work/2026/Q4
LIST "" "Work/2026*"
LOGOUT
EOF2

echo; echo "== EXAMINE is read-only, keywords, SEARCHRES, ENVELOPE from cache, SENTSINCE =="
imapsession <<'EOF2'
LOGIN test test
EXAMINE INBOX
STORE 1 +FLAGS (\Seen)
SELECT INBOX
STORE 1 +FLAGS ($Forwarded)
SELECT INBOX
SEARCH RETURN (SAVE) SUBJECT invoice
UID FETCH $ (UID FLAGS ENVELOPE)
SEARCH SENTSINCE 1-Jan-2020 FROM bob
STATUS Trash (MESSAGES UIDNEXT)
UID MOVE 2 Trash
SELECT Trash
LOGOUT
EOF2

echo; echo "== IDLE: pod-to-pod notify via RADOS watch =="
(
  kx bash -c "$(cat "$(dirname "$0")/lib/idle.sh")" </dev/null &
  sleep 3
  deliver carol@example.org "wake up" "notify test"
  wait
)

echo; echo "== TLS: implicit on 993 and STARTTLS on 143 =="
kx curl -sS -k --user test:test --url 'imaps://shardpost:993/INBOX?ALL' | tr -d '\r'
kx curl -sS -k --ssl-reqd --user test:test --url 'imap://shardpost:143/' -X 'STATUS INBOX (MESSAGES UNSEEN SIZE)' | tr -d '\r'

echo; echo "== RADOS: registry, header, vanished log =="
KR="kubectl --context $CONTEXT -n rook-ceph exec deploy/rook-ceph-tools --"
$KR sh -c 'rados -p shardpost-index -N test listomapkeys mailboxes | tr "\n" " "; echo; rados -p shardpost-index -N test getomapval mailboxes m:INBOX /tmp/rec >/dev/null; id=$(sed "s/.*\"id\":\"\([0-9a-f]*\)\".*/\1/" /tmp/rec); echo "INBOX id=$id"; rados -p shardpost-index -N test listomapkeys mbox/$id | tr "\n" " "; echo'
