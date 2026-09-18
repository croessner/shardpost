#!/usr/bin/env bash
# Extended test: chunked large message, FTS search, COPY/MOVE/DELETE/EXPUNGE.
set -euo pipefail
CONTEXT=${CONTEXT:-croessner}
NS=${NS:-shardpost}
kx() { kubectl --context "$CONTEXT" -n "$NS" exec deploy/mailtest -- "$@"; }
imap() { kx curl -sS --user test:test "$@"; }
deliver() { kx swaks --protocol LMTP --server shardpost:24 --from "$1" --to test@example.org --header "Subject: $2" --body "$3" --silent 2 >/dev/null; }

echo "== deliver three small mails =="
deliver alice@example.org "Invoice 4711"       "Please find the invoice attached. Regards, Alice"
deliver bob@example.org   "Lunch on Friday"    "Pizza or sushi? Bob"
deliver carol@example.org "Re: Invoice 4711"   "Paid yesterday, thanks. Carol"

echo "== deliver one 10 MiB mail (chunked into 4 MiB objects) =="
kx sh -c 'head -c 7500000 /dev/urandom | base64 -w 76 > /tmp/big.txt; ls -l /tmp/big.txt | awk "{print \$5}"'
kx sh -c 'swaks --protocol LMTP --server shardpost:24 --from big@example.org --to test@example.org --header "Subject: big attachment" --body - --silent 2 < /tmp/big.txt >/dev/null' 

echo "== SEARCH via RADOS full-text index =="
for q in 'SUBJECT invoice' 'FROM bob' 'BODY sushi' 'TEXT carol' 'SUBJECT invoice FROM carol' 'BODY nothingmatches'; do
  printf '%-28s -> ' "$q"; imap --url 'imap://imap/INBOX' -X "SEARCH $q" | tr -d '\r'
done

echo "== fetch the big one back and compare size =="
imap --url 'imap://imap/INBOX' -X 'FETCH 4 (RFC822.SIZE BODY.PEEK[HEADER.FIELDS (SUBJECT)])' | tr -d '\r' | head -3
imap --url 'imap://imap/INBOX;UID=4' -o /tmp/fetched.eml
kx sh -c 'wc -c < /tmp/fetched.eml; grep -c . /tmp/big.txt; tail -c 200 /tmp/fetched.eml | tail -2 | head -1 | cut -c1-40; tail -1 /tmp/big.txt | cut -c1-40'

echo "== COPY 1 -> Archive, MOVE 2 -> Archive, delete+expunge 3 =="
imap --url 'imap://imap/' -X 'CREATE Archive' >/dev/null
imap --url 'imap://imap/INBOX' -X 'UID COPY 1 Archive' | tr -d '\r'
imap --url 'imap://imap/INBOX' -X 'UID MOVE 2 Archive' | tr -d '\r'
imap --url 'imap://imap/INBOX' -X 'UID STORE 3 +FLAGS (\Deleted)' | tr -d '\r'
imap --url 'imap://imap/INBOX' -X 'EXPUNGE' | tr -d '\r'
imap --url 'imap://imap/' -X 'STATUS INBOX (MESSAGES UIDNEXT)' | tr -d '\r'
imap --url 'imap://imap/' -X 'STATUS Archive (MESSAGES UIDNEXT)' | tr -d '\r'
echo "search works in Archive after COPY/MOVE (index is per GUID), expunged carol is gone:"
imap --url 'imap://imap/Archive' -X 'SEARCH FROM bob' | tr -d '\r'
imap --url 'imap://imap/Archive' -X 'SEARCH SUBJECT invoice' | tr -d '\r'
imap --url 'imap://imap/INBOX' -X 'SEARCH FROM carol' | tr -d '\r'
