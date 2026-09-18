#!/usr/bin/env bash
# End-to-end smoke test: deliver a mail via LMTP with swaks, read it back via
# IMAP with curl. Runs inside the mailtest pod of the imap namespace.
set -euo pipefail
CONTEXT=${CONTEXT:-croessner}
NS=${NS:-shardpost}
kx() { kubectl --context "$CONTEXT" -n "$NS" exec deploy/mailtest -- "$@"; }

subject="ceph-poc $(date +%s)"
echo "== LMTP delivery (swaks) =="
kx swaks --protocol LMTP --server shardpost:24 \
   --from sender@example.org --to test@example.org \
   --header "Subject: $subject" --body "Hello from RADOS."

echo; echo "== IMAP: list mailboxes =="
kx curl -sS --url 'imap://imap/' --user test:test

echo; echo "== IMAP: search all (UIDs) =="
kx curl -sS --url 'imap://imap/INBOX?ALL' --user test:test

echo; echo "== IMAP: fetch newest message =="
uid=$(kx curl -sS --url 'imap://imap/INBOX?ALL' --user test:test | awk '{print $NF}' | tr -d '\r')
kx curl -sS --url "imap://imap/INBOX;UID=${uid}" --user test:test

echo; echo "== IMAP: STATUS via custom request =="
kx curl -sS --url 'imap://imap/' --user test:test -X 'STATUS INBOX (MESSAGES UNSEEN UIDNEXT SIZE)'
