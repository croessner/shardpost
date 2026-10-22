# Final status of the finish line of 2026-10-03

Date: 2026-10-22

## Scope

On 2026-10-03 the owner set a finish line. It listed the stages still to
do, and after them work was to stop with a final status. Optimisation
stages that were not on the list were ruled out. This report closes that
list. Each item below says what was done, where the evidence is, and
what was left out on purpose.

## The list

| Item | What | Status | Evidence |
|---|---|---|---|
| 1a | Online reshard and rebuild of the full-text index, night window | done 2026-10-03 (0.51.0, mark `fts/online`, window 02:00–05:00 Europe/Berlin) | [0.51.0](2026-10-03-shardpost-0.51.0.md) |
| 1b | `quota_recalc` online, `mailbox_delete` with a mailbox lock | done 2026-10-03 (0.52.0, marks `quota/online`, `mailbox/lock`) | [0.52.0](2026-10-03-shardpost-0.52.0.md) |
| 1 | B14h: identifier dictionary filter (N = 25) and attached-header text preparation | done 2026-10-03 (0.50.0, switched on after 1a, mark `fts/dictfilter`). The batch rebuild of all 33 accounts succeeded in the window of 2026-10-04 | [0.50.0](2026-10-03-shardpost-0.50.0.md), [0.51.0](2026-10-03-shardpost-0.51.0.md#dictionary-filter-switched-on-b14h-2026-10-03), [0.52.0](2026-10-03-shardpost-0.52.0.md#follow-up-2026-10-05-r3-switched-on-croessner) |
| 2 | R3 switch: `scrub.orphan_source: intents`, `purge.listing: false` | done 2026-10-05. Before it, two nightly scrubs had run in shadow mode and found no difference | [0.52.0, follow-up](2026-10-03-shardpost-0.52.0.md#follow-up-2026-10-05-r3-switched-on-croessner) |
| 3 | Quarantine cleanup of the system pool migration (run `00720635caff`) | done 2026-10-22 with the owner's confirmation; 238 objects deleted | [Quarantine cleanup](2026-10-22-system-pool-quarantine-cleanup.md) |
| 4 | JMAP push partitions and job runner pods | done 2026-10-03 (0.52.0). croessner runs two push pods; runner pods are available, but croessner keeps its two combined server pods | [0.52.0](2026-10-03-shardpost-0.52.0.md), [B18 implementation](2026-10-03-b18-push-partitions-runner.md) |
| – | `shardpost-load` help text (`-deliver-rate` vs. `-rate`/`-first`) | done 2026-10-03 (commit `ed72c98`) | git history |

Everything on the list is done. These changes came after the list and
were not on it:

- 2026-10-12: the remaining mailstack accounts moved to ShardPost, and
  the routing default is now `shardpost`
  ([report](2026-10-12-remaining-accounts-cutover.md)).
- 2026-10-20: the 0.52.1 fix for `Email/set` body part types
  ([report](2026-10-20-shardpost-0.52.1.md)).

## State of croessner on 2026-10-22

- Image 0.52.1 runs on the two server pods and the two push pods. Their
  registrations name the system pool `shardpost-system (id 23)`.
- `system verify --source-only` passes. The fence has `cleaned_at` set,
  and no quarantine is left.
- The nightly scrub CronJob (03:17) runs its batch with skip-if-idle
  over all accounts. `shardpostctl job list` shows every batch from
  2026-10-09 to 2026-10-22 as `succeeded`, which is 14 nights in a row
  (the last one is `20261022T031701Z-57f61be25de5`).
- `hack/kvqueue-smoke.sh` and `hack/auth-smoke.sh` passed. The logs of
  the server and push pods show no warning or error.
- Ceph reports `HEALTH_WARN` with `BLUESTORE_SLOW_OP_ALERT` on 6 OSDs.
  This alert stays raised for a while after slow BlueStore operations.
  It does not mean lost data, but it should be watched. If it keeps
  coming back, look at the OSD latencies.

## Not done, on purpose or deferred

These points were open before or were accepted on 2026-10-03. None of
them is a defect in normal operation.

- **Measurements of 0.52.0 that were not run.** These are the push plan
  measurements M1–M7, the `hold_ms`/`switch_attempts` measurement of the
  online recount, its large-account tests, and the end-to-end test "a
  front-end submit runs on a runner". The last one needs runner pods,
  which croessner does not run. The rebalancing after a hard kill and a
  scaling step n → n+1 were also not timed to the end.
- **`shardpost-load` in corpus mode** waits about 5 s between LMTP
  sessions at low rates.
- **Findings from the system pool cutover:**
  - `system migrate --check` counts the worker locks of running servers
    as blocking. As a result the documented pre-check cannot end with
    exit 0 while the cluster serves.
  - The runbook's `CEPH_SYSTEM_POOL` patch does not reach a server whose
    configuration comes from `server.yaml` alone.

  Neither affects a cluster that has already migrated. Both matter for
  the next cluster that migrates.
- **Accepted on 2026-10-03:**
  - Fan-out of the postings for accounts above about 100 000 messages is
    slower, but it works.
  - Stages B14d–f and B15 and block listing are not planned. They become
    relevant only if mail.de shows many such accounts.
- **Mailstack after the 2026-10-12 move:**
  - The mailstack still holds every mailbox, so the way back is a
    re-route.
  - The `ACCOUNT_SHARD_OVERRIDES` entries for `shardpost` are redundant
    now.

## Conclusion

The finish line of 2026-10-03 is reached. No stage on the list is open,
and croessner runs the result. The points above are the candidates for
later work: the two cutover findings matter before a second cluster
migrates, and the 0.52.0 measurements before a load like mail.de's. A
new stage starts only on the owner's decision.
