#!/bin/sh
# Starts a throwaway single-node Ceph cluster (one monitor, one OSD on the
# in-memory object store, no authentication) inside the lint image and
# runs the given command against it:
#
#   hack/ceph-test-cluster.sh go test -race -tags tentacle ./...
#
# The command sees SHARDPOST_TEST_CEPH_MON (the monitor address),
# SHARDPOST_TEST_CEPH_KEY (a key the client library accepts; the cluster
# does not check it) and SHARDPOST_TEST_CEPH_INDEX_POOL /
# SHARDPOST_TEST_CEPH_DATA_POOL. The cephstore tests that need a cluster
# skip without them. Everything lives under $TMPDIR and goes with the
# container; nothing here touches a real cluster.
set -eu

D=${TMPDIR:-/tmp}/shardpost-ceph
rm -rf "$D"
mkdir -p "$D/mon" "$D/osd0" "$D/run"

fsid=$(cat /proc/sys/kernel/random/uuid)

cat >"$D/ceph.conf" <<CONF
[global]
fsid = $fsid
mon_host = v2:127.0.0.1:3300
auth_cluster_required = none
auth_service_required = none
auth_client_required = none
osd_pool_default_size = 1
osd_pool_default_min_size = 1
mon_allow_pool_size_one = true
mon_warn_on_pool_no_redundancy = false
osd_crush_chooseleaf_type = 0
osd_objectstore = memstore
memstore_device_bytes = 2147483648
osd_class_dir = /usr/lib64/rados-classes
run_dir = $D/run
log_file = $D/\$name.log
pid_file = $D/run/\$name.pid
admin_socket = $D/run/\$name.asok
mon_data = $D/mon
osd_data = $D/osd0
keyring = /dev/null
CONF

export CEPH_CONF="$D/ceph.conf"

monmaptool --create --addv a '[v2:127.0.0.1:3300]' --fsid "$fsid" "$D/monmap" >/dev/null
ceph-mon --mkfs -i a --monmap "$D/monmap" 2>/dev/null
ceph-mon -i a

id=$(ceph --connect-timeout 30 osd create)
ceph-osd -i "$id" --mkfs 2>/dev/null
ceph-osd -i "$id" 2>/dev/null

up=0
for _ in $(seq 1 120); do
	if ceph osd stat 2>/dev/null | grep -q "1 up"; then
		up=1
		break
	fi

	sleep 1
done

if [ "$up" != 1 ]; then
	echo "ceph-test-cluster: the OSD did not come up" >&2
	exit 1
fi

for pool in shardpost-test-index shardpost-test-data; do
	ceph osd pool create "$pool" 8 >/dev/null 2>&1
	ceph osd pool set "$pool" size 1 --yes-i-really-mean-it >/dev/null 2>&1
done

# The pools are usable once an object can be written.
for _ in $(seq 1 60); do
	if echo probe | rados -p shardpost-test-index put probe - 2>/dev/null; then
		rados -p shardpost-test-index rm probe
		break
	fi

	sleep 1
done

key=$(ceph-authtool --gen-print-key)

export SHARDPOST_TEST_CEPH_MON=v2:127.0.0.1:3300
export SHARDPOST_TEST_CEPH_KEY="$key"
export SHARDPOST_TEST_CEPH_INDEX_POOL=shardpost-test-index
export SHARDPOST_TEST_CEPH_DATA_POOL=shardpost-test-data

rc=0
"$@" || rc=$?

kill "$(cat "$D/run/osd.$id.pid")" "$(cat "$D/run/mon.a.pid")" 2>/dev/null || true

exit "$rc"
