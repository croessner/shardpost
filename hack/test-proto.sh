#!/usr/bin/env bash
# Tests of the own protocol layer: CONDSTORE, QRESYNC, QUOTA, SORT, THREAD,
# OBJECTID, PREVIEW, SAVEDATE, ID, MULTIAPPEND, AUTHENTICATE PLAIN.
set -euo pipefail
CONTEXT=${CONTEXT:-croessner}
NS=${NS:-shardpost}
kx() { kubectl --context "$CONTEXT" -n "$NS" exec -i deploy/mailtest -- "$@"; }
imapsession() { kx bash -c "$(cat "$(dirname "$0")/lib/imapsession.sh")"; }
deliver() { kx swaks --protocol LMTP --server shardpost:24 --from "$1" --to test@example.org --header "Subject: $2" --header "$3" --body "$4" --silent 2 >/dev/null; }

echo "== deliver a thread and an unrelated mail =="
deliver alice@example.org "Planning"     "Message-Id: <plan-1@example.org>" "Kickoff next week."
deliver bob@example.org   "Re: Planning" "References: <plan-1@example.org>" "Works for me."
deliver carol@example.org "Lunch"        "Message-Id: <lunch-1@example.org>" "Pizza?"
deliver dave@example.org  "Re: Planning" "References: <plan-1@example.org> <20260914@nowhere>" "Me too."

PLAIN=$(printf '\0test\0test' | base64)
imapsession <<EOF2
AUTHENTICATE PLAIN $PLAIN
ID ("name" "curl-test")
ENABLE QRESYNC
SELECT INBOX
FETCH 1:4 (UID MODSEQ EMAILID THREADID PREVIEW SAVEDATE ENVELOPE)
SORT (REVERSE DATE) UTF-8 ALL
SORT (FROM) UTF-8 SUBJECT planning
THREAD REFERENCES UTF-8 ALL
THREAD ORDEREDSUBJECT UTF-8 ALL
STORE 2 +FLAGS (\Flagged)
UID STORE 1 (UNCHANGEDSINCE 1) +FLAGS (\Answered)
SEARCH RETURN (ALL COUNT) MODSEQ 1
UID MOVE 3 Trash
STATUS INBOX (MESSAGES HIGHESTMODSEQ MAILBOXID)
GETQUOTAROOT INBOX
SETQUOTA "" (STORAGE 2048 MESSAGE 500)
GETQUOTA ""
CLOSE
LOGOUT
EOF2

echo; echo "== QRESYNC resync after changes made by another session =="
MS=$(imapsession <<'EOF2' | sed -n 's/^S: \* OK \[HIGHESTMODSEQ \([0-9]*\)\].*/\1/p'
LOGIN test test
SELECT INBOX
LOGOUT
EOF2
)
UV=$(imapsession <<'EOF2' | sed -n 's/^S: \* OK \[UIDVALIDITY \([0-9]*\)\].*/\1/p'
LOGIN test test
SELECT INBOX
LOGOUT
EOF2
)
echo "client knows UIDVALIDITY=$UV HIGHESTMODSEQ=$MS"
imapsession <<'EOF2' >/dev/null
LOGIN test test
SELECT INBOX
STORE 1 +FLAGS (\Seen)
UID STORE 4 +FLAGS (\Deleted)
UID EXPUNGE 4
LOGOUT
EOF2
imapsession <<EOF2
LOGIN test test
ENABLE QRESYNC
SELECT INBOX (QRESYNC ($UV $MS))
UID FETCH 1:* (FLAGS) (CHANGEDSINCE $MS VANISHED)
LOGOUT
EOF2

echo; echo "== MULTIAPPEND with two literals (LITERAL+) and BINARY.SIZE =="
kx bash -c "$(cat "$(dirname "$0")/lib/multiappend.sh")" </dev/null | grep -vE 'CAPABILITY|OK \[(UNSEEN|UIDNEXT|PERMANENTFLAGS|UIDVALIDITY|HIGHESTMODSEQ|MAILBOXID)|FLAGS \(|RECENT'
