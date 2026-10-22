# System pool quarantine cleanup on croessner

Date: 2026-10-22

## Change

The quarantine of the system pool migration of 2026-10-02 (run
`00720635caff`, [cutover report](2026-10-02-system-pool-cutover.md)) was
deleted with `shardpost system cleanup --run 00720635caff --confirm
00720635caff`, 20 days after the migration and with the owner's
confirmation. The fence and the twelve poisoned index heads stay for
good.

Every step ran as `hack/system-job.yaml` with the 0.52.1 image (digest
`sha256:7e83ca35…`) and only the `args` changed. Servers, push workers
and the scrub CronJob kept running: unlike `migrate`, `cleanup` needs no
window.

## Before

- `system status --deep` (exit 0): fence `done`, mark `active`, phase
  M7, 238 objects in the run. Every running process (two servers, two
  push workers) was registered with 0.52.1 and `shardpost-system (id 23)`.
- Objects per namespace:

  | Pool | Namespace | Objects |
  |---|---|---|
  | `shardpost-index` | `~accounts` | 4 |
  | `shardpost-index` | `~accounts.moved/00720635caff` | 57 |
  | `shardpost-index` | `~admin` | 8 |
  | `shardpost-index` | `~admin.moved/00720635caff` | 177 |
  | `shardpost-index` | `~learning` | 1 |
  | `shardpost-index` | `~learning.moved/00720635caff` | 4 |
  | `shardpost-system` | `~accounts` | 89 |
  | `shardpost-system` | `~admin` | 160 |
  | `shardpost-system` | `~learning` | 7 |

  The 13 objects at the old place are the fence and the twelve poisoned
  heads. The quarantine held 238 objects.
- `system verify --source-only` (exit 0): no surplus objects at the old
  place.
- Ceph reported `HEALTH_WARN` with `BLUESTORE_SLOW_OP_ALERT` (6 OSDs).
  This alert stays for a while after slow BlueStore operations and does
  not mean lost data. The cleanup deletes a few hundred small objects
  (about 90 KB), so it went ahead anyway.

## Run (UTC)

| Time | Step |
|---|---|
| 10:37:02 | `system status --deep`: exit 0 |
| 10:37:22 | `system verify --source-only`: exit 0 |
| 10:37:55–10:37:57 | `system cleanup`: exit 0, 238 objects deleted, `done: true` |

## Checks after the cleanup

- `system verify --source-only`: exit 0. From now on it expects no
  quarantine.
- `system status --deep`: fence `done` with `cleaned_at
  2026-10-22T10:37:57Z`, mark `active`. No `.moved/` namespace remains.
  The old place holds the 13 fence and head objects. The system pool
  is unchanged (89 + 160 + 7 objects).
- `hack/kvqueue-smoke.sh rbdpoc2@exampleserver.de`: passed (job
  `20261022T103851Z-51a71039af41`).
- `hack/auth-smoke.sh`: passed.
- No warning or error in the logs of the servers and push workers.

## Open

Nothing for the system pool. Two findings from the cutover report are
still open: `migrate --check` counts the worker locks of running servers
as blocking, and the runbook's `CEPH_SYSTEM_POOL` patch does not reach
the server.
