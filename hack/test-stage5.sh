#!/usr/bin/env bash
# Sieve/ManageSieve, XOAUTH2/OAUTHBEARER, master user, drain, key rotation.
set -euo pipefail
CONTEXT=${CONTEXT:-croessner}
NS=${NS:-shardpost}
K="kubectl --context $CONTEXT -n $NS"
kx() { $K exec -i deploy/mailtest -- "$@"; }
imapsession() { kx bash -c "$(cat "$(dirname "$0")/lib/imapsession.sh")"; }
sievesession() { kx bash -c "$(cat "$(dirname "$0")/lib/sievesession.sh")"; }
deliver() { kx swaks --protocol LMTP --server shardpost:24 --from "$1" --to test@example.org --header "Subject: $2" --body "$3" --silent 2 >/dev/null; }

echo "== ManageSieve: put, activate, list =="
sievesession <<'EOF2' | grep -vE '^S: "(IMPLEMENTATION|SIEVE|SASL|STARTTLS|MAXREDIRECTS|VERSION)'
AUTHENTICATE "PLAIN" "AHRlc3QAdGVzdA=="
PUTSCRIPT "finance"
require ["fileinto", "imap4flags"];
if header :contains "subject" "invoice" {
  addflag "\\Flagged";
  fileinto "Finance";
}
if header :contains "subject" "spam" { discard; }
---
CHECKSCRIPT "if header :contains { }"
SETACTIVE "finance"
LISTSCRIPTS
LOGOUT
EOF2

echo; echo "== LMTP delivery through the script =="
deliver acme@example.org "Invoice 2026-09" "please pay"
deliver junk@example.org "cheap spam" "buy now"
deliver bob@example.org  "Lunch" "pizza?"
imapsession <<'EOF2' | grep -E 'STATUS|FLAGS \(\\flagged|C: '
LOGIN test test
STATUS INBOX (MESSAGES)
STATUS Finance (MESSAGES)
SELECT Finance
FETCH 1 (FLAGS ENVELOPE)
LOGOUT
EOF2

echo; echo "== SASL: XOAUTH2, OAUTHBEARER (static backend: token == password), failure continuation =="
X_OK=$(printf 'user=test\001auth=Bearer test\001\001' | base64 -w0)
X_BAD=$(printf 'user=test\001auth=Bearer wrong\001\001' | base64 -w0)
OB_OK=$(printf 'n,a=test,\001auth=Bearer test\001\001' | base64 -w0)
kx bash -c "exec 3<>/dev/tcp/imap/143; read -r -u3 g; printf 'a1 AUTHENTICATE XOAUTH2 $X_OK\r\n' >&3; read -r -u3 l; echo \"S: \${l%\$'\r'}\" | cut -c1-60; printf 'a2 LOGOUT\r\n' >&3"
kx bash -c "exec 3<>/dev/tcp/imap/143; read -r -u3 g; printf 'a1 AUTHENTICATE OAUTHBEARER $OB_OK\r\n' >&3; read -r -u3 l; echo \"S: \${l%\$'\r'}\" | cut -c1-60; printf 'a2 LOGOUT\r\n' >&3"
kx bash -c "exec 3<>/dev/tcp/imap/143; read -r -u3 g; printf 'a1 AUTHENTICATE XOAUTH2 $X_BAD\r\n' >&3; read -r -u3 l; echo \"S: \${l%\$'\r'}\"; printf '\001\r\n' >&3; read -r -u3 l; echo \"S: \${l%\$'\r'}\"; printf 'a2 LOGOUT\r\n' >&3"
echo "-- POP3 XOAUTH2"
kx bash -c "exec 3<>/dev/tcp/imap/110; read -r -u3 g; printf 'AUTH XOAUTH2 $X_OK\r\n' >&3; read -r -u3 l; echo \"S: \${l%\$'\r'}\"; printf 'QUIT\r\n' >&3"

echo; echo "== master user (director master_user mode): test*director / master password =="
imapsession <<'EOF2' | grep -E '^S: a[12] |^S: \* STATUS'
LOGIN test*director director-master-secret
STATUS INBOX (MESSAGES)
LOGOUT
EOF2
imapsession <<'EOF2' | grep -E '^S: a1 '
LOGIN test*director wrong
LOGOUT
EOF2

echo; echo "== graceful drain: IDLE session gets BYE when its pod is deleted =="
POD=$($K get pods -l app.kubernetes.io/name=shardpost-server -o jsonpath='{.items[0].metadata.name}')
POD_IP=$($K get pod $POD -o jsonpath='{.status.podIP}')
( kx bash -c "exec 3<>/dev/tcp/$POD_IP/1143; read -r -u3 g; printf 'a1 LOGIN test test\r\na2 SELECT INBOX\r\na3 IDLE\r\n' >&3; while IFS= read -r -u3 -t 40 l; do l=\${l%\$'\r'}; case \"\$l\" in '+ '*) echo 'S: idling';; '* BYE'*) echo \"S: \$l\"; break;; esac; done" ) &
sleep 3
$K delete pod $POD --wait=false >/dev/null; echo "(pod $POD deleted)"
wait
$K rollout status deploy/shardpost-server --timeout=120s >/dev/null

echo; echo "== cephx key rotation without restart: bump keyGeneration on the CephClient =="
GEN=$(kubectl --context $CONTEXT -n rook-ceph get cephclient shardpost -o jsonpath='{.spec.security.cephx.keyGeneration}')
kubectl --context $CONTEXT -n rook-ceph patch cephclient shardpost --type merge -p "{\"spec\":{\"security\":{\"cephx\":{\"keyGeneration\":$((GEN+1))}}}}" >/dev/null
echo "keyGeneration $GEN -> $((GEN+1)); waiting for the server to pick up the new key (poll interval 60s)"
for i in $(seq 1 30); do
  if $K logs -l app.kubernetes.io/name=shardpost-server --tail=200 2>/dev/null | grep -q 'cephx key rotated'; then break; fi; sleep 5
done
$K logs -l app.kubernetes.io/name=shardpost-server --tail=200 | grep -E 'key rotat|old ceph connection' | sed 's/{"time[^,]*,//' | sort -u
echo "login after rotation:"; imapsession <<'EOF2' | grep -E '^S: a2 |^S: \* STATUS'
LOGIN test test
STATUS INBOX (MESSAGES)
LOGOUT
EOF2
