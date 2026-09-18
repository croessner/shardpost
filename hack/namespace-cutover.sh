#!/usr/bin/env bash
# Move the running server from namespace imap / pools imap-* to namespace
# shardpost / pools shardpost-* (0.13.0, 2026-09-18).
#
#   hack/namespace-cutover.sh prepare   # additive: namespace, pools, client, certs, secrets
#   hack/namespace-cutover.sh cutover   # maintenance window: stop, copy, start, re-route
#   hack/namespace-cutover.sh cleanup   # after acceptance: old namespace, client, RBAC
#
# The old pools imap-index / imap-data are never deleted here; they stay as
# the rollback copy until the owner deletes them explicitly.
# Secret values only travel through pipes; nothing prints them.
set -euo pipefail

CONTEXT=${CONTEXT:-croessner}
ROOT=$(cd "$(dirname "$0")/.." && pwd)
DIRECTOR=${DIRECTOR:-$HOME/OpenCloud/Persönlich/Kubernetes/director}
NAUTHILUS=${NAUTHILUS:-$HOME/GoLandProjects/nauthilus-tests/kubernetes}
K=(kubectl --context "$CONTEXT")

log() { printf '%s %s\n' "$(date -u +%H:%M:%S)" "$*"; }

# copy_secret SRC_NS SRC DST_NS DST: create DST with SRC's type and data.
copy_secret() {
  if "${K[@]}" -n "$3" get secret "$4" >/dev/null 2>&1; then
    log "secret $3/$4 exists"
    return
  fi
  "${K[@]}" -n "$1" get secret "$2" -o json | python3 -c '
import json, sys
s = json.load(sys.stdin)
json.dump({"apiVersion": "v1", "kind": "Secret", "type": s["type"], "data": s["data"],
           "metadata": {"name": sys.argv[2], "namespace": sys.argv[1]}}, sys.stdout)' "$3" "$4" |
    "${K[@]}" create -f - >/dev/null
  log "secret $3/$4 created from $1/$2"
}

# add_secret_key NS NAME FROM TO: add key TO with the value of key FROM.
add_secret_key() {
  "${K[@]}" -n "$1" get secret "$2" -o json | python3 -c '
import json, sys
s = json.load(sys.stdin)
src, dst = sys.argv[1], sys.argv[2]
if src not in s["data"]:
    sys.exit("missing key " + src)
s["data"][dst] = s["data"][src]
json.dump(s, sys.stdout)' "$3" "$4" | "${K[@]}" replace -f - >/dev/null
  log "secret $1/$2: key $4 set from $3"
}

pool_copy() {
  "${K[@]}" -n rook-ceph exec -i deploy/rook-ceph-tools -- sh -c 'cat > /tmp/pool-copy.py' <"$ROOT/hack/pool-copy.py"
  "${K[@]}" -n rook-ceph exec deploy/rook-ceph-tools -- python3 /tmp/pool-copy.py copy "$1" "$2"
}

# nauthilus_subject ENV: apply only the Lua subject ConfigMap (director.lua).
nauthilus_subject() {
  kubectl kustomize "$NAUTHILUS/apps/nauthilus/overlays/$1" --load-restrictor=LoadRestrictionsNone |
    "${K[@]}" create --dry-run=client -f - -o json | python3 -c '
import json, sys
dec, s, out = json.JSONDecoder(), sys.stdin.read().lstrip(), []
while s:
    d, n = dec.raw_decode(s)
    out += d["items"] if d.get("kind") == "List" else [d]
    s = s[n:].lstrip()
sel = [o for o in out if o["kind"] == "ConfigMap" and o["metadata"]["name"] == "nauthilus-lua-subject"]
assert len(sel) == 1 and "\"shardpost\"" in sel[0]["data"]["director.lua"]
json.dump(sel[0], sys.stdout)' | "${K[@]}" apply -f -
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

prepare() {
  grep -q 'newTag: 0.13.0' "$ROOT/deploy/overlays/croessner/kustomization.yaml" ||
    { echo "overlay does not pin the 0.13.0 images" >&2; exit 1; }
  "${K[@]}" apply -k "$ROOT/deploy/overlays/croessner-nauthilus-paused"
  copy_secret imap imap-nauthilus-oidc shardpost shardpost-nauthilus-oidc
  copy_secret imap imap-director-backend shardpost shardpost-director-backend
  copy_secret imap imap-rspamd-learning shardpost shardpost-learning
  copy_secret imap imaptest-creds shardpost imaptest-creds
  "${K[@]}" -n shardpost wait --for=condition=Ready certificate/shardpost-ca certificate/shardpost-tls --timeout=180s
  "${K[@]}" -n rook-ceph wait --for=jsonpath='{.status.phase}'=Ready cephblockpool/shardpost-index cephblockpool/shardpost-data cephclient/shardpost --timeout=300s
  "${K[@]}" -n shardpost get secret shardpost-ca -o jsonpath='{.data.ca\.crt}' | base64 -d >"$DIRECTOR/certificates/shardpost-ca.crt"
  log "director CA written to $DIRECTOR/certificates/shardpost-ca.crt"
  kubectl kustomize "$DIRECTOR" >/dev/null
  log "prepare done; server replicas 0, scrub suspended"
}

cutover() {
  test -s "$DIRECTOR/certificates/shardpost-ca.crt" || { echo "run prepare first" >&2; exit 1; }
  test "$("${K[@]}" -n shardpost get deploy shardpost-server -o jsonpath='{.spec.replicas}')" = 0 ||
    { echo "shardpost-server is not paused" >&2; exit 1; }

  log "stopping imap/imap-server and imap-scrub"
  "${K[@]}" -n imap patch cronjob imap-scrub -p '{"spec":{"suspend":true}}'
  "${K[@]}" -n imap scale deploy/imap-server --replicas=0
  "${K[@]}" -n imap wait --for=delete pod -l app.kubernetes.io/name=imap-server --timeout=300s
  if "${K[@]}" -n imap get jobs -o jsonpath='{.items[*].status.active}' | grep -q '[1-9]'; then
    echo "a scrub job is still running; wait for it, then rerun cutover" >&2
    exit 1
  fi

  log "copying pools"
  pool_copy imap-index shardpost-index
  pool_copy imap-data shardpost-data

  log "starting shardpost/shardpost-server"
  "${K[@]}" apply -k "$ROOT/deploy/overlays/croessner-nauthilus"
  "${K[@]}" -n shardpost rollout status deploy/shardpost-server --timeout=300s

  log "director: new backends, CA and secret keys"
  add_secret_key director director-backend-master imappoc-password shardpost-password
  make -C "$DIRECTOR" deploy

  log "nauthilus: mailShard=shardpost"
  local prod shadow
  prod=$(generation auth nauthilus)
  shadow=$(generation auth-test nauthilus-shadow)
  nauthilus_subject prod
  nauthilus_subject shadow
  reloaded auth nauthilus "$prod"
  reloaded auth-test nauthilus-shadow "$shadow"
  log "cutover done; run the acceptance checks"
}

cleanup() {
  "${K[@]}" -n shardpost rollout status deploy/shardpost-server --timeout=30s
  "${K[@]}" delete namespace imap --wait=true --timeout=300s
  "${K[@]}" -n rook-ceph delete rolebinding,role imap-ceph-credentials-reader --ignore-not-found
  "${K[@]}" -n rook-ceph delete cephclient imap --ignore-not-found
  log "cleanup done; pools imap-index and imap-data are kept"
}

case ${1:-} in
  prepare | cutover | cleanup) "$1" ;;
  *) sed -n '2,11p' "$0"; exit 2 ;;
esac
