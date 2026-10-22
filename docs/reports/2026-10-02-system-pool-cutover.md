# System pool cutover on croessner (0.48.0)

Date: 2026-10-02

## Change

The store-wide namespaces of the croessner cluster (`~admin`,
`~accounts`, `~learning`, `~server`) moved from `shardpost-index` to the
system pool `shardpost-system` with `shardpost system migrate`
(stage B11b, release 0.48.0; plan `temp/12-system-pool.md`).

## Before

- 0.48.0 rolled out to server, push worker and the scrub CronJob image;
  `hack/system-job.yaml` pinned to the 0.48.0 digest.
- `nodelete` set on `shardpost-index`, `shardpost-data` and
  `shardpost-system`; the mons report no time skew.
- `system migrate --check` in normal operation: target and probe passed;
  it reported the running registrations, the push lease and the watchers
  as expected — and the worker locks of the running servers as blocking
  (exit 2). The runbook expects exit 0 there; see "Findings".

## Window (UTC)

| Time | Step |
|---|---|
| 19:56:50 | scrub CronJob suspended, push worker and servers scaled to 0 |
| 19:57:32 | all pods gone |
| 19:57:41–19:57:44 | `system migrate --to shardpost-system`: exit 0 at the first attempt, run `00720635caff`; 238 objects (90 924 bytes of data) copied, 226 moved to the quarantine, 12 index heads poisoned, fence `done` |
| 19:58 | `system verify`: exit 0 |
| 19:58:48 | configuration switched, `make deploy`, CronJob resumed |
| 19:58:56 | servers and push worker running again |

Mail service was down for about two minutes; deliveries waited in the
MTA queue.

## Checks after the switch

- `shardpostctl server list`: the new server and push registrations name
  `shardpost-system (id 23)`.
- `account list`: 33 accounts; `job list`: the old jobs are there;
  `fts layout3 status`: the mark is read from the new pool.
- A job created after the switch (`hack/kvqueue-smoke.sh`) exists in the
  system pool and not in the index pool.
- `hack/auth-smoke.sh`, `hack/kvqueue-smoke.sh`, `hack/learning-smoke.sh`
  (mode read), `hack/r3-smoke.sh`, `hack/r5-smoke.sh` (JMAP delayed send
  and delivery): passed.
- imaptest: 72 groups, 0 failed, 8 skipped; base 0/460, extensions
  0/272.
- `system verify --source-only`: exit 0 — at the old place only the
  fence, the poisoned heads, the sweep's objects and the quarantine.
- No error or warning in the logs of servers and push worker.
- `ceph df`: `shardpost-system` holds 260 objects (1 MiB).

The quarantine stays until `system cleanup --run 00720635caff --confirm
00720635caff`, at the earliest seven days after the migration and after
the owner's confirmation. It was deleted on 2026-10-22
([quarantine cleanup](2026-10-22-system-pool-quarantine-cleanup.md)).

## Findings

- **`migrate --check` in normal operation** counts the worker locks of
  the running servers as blocking, so the documented pre-check cannot
  end with exit 0 while the cluster serves. After the scale-down the
  locks were released by the orderly shutdown and the real run passed at
  once. To fix: treat locks held by fresh registrations like the leases
  in `--check`.
- **Runbook, server configuration:** the croessner overlay gives the
  server its configuration as `server.yaml` only (its env list is
  replaced), so the documented `CEPH_SYSTEM_POOL` patch has no effect on
  the server. `ceph.system_pool` was set in the overlay's `server.yaml`;
  the push worker and the CronJob take the env patch. To fix in the
  runbook.
- **Not part of the cutover:** Ceph reports two large omap objects in
  `shardpost-index`, the layout 3 dictionary objects of the largest
  account (36 000 messages): about 320 000 keys each at the cap of 32
  dictionary objects (`fts.shards_max`). To be addressed next.
