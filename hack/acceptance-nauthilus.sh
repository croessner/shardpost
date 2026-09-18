#!/usr/bin/env bash
# Acceptance test of the test-profile with Nauthilus authentication, run from the
# tools pod against the test-profile service directly (the director path is tested
# from the director repo). Passwords come from the SOPS file of the
# rns-server repo and are handed to curl via a config on stdin, never as
# an argument or in the pod's process list.
set -euo pipefail
CTX=${CTX:-croessner}
SOPS_FILE=${SOPS_FILE:-$HOME/src/rns-server/secrets/mail-test-accounts/shardpost.sops.yaml}
export SOPS_AGE_KEY_FILE=${SOPS_AGE_KEY_FILE:-"$HOME/Library/Application Support/sops/age/keys.txt"}
K="kubectl --context $CTX -n shardpost"

pw() { sops -d --extract "[\"accounts\"][$1][\"password\"]" "$SOPS_FILE"; }
imap() { # $1 index, $2 login, rest: curl args; password via config on stdin.
  # Server lines are taken from curl's verbose stream (custom commands do
  # not always echo untagged responses on stdout); authentication lines
  # are never printed.
  local i=$1 user=$2; shift 2
  { printf 'user = "%s:' "$user"; pw "$i" | tr -d '\n'; printf '"\n'; } \
    | $K exec -i deploy/mailtest -- curl -sS -v -K - --ssl-reqd --insecure "$@" 2>&1 \
    | grep -a "^< " | grep -av "AUTHENTICATE\|LOGIN\|CAPABILITY\|^< \* OK \[" | sed 's/^< //'
}

for i in 0 1; do
  login=$(sops -d --extract "[\"accounts\"][$i][\"login\"]" "$SOPS_FILE")
  echo "== $login: STARTTLS login + STATUS INBOX via Nauthilus"
  imap $i "$login" "imap://shardpost:143/" -X "STATUS INBOX (MESSAGES UIDNEXT)"
  echo "== $login: LMTP delivery (recipient lookup via Nauthilus)"
  $K exec deploy/mailtest -- swaks --protocol LMTP --server shardpost:24 --to "$login" \
      --from acceptance@exampleserver.de --header "Subject: test-profile acceptance $(date -u +%FT%TZ)" \
      --body "acceptance" --silent 2 | tail -2
  echo "== $login: message visible"
  imap $i "$login" "imap://shardpost:143/INBOX" -X "SEARCH HEADER Subject \"test-profile acceptance\""
done
echo "== negative: wrong password must fail"
if { printf 'user = "rbdpoc1@exampleserver.de:definitely-wrong"\n'; } \
   | $K exec -i deploy/mailtest -- curl -sS -K - --ssl-reqd --insecure "imap://shardpost:143/" -X "NOOP" 2>/dev/null; then
  echo "FAIL: wrong password accepted"; exit 1
else
  echo "ok: rejected"
fi
echo "== unknown LMTP recipient must be rejected"
$K exec deploy/mailtest -- swaks --protocol LMTP --server shardpost:24 --to nobody-here@exampleserver.de \
    --from acceptance@exampleserver.de --silent 2 | tail -2 || true

# --- ACL / shared folders, METADATA, CATENATE between the two accounts ----
u1=$(sops -d --extract '["accounts"][0]["login"]' "$SOPS_FILE")
u2=$(sops -d --extract '["accounts"][1]["login"]' "$SOPS_FILE")
box="Shared-$(date -u +%s)"
echo "== $u1: create $box, share it with $u2 (lrs), annotate it"
imap 0 "$u1" "imap://shardpost:143/" -X "CREATE $box"
imap 0 "$u1" "imap://shardpost:143/" -X "SETACL $box $u2 lrs"
imap 0 "$u1" "imap://shardpost:143/" -X "GETACL $box"
imap 0 "$u1" "imap://shardpost:143/" -X "SETMETADATA $box (/shared/comment \"team box\")"
echo "== $u1: APPEND a message, then CATENATE header and text sections of it (UID 1) into UID 2"
$K exec -i deploy/mailtest -- sh -c "printf 'From: a@example.org\r\nSubject: shared one\r\n\r\nhello\r\n' > /tmp/m.eml"
imap 0 "$u1" "imap://shardpost:143/$box" -T /tmp/m.eml
imap 0 "$u1" "imap://shardpost:143/" -X "APPEND $box CATENATE (URL \"imap:///$box/;UID=1/;SECTION=HEADER\" URL \"imap:///$box/;UID=1/;SECTION=TEXT\")"
echo "== $u2: sees it under Other Users, reads it, may not flag it"
imap 1 "$u2" "imap://shardpost:143/" -X "LIST \"\" \"Other Users/*\""
imap 1 "$u2" "imap://shardpost:143/" -X "MYRIGHTS \"Other Users/$u1/$box\""
imap 1 "$u2" "imap://shardpost:143/" -X "GETMETADATA \"Other Users/$u1/$box\" /shared/comment"
imap 1 "$u2" "imap://shardpost:143/" -X "STATUS \"Other Users/$u1/$box\" (MESSAGES)"
imap 1 "$u2" "imap://shardpost:143/Other%20Users%2F$u1%2F$box" -X "FETCH 2 (BODY.PEEK[HEADER.FIELDS (SUBJECT)] BODY.PEEK[TEXT])"
out=$(imap 1 "$u2" "imap://shardpost:143/Other%20Users%2F$u1%2F$box" -X 'STORE 1 +FLAGS (\Flagged)' || true)
if grep -q "NOPERM" <<<"$out"; then
  echo "ok: STORE without w right rejected"
else
  echo "FAIL: flag change without w right accepted"; exit 1
fi
echo "== $u1: revoke and delete"
imap 0 "$u1" "imap://shardpost:143/" -X "DELETEACL $box $u2"
out=$(imap 1 "$u2" "imap://shardpost:143/" -X "LIST \"\" \"Other Users/*\"" || true)
if grep -q "$box" <<<"$out"; then echo "FAIL: still visible"; exit 1; else echo "ok: hidden after revoke"; fi
imap 0 "$u1" "imap://shardpost:143/" -X "DELETE $box"
