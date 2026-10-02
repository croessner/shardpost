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
- delivery latency (p50, p90, p99) and compare-and-swap retries, by
  kind and by object class;
- with `measure --contention P`, the same for `P` deliveries at a time
  into one account;
- with `measure --search K` and `--expunge N`, the cost of a search query
  and of an expunge, and a fingerprint of the search results;
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
  `ceph.data_pool` of the configuration (file or environment) or their
  built-in defaults `shardpost-index` and `shardpost-data`, or both name
  the same pool;
- the account prefix does not start with `bench-`;
- `measure` finds a pool without the bench marker (`~bench/marker`,
  written by the first `populate`);
- `populate` finds a pool without the marker that holds any object, in
  the index pool in particular an account directory
  (`~accounts/ix.accounts/head`): a pool somebody uses. A missing marker
  is written only into a pool that is empty, pool by pool. The objects
  `rbd_info` and `rbd_trash` that `rbd pool init` leaves in the default
  namespace do not count: Rook initialises every CephBlockPool for RBD.

The name comparison is only as good as the configuration the bench is
given; the marker check does not depend on it. The Job sets the
production names in its environment, so check them against the server's
configuration before the run ([below](#before-the-run)).

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

### PG count

The bench pools get a fixed PG count from the start (`pg_num` and
`pg_num_min` 32, autoscaler on `warn`). With the autoscaler alone a new
pool starts with few PGs and is split while `populate` fills it; the
splits move data and RocksDB contents around and distort the latency and
cache figures. 32 PGs per pool on the three `rgw` OSDs put 32 PG shards
of each pool on every OSD (replica size 3, and 2+1 chunks), 64 for both:
enough to spread the load, and well below `mon_max_pg_per_osd` (250)
together with the production and RGW pools there. Check the headroom and
the result:

```bash
ceph osd df tree | awk 'NR==1 || /rgw/'          # column PGS per OSD
ceph config get mon mon_max_pg_per_osd
ceph osd pool get shardpost-load-index pg_num
ceph osd pool get shardpost-load-data pg_num
```

With more headroom, 64 per pool gives a finer spread; keep both pools
the same for comparable numbers. In production the PG count belongs to
the scale: the scaling plan expects 4096 to 16384 PGs per pool for
millions of accounts (spread over correspondingly many OSDs, at about
100 to 200 PG shards per OSD). The bench's objects per account and the
OSD count give `pg_num` for a target size; set it with `pg_num_min` (or
`bulk true`) so that the autoscaler does not start small and split under
load.

Without Rook, in a shell with the admin key, take the rule and the
profile of the production pools:

```bash
ceph osd pool get shardpost-index crush_rule
ceph osd pool get shardpost-data erasure_code_profile
ceph osd pool get shardpost-data crush_rule
ceph osd pool create shardpost-load-index 32 32 replicated <index rule>
ceph osd pool set shardpost-load-index pg_autoscale_mode warn
ceph osd pool set shardpost-load-index pg_num_min 32
ceph osd pool set shardpost-load-index size 3
ceph osd pool create shardpost-load-data 32 32 erasure <profile> <data rule>
ceph osd pool set shardpost-load-data pg_autoscale_mode warn
ceph osd pool set shardpost-load-data pg_num_min 32
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

## Before the run

The bench pools sit on the same `rgw` OSDs as production, so the run
competes with the mail service for them.

- **Off-peak.** Start it when the mail load is low; `populate` with the
  Job's `--parallel 4` (the default) keeps four deliveries in flight.
- **Three hosts.** Failure domain host needs the `rgw` OSDs on three
  different hosts, or the PGs of the bench pools stay undersized:

  ```bash
  ceph osd tree | grep -E 'host|rgw'
  ceph osd crush rule dump shardpost-load-index | jq '.steps'
  ```

- **Production pool names.** The server reads its pools from
  `server.yaml` in the generated ConfigMap `shardpost-server-config-<hash>`;
  its name changes with every edit, so the Job cannot reference it and
  sets the names itself. Compare them:

  ```bash
  cm=$(kubectl --context croessner -n shardpost get deployment shardpost-server \
    -o jsonpath='{.spec.template.spec.volumes[?(@.name=="server-config")].configMap.name}')
  kubectl --context croessner -n shardpost get configmap "$cm" -o jsonpath='{.data.server\.yaml}' \
    | sed -n '/^ceph:/,/^[a-z]/p'
  grep -A1 -E 'CEPH_(INDEX|DATA)_POOL' hack/bench-job.yaml
  ```

  Whatever the names, the bench refuses a pool that holds objects but
  no bench marker, and its cephx user can reach the bench pools only.
- **Network.** The NetworkPolicies of the overlay
  (`shardpost-server-ingress`, `shardpost-admin-ingress`) select
  `app.kubernetes.io/name: shardpost-server` and restrict ingress only;
  nothing in `deploy/` limits egress, so the bench pod
  (`app.kubernetes.io/name: shardpost-bench`) reaches the monitors, the
  OSDs and the Kubernetes API (for the Rook secret) as the scrub
  CronJob does. Check for policies from outside the repository before
  the run:

  ```bash
  kubectl --context croessner -n shardpost get networkpolicy
  kubectl --context croessner get globalnetworkpolicies.crd.projectcalico.org 2>/dev/null
  ```

## Watching and stopping

While it runs, watch the cluster and the OSD latency:

```bash
watch -n 10 'ceph -s; ceph osd perf | sort -k3 -n | tail -5'
ceph osd pool stats shardpost-load-index
ceph osd pool stats shardpost-load-data
```

Stop it when commit/apply latencies of the `rgw` OSDs climb well above
their usual values, `ceph -s` reports slow ops, or the mail service
slows down:

```bash
kubectl --context croessner -n shardpost delete job shardpost-bench
```

The pod gets SIGTERM; `populate` stops after the deliveries in flight,
and a later run continues where it stopped. A smaller `--parallel` lowers
the load of the next run. The Job's `activeDeadlineSeconds` (4 hours)
ends a run that goes on too long the same way.

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
ranges fill in parallel; each needs a Job name of its own (copy the
manifest and change `metadata.name`, say `shardpost-bench-2`), and
together they multiply the load. To grow the accounts, run it again with a
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
  methods). The latency, `deliveries_per_second` and the
  compare-and-swap retries (`cas_retries.by_kind`, `by_class`) come from
  a round without the log. Both rounds deliver into populated accounts,
  one delivery at a time.
- `contention` (only with `measure --contention P`): the same two rounds
  again, with `P` deliveries at a time into each probe account
  (`--probe-messages` each; the accounts one after the other). The
  deliveries contend for the account's registry, INBOX header, id map
  and posting shards. `cas_retries.by_class` shows where they collide
  (`registry`, `mbox`, `ftsmap`, `fts`, …) and `ops_per_delivery`
  includes the reads and writes of the lost rounds; a lost round of the
  omap compare-and-swap (`kind` `omap`: registry, account, id map,
  posting shards, mailbox UID shards; not the mailbox header) waits 1 to 10 ms times the rounds it lost before it
  reads again. Compare `deliveries_per_second` and the latency with the
  `delivery` rounds. `--contention` is not `--parallel`, which only sets
  how many accounts the inventory and `populate` work on at once.
- `search` (only with `measure --search K`): runs first, before any
  delivery. Every probe account answers `K` queries from a fixed set
  drawn from the generator's vocabulary (whole words, word starts and
  inner parts of 3 to 6 characters; frequent and rare words; `TEXT`,
  `BODY`, `SUBJECT`, `FROM`), each in a view of its own: the candidates
  of the IMAP substring search (`SearchCandidates`, `TEXT` as header
  and body) and the word search (`SearchFields`). `ops_per_call` and
  the latency are per query, the view's first check of the index
  included. `queries[]` gives per query the messages found
  (`candidates`, `words`, `not_narrowed` for accounts where the index
  cannot narrow) and a SHA-256 `fingerprint` of the results (account,
  field set, GUID); `fingerprint` covers all queries. A run with
  `--probe-messages 0` and without `--expunge` leaves the accounts as
  they were, so two such runs on the same accounts, one before and one
  after a change of the index (a conversion, a new layout), must report
  the same `fingerprint`; where they differ, `queries[]` shows which
  query. `mismatch` says that the two rounds of one run found different
  messages.
- `expunge` (only with `measure --expunge N`): runs last. Every probe
  account loses its `N` oldest INBOX messages in each of two rounds, one
  EXPUNGE per message in a view of its own; `ops_per_call`, the latency
  and the compare-and-swap retries are per expunge. The messages are
  gone afterwards (a later `populate` does not refill them).
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
