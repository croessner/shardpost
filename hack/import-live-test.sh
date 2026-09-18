#!/usr/bin/env bash
# Live test of the account import against RADOS and a real Dovecot source
# (hack/import-live-test.yaml). Reads the source account read-only (EXAMINE,
# BODY.PEEK) and imports it into the throwaway namespace "importtest".
#
#   hack/import-live-test.sh run <source-account>   import twice, compare, scrub dry-run
#   hack/import-live-test.sh cleanup                 empty the namespace, remove the pod
#
# Needs the Secret shardpost-migrate-source (see docs/admin-api.md).
set -euo pipefail

CONTEXT=${CONTEXT:-croessner}
NS=shardpost
POD=shardpost-importtest
HERE=$(cd "$(dirname "$0")" && pwd)
k() { kubectl --context "$CONTEXT" -n "$NS" "$@"; }
inpod() { k exec "$POD" -c server -- "$@"; }

# imap <commands...>: runs IMAP commands as importtest on the test pod.
imap() {
	local script='exec 3<>/dev/tcp/127.0.0.1/1143; read -r _ <&3; n=0
for c in "a LOGIN importtest importtest" "$@" "z LOGOUT"; do printf "%s\r\n" "$c" >&3
  tag=${c%% *}; while read -r l <&3; do printf "%s\n" "${l%$'"'"'\r'"'"'}"; case $l in "$tag "*) break;; esac; done; done'
	inpod bash -c "$script" imap "$@"
}

run() {
	local source=$1
	k apply -f "$HERE/import-live-test.yaml"
	k wait --for=condition=Ready "pod/$POD" --timeout=180s

	echo "== first run"
	inpod shardpostctl -timeout 10m migrate -source-user "$source" --json importtest | tee /tmp/import-run1.json
	echo "== second run (must change nothing)"
	inpod shardpostctl -timeout 10m migrate -source-user "$source" --json importtest | tee /tmp/import-run2.json

	echo "== target state per mailbox"
	imap 'b LIST "" "*" RETURN (STATUS (UIDVALIDITY UIDNEXT HIGHESTMODSEQ MESSAGES))'

	echo "== QRESYNC with the source state of INBOX (expect no FETCH/VANISHED)"
	local uv ms
	uv=$(python3 -c 'import json,sys; r=json.load(open(sys.argv[1])); print(next(m["uidvalidity"] for m in r["mailboxes"] if m["name"]=="INBOX"))' /tmp/import-run2.json)
	ms=$(python3 -c 'import json,sys; r=json.load(open(sys.argv[1])); print(next(m["highest_modseq"] for m in r["mailboxes"] if m["name"]=="INBOX"))' /tmp/import-run2.json)
	imap "b ENABLE QRESYNC" "c SELECT INBOX (QRESYNC ($uv $ms))"
	echo "== QRESYNC 50 modseqs behind (expect FETCH lines)"
	imap "b ENABLE QRESYNC" "c SELECT INBOX (QRESYNC ($uv $((ms - 50))))" | grep -c FETCH || true

	echo "== scrub (dry run)"
	inpod env SCRUB_USERS=importtest SCRUB_DRY_RUN=true SCRUB_GRACE_SECONDS=0 shardpost scrub
}

cleanup() {
	# Expunge everything and delete the folders, then let the scrubber
	# reap the blobs (two runs: orphans go on the second sighting).
	local boxes
	boxes=$(imap 'b LIST "" "*"' | sed -n 's/^\* LIST ([^)]*) "[^"]*" "\{0,1\}\([^"]*\)"\{0,1\}$/\1/p' | sort -r)
	while IFS= read -r mb; do
		[[ -z $mb ]] && continue
		imap "b SELECT \"$mb\"" 'c STORE 1:* +FLAGS.SILENT (\Deleted)' 'd EXPUNGE' >/dev/null || true
		[[ $mb == INBOX ]] || imap "b DELETE \"$mb\"" >/dev/null || true
	done <<<"$boxes"
	inpod env SCRUB_USERS=importtest SCRUB_GRACE_SECONDS=0 shardpost scrub
	inpod env SCRUB_USERS=importtest SCRUB_GRACE_SECONDS=0 shardpost scrub
	k delete -f "$HERE/import-live-test.yaml" --wait=false
}

case ${1:-} in
run) run "${2:?source account}" ;;
cleanup) cleanup ;;
*)
	echo "usage: $0 run <source-account> | cleanup" >&2
	exit 64
	;;
esac
