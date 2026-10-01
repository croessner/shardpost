#!/usr/bin/env bash
# Cluster smoke test of the account directory (scaling stage R1a), run
# after a rollout. Uses only shardpostctl and shardpost inside a server
# pod:
#
#   1. create a mailbox for a throwaway account and check its directory
#      entry (active, registry present),
#   2. purge the account through a planned job and check that entry and
#      registry are gone,
#   3. run the account backfill and print its completion marker.
#
#   hack/directory-smoke.sh <throwaway-account>
#
# The account must exist in the user database (mailbox create resolves it
# through Nauthilus) and must hold nothing worth keeping: the purge
# deletes all of its data. The script refuses an account that already has
# a registry unless FORCE=1.
set -euo pipefail

CONTEXT=${CONTEXT:-croessner}
NS=${NS:-shardpost}
DEPLOY=${DEPLOY:-deploy/shardpost-server}
ACCOUNT=${1:?usage: $0 <throwaway-account>}

inpod() { kubectl --context "$CONTEXT" -n "$NS" exec -i "$DEPLOY" -c server -- "$@"; }
ctl() { inpod shardpostctl "$@"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

# field <name> reads "name": value from indented JSON on stdin.
field() { sed -n "s/^ *\"$1\": \"\{0,1\}\([^\",]*\)\"\{0,1\},\{0,1\}$/\1/p" | head -1; }

echo "== directory before =="
before=$(ctl account directory --json "$ACCOUNT")
echo "$before"

if [ "$(echo "$before" | field registry)" = true ] && [ "${FORCE:-0}" != 1 ]; then
	fail "$ACCOUNT already has data; use a throwaway account (or FORCE=1)"
fi

echo; echo "== create a mailbox: registers the account =="
ctl mailbox create "$ACCOUNT" DirectorySmoke >/dev/null
after=$(ctl account directory --json "$ACCOUNT")
echo "$after"
[ "$(echo "$after" | field found)" = true ] || fail "no directory entry after the first mutation"
[ "$(echo "$after" | field state)" = active ] || fail "entry is not active"
[ "$(echo "$after" | field registry)" = true ] || fail "no registry after the first mutation"

echo; echo "== purge through a planned job =="
plan=$(ctl plan --json purge "$ACCOUNT" | field id)
[ -n "$plan" ] || fail "no plan id"
ctl apply -wait "$plan" || fail "purge job did not succeed"

gone=$(ctl account directory --json "$ACCOUNT")
echo "$gone"
[ "$(echo "$gone" | field found)" = false ] || fail "directory entry left after the purge"
[ "$(echo "$gone" | field registry)" = false ] || fail "registry left after the purge (ghost account)"

echo; echo "== repair finds nothing to do =="
ctl account repair "$ACCOUNT"

echo; echo "== account backfill =="
inpod shardpost index backfill accounts
ctl account directory --json "$ACCOUNT" | sed -n 's/^ *"backfill": //p'

echo; echo "== listings counted (imapceph_rados_listings_total) =="
inpod sh -c 'wget -qO- http://127.0.0.1:8080/metrics 2>/dev/null || curl -s http://127.0.0.1:8080/metrics' |
	grep '^imapceph_rados_listings_total' || echo "(metrics endpoint not reachable from the pod)"

echo; echo "OK: account directory smoke test passed for $ACCOUNT"
