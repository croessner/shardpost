#!/usr/bin/env bash
# Live check of Sieve redirect and vacation through the relay: rbdpoc2
# gets a script that forwards a copy to rbdpoc1 and answers with a
# vacation message; a mail from rbdpoc1 to rbdpoc2 must then produce two
# new messages in rbdpoc1's INBOX (the copy and the auto-reply), both
# arriving through the relay and the director.
set -euo pipefail
CTX=${CTX:-croessner}
SOPS_FILE=${SOPS_FILE:-$HOME/src/rns-server/secrets/mail-test-accounts/shardpost.sops.yaml}
export SOPS_AGE_KEY_FILE=${SOPS_AGE_KEY_FILE:-"$HOME/Library/Application Support/sops/age/keys.txt"}
K="kubectl --context $CTX -n shardpost"
u1=$(sops -d --extract '["accounts"][0]["login"]' "$SOPS_FILE")
u2=$(sops -d --extract '["accounts"][1]["login"]' "$SOPS_FILE")
pw() { sops -d --extract "[\"accounts\"][$1][\"password\"]" "$SOPS_FILE" | tr -d '\n'; }
b64() { printf '\0%s\0%s' "$1" "$2" | base64 | tr -d '\n'; }
marker="poc-$(date -u +%s)"
script="require [\"vacation\", \"copy\"];
redirect :copy \"$u1\";
vacation :days 1 :subject \"Out of office $marker\" \"I am away ($marker).\";"
# ManageSieve over STARTTLS with openssl; the password never leaves the pipe.
$K exec -i deploy/mailtest -- sh -c "cat > /tmp/sieve.txt" <<<"$script"
len=$(printf '%s' "$script" | wc -c | tr -d ' ')
{ printf 'AUTHENTICATE "PLAIN" "%s"\r\n' "$(b64 "$u2" "$(pw 1)")"; sleep 1; printf 'PUTSCRIPT "ooo" {%d+}\r\n' "$len"; printf '%s' "$script"; printf '\r\nSETACTIVE "ooo"\r\nLOGOUT\r\n'; sleep 1; } \
  | $K exec -i deploy/mailtest -- openssl s_client -quiet -starttls sieve -connect shardpost:4190 2>/dev/null | grep -a "^OK\|^NO\|^BYE" | grep -av "AUTHENTICATE" | tail -4 || true
echo "== deliver from $u1 to $u2 via LMTP ($marker)"
$K exec deploy/mailtest -- swaks --protocol LMTP --server shardpost:24 --to "$u2" --from "$u1" --header "Subject: trigger $marker" --body "hello" --silent 2 | tail -1 || true
echo "== server log"
sleep 3
$K logs deploy/shardpost-server --since=2m --all-containers 2>/dev/null | grep -a "redirected\|vacation" | grep -o '"msg":"[^"]*"\|"to":[^,]*\|"err":"[^"]*"' | paste - - 2>/dev/null | tail -4 || true
echo "== $u1 INBOX: wait for copy and auto-reply via the director"
for i in $(seq 1 12); do
  out=$( { printf 'user = "%s:' "$u1"; pw 0; printf '"\n'; } | $K exec -i deploy/mailtest -- curl -sS -K - --ssl-reqd --insecure "imap://shardpost:143/INBOX" -X "SEARCH HEADER Subject \"$marker\"" 2>/dev/null | tr -d '\r')
  n=$(echo "$out" | sed 's/\* SEARCH//' | wc -w | tr -d ' ')
  [ "$n" -ge 2 ] && break
  sleep 10
done
echo "$out (messages with marker: $n; expected 2 = redirect copy + vacation reply)"
{ printf 'AUTHENTICATE "PLAIN" "%s"\r\n' "$(b64 "$u2" "$(pw 1)")"; sleep 1; printf 'SETACTIVE ""\r\nDELETESCRIPT "ooo"\r\nLOGOUT\r\n'; sleep 1; } \
  | $K exec -i deploy/mailtest -- openssl s_client -quiet -starttls sieve -connect shardpost:4190 2>/dev/null | grep -a "^OK\|^NO" | tail -2 || true
