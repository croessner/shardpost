#!/usr/bin/env bash
# Stemming, substring and parallel-delivery test for the RADOS full-text index.
set -euo pipefail
CONTEXT=${CONTEXT:-croessner}
NS=${NS:-shardpost}
kx() { kubectl --context "$CONTEXT" -n "$NS" exec deploy/mailtest -- "$@"; }
imap() { kx curl -sS --user test:test "$@"; }
deliver() { kx swaks --protocol LMTP --server shardpost:24 --from "$1" --to test@example.org --header "Subject: $2" --body "$3" --silent 2 >/dev/null; }

echo "== deliver =="
deliver alice@example.org "Invoices for September"   "Please find the invoices attached."
deliver bob@example.org   "Rechnungen und Lieferung" "Die Rechnung wurde gestern bezahlt."
deliver carol@example.org "Lunch"                    "Pizza or sushi?"

echo "== stemming and substring =="
for q in 'SUBJECT invoice' 'SUBJECT invoicing' 'BODY invoiced' 'SUBJECT rechnung' 'BODY rechnungen' 'SUBJECT voic' 'FROM lic' 'BODY zah' 'TEXT sush' 'SUBJECT liefer BODY bezahlt' 'BODY xyz'; do
  printf '%-28s -> ' "$q"; imap --url 'imap://imap/INBOX' -X "SEARCH $q" | tr -d '\r'
done

echo "== 40 parallel deliveries from two clients (CAS under load) =="
kx sh -c 'for i in $(seq 1 40); do swaks --protocol LMTP --server shardpost:24 --from load$i@example.org --to test@example.org --header "Subject: loadtest $i" --body "parallel message number $i" --silent 2 >/dev/null & done; wait; echo done'
imap --url 'imap://imap/' -X 'STATUS INBOX (MESSAGES UIDNEXT)' | tr -d '\r'
printf 'SEARCH SUBJECT loadtest       -> '; imap --url 'imap://imap/INBOX' -X 'SEARCH SUBJECT loadtest' | tr -d '\r' | wc -w
printf 'SEARCH FROM load7             -> '; imap --url 'imap://imap/INBOX' -X 'SEARCH FROM load7' | tr -d '\r'

echo "== expunge all loadtest mails, dictionary must shrink =="
imap --url 'imap://imap/INBOX' -X 'UID STORE 4:* +FLAGS.SILENT (\Deleted)' >/dev/null
imap --url 'imap://imap/INBOX' -X 'EXPUNGE' >/dev/null
imap --url 'imap://imap/' -X 'STATUS INBOX (MESSAGES)' | tr -d '\r'
printf 'SEARCH SUBJECT loadtest       -> '; imap --url 'imap://imap/INBOX' -X 'SEARCH SUBJECT loadtest' | tr -d '\r'
