# Bench: what an account costs in Ceph

[Operations](README.md) · [Manual page](../man/man1/shardpost-bench.1) · [Storage model](../architecture/storage.md)

`shardpost bench` is the measurement of the scaling plan's stage R4
(before B14, fewer full-text writes per delivery, and B15, small mails
outside the erasure coded pool). It fills two pools of its own with
accounts and messages, writing directly through the RADOS store, and
reports per account and per delivery:

- objects and bytes per pool and object class (`mbox`, `msg`, `fts`,
  `ftst`, `ftsh`, `ftsmap`, registry, account, …), with omap keys and
  bytes, and the raw space the data objects take in the erasure coded
  pool;
- RADOS operations per delivery by object class (reads, writes, class
  method calls);
- delivery latency (p50, p90, p99) and compare-and-swap retries;
- the cost of `Store.User()` and of opening a session.

The per-account values are then extrapolated (default: to one million
accounts). The RocksDB and onode cache sizes come from the OSDs
([below](#rocksdb-and-onode-caches)).

The run is scaled down on purpose: about 1000 accounts with 50 messages
each, raised with `--accounts` and `--messages` when a value does not
settle. It needs no Nauthilus, no IMAP and no LMTP login: the messages go
through the same delivery pipeline LMTP uses (Sieve, delivery headers,
reservation, blob, full-text index, index entry, account change log),
called in the process.

## Safety

The bench writes into the bench pools only, and it refuses to start
(exit status 2) when

- `--index-pool` or `--data-pool` is missing, names `ceph.index_pool` or
  `ceph.data_pool` of the configuration, or both name the same pool, or
  the configuration names no production pools;
- the account prefix does not start with `bench-`;
- `measure` finds no bench marker (`~bench/marker`, written by the first
  `populate`) in the pools;
- `populate` finds an index pool without the marker that holds an
  account directory (`~accounts/ix.accounts/head`), that is a pool a
  server uses.

The store's own objects (the account directory in `~accounts`) are
written into the bench pools; `~admin` and `~accounts` of the production
pools are not touched. Delete the bench pools when the measurement is
done.

## Bench pools

The pools mirror the production layout of the croessner overlay: the
index pool replicated with size 3, the data pool erasure coded 2+1, both
with failure domain host on the device class `rgw` (OSD 3/4/5), where the
production CRUSH rules `shardpost-index-200gb` and
`shardpost-data-200gb` place the production pools. Rook creates a CRUSH
rule for each bench pool with that failure domain and device class.
[`hack/bench-pools.yaml`](../../hack/bench-pools.yaml) holds the pools
and the bench credentials (below):

```bash
kubectl --context croessner apply -f hack/bench-pools.yaml
kubectl --context croessner -n rook-ceph get cephblockpool shardpost-load-index shardpost-load-data
```

Without Rook, in a shell with the admin key, take the rule and the
profile of the production pools:

```bash
ceph osd pool get shardpost-index crush_rule
ceph osd pool get shardpost-data erasure_code_profile
ceph osd pool get shardpost-data crush_rule
ceph osd pool create shardpost-load-index 32 32 replicated <index rule>
ceph osd pool set shardpost-load-index size 3
ceph osd pool create shardpost-load-data 32 32 erasure <profile> <data rule>
ceph osd pool application enable shardpost-load-index shardpost
ceph osd pool application enable shardpost-load-data shardpost
```

Delete them afterwards, with everything the bench wrote and the bench
credentials:

```bash
kubectl --context croessner delete -f hack/bench-pools.yaml
# without Rook (needs mon_allow_pool_delete):
ceph osd pool rm shardpost-load-index shardpost-load-index --yes-i-really-really-mean-it
ceph osd pool rm shardpost-load-data shardpost-load-data --yes-i-really-really-mean-it
```

## Ceph permissions

The bench runs as a cephx user of its own, `client.shardpost-bench`,
whose caps name the bench pools only, so the bench cannot reach the
production pools at all and the production client keeps its caps:

```
mon 'allow r'
osd 'allow rwx pool=shardpost-load-index, allow rwx pool=shardpost-load-data'
```

`x` is needed for the object classes (`cls_lock`); `mon 'allow r'` for
the pool statistics. Rook creates the key from the CephClient
`shardpost-bench` and stores it in the Secret
`rook-ceph-client-shardpost-bench`. The Job runs under the ServiceAccount
`shardpost-bench`, whose Role may read exactly that Secret and the
ConfigMap `rook-ceph-mon-endpoints`; with `CEPH_USER=shardpost-bench` the
server derives the Secret's name. All of it is in
`hack/bench-pools.yaml`.

## Running the Job

[`hack/bench-job.yaml`](../../hack/bench-job.yaml) runs `populate` as an
init container and `measure` after it, with the server's image, the
restricted Pod Security profile and the production pool names in
`CEPH_INDEX_POOL`/`CEPH_DATA_POOL` (for the guard only; the bench writes
where `--index-pool`/`--data-pool` say). `MBOX_SHARD_SIZE` and the quota
match the production configuration; `server.fts_shards` is the default
(16) there as here.

```bash
kubectl --context croessner -n shardpost apply -f hack/bench-job.yaml
kubectl --context croessner -n shardpost logs -f job/shardpost-bench -c populate
kubectl --context croessner -n shardpost logs job/shardpost-bench -c measure \
  | sed -n '/^{$/,$p' > measure.json
kubectl --context croessner -n shardpost logs job/shardpost-bench -c measure | grep -v '^{"time"'
kubectl --context croessner -n shardpost delete job shardpost-bench
```

`populate` resumes where it stopped: delete the Job and apply it again;
complete accounts are skipped. Several Jobs with disjoint `--first`
ranges fill in parallel. To grow the accounts, run it again with a
higher `--messages`; to measure a part of many accounts, use
`measure --sample-every 10`.

## Reading the report

- `inventory.per_account.<pool>.<class>`: the mean, minimum and maximum
  per account. The full-text shards have a fixed count per account
  (`fts/` and `ftsh/`: `server.fts_shards`, `ftst/`: 256), so they do
  not grow with the messages; `msg` grows with the messages and their
  size (one object per `ceph.chunk_size`).
- `inventory.per_message`: the account mean divided by the message
  count, meaningful for the data pool and the index entries.
- `data_raw`: the estimated raw space of the data objects with the
  profile's `k`, `m`, stripe unit and BlueStore's allocation unit
  (`--ec-k`, `--ec-m`, `--stripe-unit`, `--min-alloc`): every object takes
  at least one allocation unit on each of `k+m` OSDs, which is what B15
  is about.
- `delivery.ops_per_delivery.<pool>.<class>`: operations sent to an OSD
  per delivery, counted from the client library's objecter log
  (`debug_objecter` 10, one line per operation) in a round of its own;
  `reads`, `writes` (at least one write step) and `calls` (object class
  methods). The latency and the compare-and-swap retries come from a
  round without the log. Both rounds deliver into populated accounts.
- `user_open`, `session_open`: `Store.User()` opens two I/O contexts and
  sends nothing to an OSD; a session adds the registry and INBOX reads.
  `heap_bytes_per_view` is the Go heap of an open view (the client
  library's own memory is not in it).
- `pools`: the cluster's statistics of the bench pools; the OSDs report
  them every few seconds, so they lag behind the run.

The operations are counted in the bench rather than with the
`imapceph_rados_op_seconds` histogram: that histogram times four store
paths only (header read and CAS, entry list and get), so its `_count`
cannot add up to the operations of a delivery.

## RocksDB and onode caches

The bench does not query the OSDs. Collect their figures before
`populate`, after it, and after `measure`, for every OSD that holds
placement groups of the bench pools, in the Rook toolbox:

```bash
ceph pg ls-by-pool shardpost-load-index -f json | jq -r '.pg_stats[].acting[]' | sort -nu   # the OSDs
ceph tell osd.0 perf dump bluestore | jq '.bluestore | with_entries(select(.key | test("onode")))'
ceph tell osd.0 perf dump rocksdb
ceph tell osd.0 dump_mempools | jq '.mempool.by_pool | {bluestore_cache_onode, bluestore_cache_meta, bluestore_cache_data, bluestore_cache_other}'
ceph tell osd.0 config get osd_memory_target
ceph osd df tree
ceph df detail
```

On an OSD host, `ceph daemon osd.0 perf dump` reads the same through the
admin socket. `bluestore_cache_onode` (items and bytes) is the onode
cache, `bluestore_cache_meta` RocksDB's block cache share; with the
object counts of the inventory they give the cache needed per account.
