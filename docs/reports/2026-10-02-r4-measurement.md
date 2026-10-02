# Scaling stage R4: what an account costs in Ceph

Date: 2026-10-02

## Set-up

- `shardpost bench` 0.43.0 in two separate bench pools on the `rgw` OSDs
  of the croessner cluster (3 hosts, one 186 GiB OSD each, where the
  production pools live): `shardpost-load-index` replicated size 3,
  `shardpost-load-data` erasure coded 2+1, 32 PGs each; cephx user
  `client.shardpost-bench` with caps on these pools only
  (`hack/bench-pools.yaml`, `hack/bench-job.yaml`).
- 1000 accounts with six mailboxes each, 50 messages per account
  through the LMTP delivery pipeline (Sieve, reservation, blob,
  full-text index, index entry, account log); sizes 60 % 1–8 KiB, 30 %
  8–64 KiB, 10 % 64 KiB–1 MiB; `fts_shards` 16, `mbox_shard_size`
  100000.
- The production service kept running on the same OSDs; the cluster
  stayed `HEALTH_OK` with OSD commit latencies of 4–5 ms.

## Results

**Populate** (4 deliveries in flight): 50000 deliveries, none failed,
3.2 GiB in 7394 s (6.8/s); delivery latency p50 578 ms, p90 748 ms,
p99 949 ms; 842 compare-and-swap retries (0.017 per delivery, all on
omap).

**Per account** (50 messages):

| Pool | Class | Objects | omap keys | omap bytes |
|---|---|---|---|---|
| index | `ftst` (trigram dictionary) | 252 | 63273 | 862 KiB |
| index | `fts` (postings) | 16 | 11958 | 164 KiB |
| index | `ftsh` | 15.4 | 50 | 12.6 KiB |
| index | `mbox` (6 mailboxes) | 6 | 136 | 46 KiB |
| index | `account`, `ftsmap`, registry | 3 | 173 | 12 KiB |
| index | total | 292.4 | 75590 | 1.1 MiB |
| data | `msg` | 50 | – | 3.3 MiB data |

The trigram dictionary is 86 % of the index objects and 84 % of the omap
keys of an account. Cluster statistics: 845 MiB omap stored in the bench
index pool (2.5 GiB with three replicas), 3.4 GiB in the data pool (5.2
GiB raw, overhead 1.58 for EC 2+1).

**Operations per delivery** into a populated account (sequential, 200
deliveries): 245.6 RADOS operations, 45 reads and 200.6 writes.

| Class | Reads | Writes |
|---|---|---|
| `ftst` | 0 | 176.6 |
| `fts` | 32 | 16 |
| `account`, registry, `ftsmap`, `mbox`, `metadata`, `sieve`, `ftsh` | 13 | 7 |
| `msg` (blob) | 0 | 1 |

The full-text index makes 225 of the 245.6 operations (92 %): every
delivery writes about 177 trigram dictionary shards and reads and writes
all 16 postings shards. Latency p50 289 ms, p99 524 ms, no
compare-and-swap retry.

**Opening an account:** `Store.User()` sends no operation (0.04 ms);
`User` + mailbox list + INBOX state costs 4 reads (p50 4.7 ms).

**OSD caches** (`osd_memory_target` 3.2 GiB, the three `rgw` OSDs after
the run): onode cache 160–185 k onodes (112–130 MiB), RocksDB block
cache share 91–104 MiB, BlueFS DB 3.3–3.9 GiB per OSD. Each OSD holds a
replica of all 293 k bench index objects, more than its onode cache
keeps.

**Extrapolated to 10⁶ accounts** (50 messages each): index 2.9 × 10⁸
objects, 7.6 × 10¹⁰ omap keys, 1.0 TiB omap per replica; data 5 × 10⁷
objects, 3.2 TiB (5.0 TiB raw).

## Assessment

- **B14 (full-text write amplification) is justified.** The trigram
  dictionary and the 16 postings shards dominate everything the plan
  wanted to measure: objects and omap per account, operations per
  delivery, and with them OSD write load, onode cache and RocksDB size.
  Batching the dictionary writes (fewer, larger objects per delivery)
  and touching fewer postings shards per delivery promise the largest
  gain of any remaining stage.
- **B15 (small mails) is not justified now.** An account has 50 data
  objects against 292 index objects; a delivery makes one data write
  against 200 index writes. The EC overhead of 1.58 is what 2+1 costs
  anyway; packing small mails would add complexity for a minor share.
- **Placement groups:** at scale the index pool needs the 4096–16384 PGs
  of plan §4 (set with `pg_num_min` or `bulk`, see
  `docs/operations/bench.md`); with 32 PGs a bench PG held about 9 k
  index objects.
- `Store.User()` costs nothing measurable; opening a session is 4 reads.

The bench pools stay until B14 has been measured against them; they are
deleted with `kubectl --context croessner delete -f hack/bench-pools.yaml`.
