#!/usr/bin/env bash
# Rename the external identities of ShardPost (2026-09-18):
#   OIDC client  imappoc-service      -> shardpost-service
#   health user  mail_health_imappoc  -> mail_health_shardpost
#   test users   cn/sn "IMAP POC n"   -> "ShardPost Test n"
#
#   hack/identity-cutover.sh secrets   # apply the new Nauthilus SOPS keys (add-only)
#   hack/identity-cutover.sh ldap      # add mail_health_shardpost, rename test-user cn/sn
#   hack/identity-cutover.sh oidc      # Nauthilus config and ShardPost OIDC secret
#   hack/identity-cutover.sh director  # director health user (needs the ldap step)
#   hack/identity-cutover.sh cleanup   # drop old Nauthilus keys, old director CA/key
#   hack/identity-cutover.sh ldap-cleanup  # delete mail_health_imappoc
#
# Every step is idempotent. Secret values travel only through pipes and
# process memory; nothing prints them.
set -euo pipefail

CONTEXT=${CONTEXT:-croessner}
DIRECTOR=${DIRECTOR:-$HOME/OpenCloud/Persönlich/Kubernetes/director}
NAUTHILUS=${NAUTHILUS:-$HOME/GoLandProjects/nauthilus-tests/kubernetes}
export SOPS_AGE_KEY_FILE=${SOPS_AGE_KEY_FILE:-$HOME/Library/Application Support/sops/age/keys.txt}
MGMT=(ssh -p 50216 support@mgmt.roessner-net.de)
K=(kubectl --context "$CONTEXT")
OIDC_PROD=$NAUTHILUS/secrets/prod/nauthilus-oidc-client-secrets.sops.yaml
OIDC_SHADOW=$NAUTHILUS/secrets/shadow/nauthilus-oidc-client-secrets.sops.yaml
HEALTH=$NAUTHILUS/secrets/prod/nauthilus-backend-health-check-secrets.sops.yaml
LDAPI='ldapi://%2Fvar%2Frun%2Fopenldap%2Fldapi'
HEALTH_DN='ou=health,ou=people,ou=it,dc=roessner-net,dc=de'

log() { printf '%s %s\n' "$(date -u +%H:%M:%S)" "$*"; }

# sops_value FILE KEY: print one stringData value (only into pipes).
sops_value() { sops -d --extract "[\"stringData\"][\"$2\"]" "$1"; }

# apply_sops FILE NAMESPACE: apply a SOPS secret after checking that every
# key already live keeps its value (no foreign pending change sneaks in).
apply_sops() {
  sops -d --output-type json "$1" | python3 -c '
import base64, json, subprocess, sys
doc = json.load(sys.stdin)
ns, ctx = sys.argv[1], sys.argv[2]
want = {k: v.encode() for k, v in doc.get("stringData", {}).items()}
want.update({k: base64.b64decode(v) for k, v in doc.get("data", {}).items()})
live = subprocess.run(["kubectl", "--context", ctx, "-n", ns, "get", "secret", doc["metadata"]["name"], "-o", "json"],
                      capture_output=True, check=True).stdout
changed = [k for k, v in json.loads(live)["data"].items() if k in want and base64.b64decode(v) != want[k]]
if changed:
    sys.exit("live values differ for %s; refusing" % changed)
out = subprocess.run(["kubectl", "--context", ctx, "apply", "-f", "-"], input=json.dumps(doc).encode(),
                     capture_output=True, check=True).stdout
print(out.decode().strip())' "$2" "$CONTEXT"
}

generation() { "${K[@]}" -n "$1" get deploy "$2" -o jsonpath='{.metadata.generation}'; }

# reloaded NS DEPLOY GEN: wait until Reloader restarted DEPLOY past GEN.
reloaded() {
  for _ in $(seq 60); do
    if [ "$(generation "$1" "$2")" -gt "$3" ]; then
      "${K[@]}" -n "$1" rollout status deploy/"$2" --timeout=600s
      return
    fi
    sleep 5
  done
  echo "$1/$2 was not restarted by Reloader" >&2
  exit 1
}

# nauthilus_secrets: apply the three secrets and wait for both restarts.
nauthilus_secrets() {
  local prod shadow
  prod=$(generation auth nauthilus)
  shadow=$(generation auth-test nauthilus-shadow)
  apply_sops "$OIDC_PROD" auth
  apply_sops "$OIDC_SHADOW" auth-test
  apply_sops "$HEALTH" auth
  reloaded auth nauthilus "$prod"
  reloaded auth-test nauthilus-shadow "$shadow"
}

# ldap_run SCRIPT: run a shell script as root on the LDAP provider host via
# mgmt; the script travels base64-encoded (no quoting), stdin is passed on.
ldap_run() {
  local b64
  b64=$(printf %s "$1" | base64 | tr -d '\n')
  "${MGMT[@]}" "sudo -n ssh ldap.roessner-net.de 'sudo -n bash -c \"\$(echo $b64 | base64 -d)\"'"
}

# The single-quoted text is the remote script; $pw and friends expand there.
# shellcheck disable=SC2016
ldap() {
  log "LDAP: mail_health_shardpost"
  # stdin: password and its salted SSHA512 hash (like the other health
  # accounts since 2026-09-14), one per line; both stay in memory.
  sops_value "$HEALTH" BACKEND_HEALTH_CHECK_SHARDPOST_TEST_PASSWORD | python3 -c '
import base64, hashlib, os, sys
pw = sys.stdin.read().encode()
salt = os.urandom(16)
print(pw.decode())
print("{SSHA512}" + base64.b64encode(hashlib.sha512(pw + salt).digest() + salt).decode())' | ldap_run '
set -euo pipefail
read -r pw
read -r hash
exists=$(podman exec --user 0 openldap ldapsearch -LLL -Q -Y EXTERNAL -H '"$LDAPI"' -b '"$HEALTH_DN"' "(uid=mail_health_shardpost)" dn | grep -c "^dn:" || true)
if [ "$exists" = 0 ]; then
  printf "%s\n" "dn: uid=mail_health_shardpost,'"$HEALTH_DN"'" "changetype: add" \
    "objectClass: top" "objectClass: account" "objectClass: extensibleObject" "objectClass: rnsMSDovecotAccount" \
    "uid: mail_health_shardpost" "uniqueIdentifier: mail_health_shardpost" \
    "description: Dovecot IMAP and ManageSieve health identity for shardpost" \
    "rnsMSDovecotUser: mail-health@shardpost.health.srvint.net" \
    "rnsMSRecipientAddress: mail-health@shardpost.health.srvint.net" \
    "rnsMSEnableDovecot: TRUE" "rnsMSOverQuota: FALSE" "userPassword: $hash" |
    podman exec -i --user 0 openldap ldapmodify -Q -Y EXTERNAL -H '"$LDAPI"'
fi
printf %s "$pw" | podman exec -i --user 0 openldap ldapwhoami -x -H '"$LDAPI"' -D "uid=mail_health_shardpost,'"$HEALTH_DN"'" -y /dev/stdin
for n in 1 2; do
  printf "%s\n" "dn: uid=rbdpoc$n,ou=people,ou=it,dc=roessner-net,dc=de" "changetype: modify" \
    "replace: cn" "cn: ShardPost Test $n" "-" "replace: sn" "sn: ShardPost Test $n" "-" \
    "replace: cn;x-hidden" "cn;x-hidden: ShardPost Test $n" "-" "replace: sn;x-hidden" "sn;x-hidden: ShardPost Test $n" |
    podman exec -i --user 0 openldap ldapmodify -Q -Y EXTERNAL -H '"$LDAPI"'
done
podman exec --user 0 openldap ldapsearch -LLL -Q -Y EXTERNAL -H '"$LDAPI"' -b ou=people,ou=it,dc=roessner-net,dc=de \
  "(|(uid=mail_health_shardpost)(uid=rbdpoc1)(uid=rbdpoc2))" uid cn rnsMSDovecotUser rnsMSEnableDovecot'
}

oidc() {
  # Precondition: live prod/shadow equal the repo apart from this change
  # (checked with kubectl diff before running; true since alpha.38).
  log "Nauthilus: identity config with shardpost-service"
  local prod shadow
  prod=$(generation auth nauthilus)
  shadow=$(generation auth-test nauthilus-shadow)
  make -C "$NAUTHILUS" apply-prod-manifests apply-shadow-manifests
  reloaded auth nauthilus "$prod"
  reloaded auth-test nauthilus-shadow "$shadow"

  log "ShardPost: OIDC client shardpost-service"
  sops_value "$OIDC_PROD" OIDC_CLIENT_SECRET_SHARDPOST_SERVICE | python3 -c '
import base64, json, subprocess, sys
secret = sys.stdin.read()
ctx = sys.argv[1]
live = json.loads(subprocess.run(["kubectl", "--context", ctx, "-n", "shardpost", "get", "secret",
                                  "shardpost-nauthilus-oidc", "-o", "json"], capture_output=True, check=True).stdout)
live["data"]["client-id"] = base64.b64encode(b"shardpost-service").decode()
live["data"]["client-secret"] = base64.b64encode(secret.encode()).decode()
subprocess.run(["kubectl", "--context", ctx, "replace", "-f", "-"], input=json.dumps(live).encode(),
               capture_output=True, check=True)' "$CONTEXT"
  "${K[@]}" -n shardpost rollout restart deploy/shardpost-server
  "${K[@]}" -n shardpost rollout status deploy/shardpost-server --timeout=300s
}

director() {
  log "director: health user mail_health_shardpost"
  make -C "$DIRECTOR" deploy
}

cleanup() {
  log "Nauthilus: drop IMAPPOC keys"
  for ns in auth auth-test; do
    if "${K[@]}" -n "$ns" get configmap nauthilus-config-file -o json | grep -q IMAPPOC; then
      echo "$ns config still references IMAPPOC; run oidc first" >&2
      exit 1
    fi
  done
  sops unset "$OIDC_PROD" '["stringData"]["OIDC_CLIENT_SECRET_IMAPPOC_SERVICE"]' || true
  sops unset "$OIDC_SHADOW" '["stringData"]["OIDC_CLIENT_SECRET_IMAPPOC_SERVICE"]' || true
  sops unset "$HEALTH" '["stringData"]["BACKEND_HEALTH_CHECK_IMAPPOC_TEST_PASSWORD"]' || true
  local prod shadow
  prod=$(generation auth nauthilus)
  shadow=$(generation auth-test nauthilus-shadow)
  sops -d "$OIDC_PROD" | "${K[@]}" -n auth apply -f -
  sops -d "$OIDC_SHADOW" | "${K[@]}" -n auth-test apply -f -
  sops -d "$HEALTH" | "${K[@]}" -n auth apply -f -
  reloaded auth nauthilus "$prod"
  reloaded auth-test nauthilus-shadow "$shadow"

  log "director: drop imappoc key and CA"
  "${K[@]}" -n director get secret director-backend-master -o json | python3 -c '
import json, sys
s = json.load(sys.stdin)
s["data"].pop("imappoc-password", None)
json.dump(s, sys.stdout)' | "${K[@]}" replace -f - >/dev/null
  "${K[@]}" -n director delete configmap director-imappoc-ca --ignore-not-found
  rm -f "$DIRECTOR/certificates/imap-poc-ca.crt"
  log "cleanup done"
}

ldap_cleanup() {
  log "LDAP: delete mail_health_imappoc"
  ldap_run "podman exec --user 0 openldap ldapdelete -Q -Y EXTERNAL -H '$LDAPI' 'uid=mail_health_imappoc,$HEALTH_DN' || true"
}

case ${1:-} in
  secrets) nauthilus_secrets ;;
  ldap-cleanup) ldap_cleanup ;;
  ldap | oidc | director | cleanup) "$1" ;;
  *) sed -n '2,15p' "$0"; exit 2 ;;
esac
