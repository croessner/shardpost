# Observability

## Signal sources

The service exposes Prometheus metrics at `/metrics` on `HTTP_ADDR`
(default `:8080`). `/healthz` checks the HTTP process; `/readyz`
additionally checks the store and takes the drain state into account. A
successful scrape alone is therefore no proof of working mail delivery.

OpenTelemetry exports traces as soon as `OTEL_EXPORTER_OTLP_ENDPOINT` is
set. `OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf` selects HTTP; otherwise the
service uses gRPC. `telemetry.service_name` and `OTEL_SERVICE_NAME`
determine `service.name` (default `shardpost`, since 0.13.0 in the
production profile as well; up to 0.12.x `imapcephpoc`). The collector
redaction does not depend on the service name; `service.instance.id` is the
host name. The sampler is ParentBased with a TraceIDRatioBased root
sampler: `OTEL_TRACES_SAMPLER_ARG=0.1` captures roughly ten percent of the
root traces. Without an argument the ratio is 1. A different
`OTEL_TRACES_SAMPLER` value does not change this selection, which is fixed in code.

## Reading the metrics correctly

| Metric | Meaning and limitation |
| --- | --- |
| `imapceph_connections{proto}` | Active IMAP, POP3 and ManageSieve (`proto="managesieve"`) connections; not a complete connection count for all protocols. |
| `imapceph_command_seconds` | Histogram of the instrumented IMAP, POP3 and ManageSieve commands. `_count` is suitable for the command rate; `_bucket` for quantiles. |
| `imapceph_commands_total` | Protocol-specific events. Not a universal request counter. Evaluate IMAP `ok/no/bad` separately from auth, connection and poll events. |
| `imapceph_lmtp_deliveries_total{outcome}` | Successful and failed instrumented delivery attempts. |
| `imapceph_lmtp_delivered_bytes_total` | Bytes of successful LMTP deliveries. |
| `imapceph_rados_op_seconds{op}` | Latencies of selected operations: `header_read`, `header_cas`, `list_entries`, `get_entries` (mailbox headers and entries), `omap_read` and `omap_cas` (the versioned omap reads and the compare-and-swap writes of the index objects), `index_page` and `index_write` (the sorted indexes and queues). Not a complete RADOS I/O profile. |
| `imapceph_rados_inflight_ops` | The operations of `imapceph_rados_op_seconds` this process has started and not finished. Against the objecter throttles (`ceph.client_options`: `objecter_inflight_ops`, `objecter_inflight_op_bytes`) a high value shows a pod that waits in its own client, which Ceph does not see. |
| `imapceph_system_placement{state}` | 1 for the placement of the store's own namespaces the last check found, 0 for the others: `none` (index pool), `native` (system pool, initialized there), `migrated` (system pool, migrated there), `unknown` (the last check could not read the fence or the mark; the process goes on). Servers and the push worker check every minute; see [Storage: placement](architecture/storage.md#system-pool-and-placement). |
| `imapceph_rados_cas_retries_total{kind,class}` | Retries after compare-and-swap conflicts. `kind` is the compare-and-swap: `omap` (the omap compare-and-swap of the account's index objects; each lost round is followed by a random wait of 1–10 ms times the rounds lost so far), `header` (mailbox header), `modseq_restamp` (flags stamped again after a modseq collision), `refs` (reference count of a message blob), `index` (shard objects of the store-wide sorted indexes, the account directory). `class` is the class of the object, a fixed set: `fts` (posting shards and the other `fts*/` objects, the layout 3 objects `ftx/`), `ftsmap` (document ids), `registry` (mailbox registry), `mbox` (mailbox header and UID shards), `account`, `index` (sorted indexes), `msg` (message blobs), `other`. No account labels. Contention on one account, e.g. parallel deliveries, shows as `class="fts"`, `"registry"` or `"mbox"`; `shardpost bench measure --contention` provokes it. |
| `imapceph_rados_listings_total{site}` | RADOS object listings by call site. A listing visits every placement group of the pool, so its cost grows with the whole pool, not with the namespace listed. Sites: `list_users` (namespace listing of the account backfill, and of `shardpost scrub` only while the account directory has no completed backfill), `scrub_orphans` and `scrub_unknown_index` (once per account and scrub with `scrub.orphan_source` `listing` or `shadow`, none with `intents`; a batch scrub with `skip_idle` skips them for idle accounts), `purge_objects` (twice per purge with `purge.listing: true`, the default; none with `false`), `sweep` (twice per `shardpost scrub --sweep`, more when a pool is listed again), `admin_kv` (with `admin.kv_index: write`: job runner every poll and cleanup on every server, every push partition holder every scan (`event=push_scan_listing`), admin API; with `read`, the default, only listings of a prefix that is not an indexed collection, since the runner and the push workers then read the queues and hint indexes and the cleanup does not run), `admin_kv_backfill` (`shardpost index backfill kv`, once per run), `learning_all` (with `imapsieve.learning.index` `write` or `off`: learning worker every second, admin API learning list; none with `read`), `learning_purge_owner` (per purge, not with `read`), `learning_backfill` (`shardpost index backfill learning`, once per run), `system_init` (the system pool init: four namespaces of each pool, once per init that writes, in `shardpost system init` and the bench), `system_migrate` (the other system tools of `shardpost system`, which list the index pool, the system pool and the quarantine a few times per run; never a server), `bench_inventory` (`shardpost bench measure`, twice per run) and `bench_guard` (`shardpost bench populate`, at most once per unmarked pool, stops at the first object), both in the bench pools only. No account labels. The aim is that routine paths stop listing; a rising rate at a site shows where they still do. |
| `imapceph_queue_ops_total{queue,op,outcome}` | Operations on the due queues (`queue` `jobs/claimable`, `jobs/expiry`, `learning`, `uploads`, `submission` or `fts_due`; `op` `move` — one write that removes and sets entries — or `due` — one read of every shard head; `outcome` `ok` or `error`). No account labels. |
| `imapceph_queue_stale_entries_total{queue,action}` | Queue entries a reader found not matching their record: `rescheduled` (moved to the record's position, e.g. after a crash between a heartbeat's record write and its queue move), `removed` (the record ended or is gone), `skipped` (within the ten-minute grace, its writer may still be on its way), `unparsable` (a key `Due` cannot parse; skipped and counted at every read, logged at debug level). A steady rate of `rescheduled` means writers fail between the two writes. |
| `imapceph_kv_index_errors_total{collection}` | Hint index writes of the admin store that failed after their record was written (`job`, `plan`, `idem`, `jmap/push/acct`, `servers`), and index writes of the learning queue (`learning` — a queue move after the job's record write — and `learning/owner`). The record stands; `shardpost index backfill kv` repairs an admin store entry, and `shardpost index backfill learning` repairs the learning queue: a rising `collection="learning/owner"` means owner entries are missing, so the admin list and the purge in `read` mode miss jobs until that backfill ran. Readers move a queue entry that lags behind its job. Should stay 0. |
| `imap_imapsieve_learning_wakeups_total` | Learning worker passes a queue notify started (an enqueue or a retry; only with `imapsieve.learning.index: read`). The once-a-second poll is not counted. |
| `imap_imapsieve_learning_lock_attempts_total{result}` | Attempts to take the learning worker lease (one object every server locks), by `result`: `taken` or `busy`. A server without the lease tries every `imapsieve.learning.lock_retry_seconds` (±50 %), not every second; the holder does not try again. About `pods / 20` per second in a cluster is expected. |
| `imapceph_job_wakeups_total{cause}` | Looks of a job runner for claimable jobs: `poll` (`jobs.poll_seconds`) or `notify` (a submit or a release at shutdown woke it through the claimable queue; only with `admin.kv_index: read`). |
| `imapceph_rados_notify_seconds` | Registered, but currently without a measuring call. Do not interpret it as a confirmed notify latency. |
| `imapceph_scrub_findings_total{kind}` | Findings in the respective scrubber process. A server scrape is no proof of a successful external scrub run. The batch scrub runs in the servers, so its findings show in their scrapes. |
| `imapceph_scrub_shadow_differences_total{kind}` | Differences between the listing and the intents in scrubs with `scrub.orphan_source: shadow`: `orphan_missed`, `unknown_missed`, `marks_missed` (debris the listing found that no intent names: from before 0.42.0, from an older server during a rolling update, or a write path without an intent), `orphan_extra` (blobs the intents take for orphans that the listing does not; must stay 0). See [operations](operations/README.md#scrub-without-listings). |
| `imapceph_scrub_purge_unnamed_objects_total` | Objects a purge with `purge.listing: true` found by listing that the account's data model does not name: debris from before 0.42.0 that a purge without listing would leave for the sweep. |
| `imapceph_batch_accounts_total{kind,outcome}` | Accounts a batch over all accounts handled, by the kind it runs (`index_check`, `scrub`, `fts_rebuild`, `attachment_backfill`, `fts_layout`) and outcome: `done`, `skipped` (idle, scrub with `skip_idle`), `gone` (purged meanwhile), `deferred` (locked, tried again at the end) and `failed`. Counted per page, in the server that ran it; a page a takeover repeats counts twice. The share of `skipped` in a nightly scrub is the effect of skip-if-idle. No account labels. |
| `imapceph_fts_frozen_waits_total{result}` | Waits of an indexing or a removal for a conversion that froze the account's full-text index ([layout 3](architecture/storage.md#full-text-index-layout-3)), by how they ended: `moved` (the conversion switched generations; the operation went on in the new one), `thawed` (the conversion went away without a switch) and `timeout` (after `fts.frozen_wait_seconds`, or at once with a wait of 0: the delivery answered a temporary failure). A `timeout` outside a conversion means an unfinished conversion whose run ended: see `shardpostctl fts layout show`. |
| `imapceph_fts_stale_freeze_lifted_total` | Freezes a writer found without a conversion naming them (the run cleared its record but not the freeze) and lifted in its own write. |
| `imapceph_fts_layout_retries_total{reason}` | Restarts of an indexing, a removal or a search in another index generation: `generation` (the layout record named another generation after an anomaly), `frozen` (after a wait for a frozen object). A burst while conversions run is expected. |
| `imapceph_fts_shard_keys{kind}` | Keys of the largest object of the last conversion this server ran, by `kind` (`postings`, `dictionary`, `headers`); against `fts.shard_target_keys` it shows how full the objects are. |
| `imapceph_fts_layout_configured` | `fts.layout` of this server (2 or 3; the default is 3 since 0.46.0). |
| `imapceph_fts_layout_effective` | The layout new full-text indexes of this server get: 3 only with `fts.layout` 3 and the layout 3 cluster mark set, as of the last read of the mark (at startup and whenever an index is created); 0 while a server with `fts.layout` 3 has not read the mark yet (the startup read failed and no index was created since). A server with the default and no mark shows `configured` 3 and `effective` 2. The startup log line `full-text layout of new indexes` (`event=fts_layout`) says the same and is repeated when a server sees the mark change. |
| `imapceph_fts_reshard_due_total` | Accounts a writer queued for a reshard (`ix.fts_due`) because a layout 3 object outgrew 1.5 × its target (`fts.shard_target_keys`, for the dictionary `fts.dict_target_keys`) below its cap; each server queues a generation once per ten minutes. |
| `imapceph_fts_layout_runs_total{mode,result}` | Conversions and reshards of the full-text index (`fts_layout` jobs, the import reshard, the bench), by `mode` (`convert`: between layouts, `reshard`: layout 3 to layout 3, `noop`: nothing to do) and `result` (`ok`, `error`). |
| `imapceph_fts_shard_missing_total` | Layout 3 objects an operation or the scrub found missing (or of another generation) in the generation the layout record names. Searches and deliveries of the account fail until `fts_rebuild`; should stay 0. |
| `imapceph_fts_shards_capped_total{kind}` | Index generations whose objects of a `kind` (`postings`, `dictionary`, `headers`) outgrew 1.5 × their target while their count is at its cap (`fts.shards_max`; for the dictionary `fts.dict_shards_max`): counted by the writer that sees it, once per generation, kind and server (again after a restart). The trigger is silent at the cap, so the objects grow beyond the target from there on, and Ceph warns from 200 000 keys per object. Before 0.49.0 the counter had no label and counted planned reshards. |
| `imapceph_fts_convert_buffer_keys` | Keys the copy phases (P5) of the full-text conversions running on this server hold before they write them, summed over the conversions; each holds at most max(`fts.convert_buffer_keys`, 1024 × target objects); 0 outside a conversion. |
| `imapceph_fts_dict_keys_written_total{entry}` | Substring dictionary keys added by the writers and the conversions of this server, by entry: `trigram` (one per trigram of a term) and `prefix` (the one start entry of a body identifier in an index generation with the identifier filter, `fts.dict_filter: ids`, since 0.50.0). Removals are not counted. The share of `prefix` shows how much the filter saves; without the filter it stays 0. |
| `imapceph_fts_version_drift_total` | Full-text version markers the scrub found that do not fit their index generation (`fts_version_drift`, since 0.50.0): a `3` that a version before 0.50.0 left after converting a filtered generation (set to `2`), a `2` that such a version wrote on an empty filtered or `nosig` generation (set to `3` or `4`), or a `4` whose layout record lost its text preparation (reported only). |
| `imapceph_fts_build_runs_total{mode,result}` | Online runs of the full-text index (`fts_layout`, `fts_rebuild` without the account lock, since 0.51.0, [storage](architecture/storage.md#online-runs-of-the-full-text-index)), by `mode` (`copy`, `reindex`) and `result`: `ok`, `noop`, `busy` (another run alive, or the account locked: the worker backs off), `deferred` (outside the reshard window), `unsupported` (no layout 3 index), `mark_off` (the mark of online runs went after the plan: no change, no failed job), `lost` (another run took over), `error`. Alert on `error`: a run that did not switch (also after three holds without converging). |
| `imapceph_fts_build_phase_seconds{phase}` | Duration of the phases of an online run: `O2` (record), `O3` (freeze), `O5` (snapshot and copy or reindex), `O6` (catch-up and switch), `O8` (clean-up). |
| `imapceph_fts_build_registered_total{via}` | Documents writers recorded for an online run: `alloc` in the id allocation (the normal way), `crossing` after they met the run's freeze (an indexing that began before the run; rare). |
| `imapceph_fts_build_catchup_rounds` | Catch-up rounds of an online run until it switched (histogram). |
| `imapceph_fts_build_hold_seconds` | Holds of new id allocations before an online run's switch (histogram); only accounts with a steady stream of deliveries need one. Alert when it grows: the deliveries of those accounts wait up to `fts.build_hold_ms`. |
| `imapceph_fts_soft_freeze_pass_total{op}` | Operations (`index`, `remove`) that wrote through the freeze of an online run: one per operation that met it. |
| `imapceph_fts_build_window_deferred_total` | Runs the `fts_due` worker, a batch or the run itself put off to the next reshard window. |
| `imapceph_fts_build_window_too_small_total` | Runs not started because their estimate is longer than the whole reshard window (`window_too_small`): plan the account on its own or widen `fts.reshard_window`. |
| `imapceph_imap_watches{object}` | RADOS watches the IMAP watch hub of the pod holds, by `object` (`mailbox`: IDLE, `account`: NOTIFY). One per object however many sessions follow it, so it grows with the mailboxes and accounts in use, not with the sessions. |
| `imapceph_imap_watch_subscribers{object}` | Sessions following a watch of the hub; against `imapceph_imap_watches` it shows how much the hub shares. |
| `imapceph_imap_watch_rearms_total` | Watches the hub set up again after an error (the OSD dropped it, a key rotation). A burst after a rotation is expected; a steady rate is not. |
| `imapceph_imap_watch_refused_total` | Watch subscriptions refused because the connection was at its bound: `server.imap_max_watches` for IDLE, one less for NOTIFY (one stays free for IDLE). A refused NOTIFY mailbox watch makes `NOTIFY SET` answer `NO [LIMIT]` (the earlier NOTIFY state stays); a refused follow of a selected mailbox is covered by the 60 s look; IDLE is never refused. |
| `imapceph_auth_cache_total{op,result}` | Answers of the auth cache, by `op` (`authenticate`: logins of every protocol; `lookup`: LMTP recipients, master-user targets, JMAP identities, admin lookups, the push worker) and `result`: `hit` (a cached entry, positive or negative), `miss` (this caller asked Nauthilus), `shared` (this caller waited for an identical call another caller made) and `bypass` (an admin lookup that went past a cached "unknown"). `hit / (hit + miss + shared)` is the share of logins that did not reach Nauthilus on their own; `miss` is the load on Nauthilus. Absent when the cache is off (`auth.cache.ttl: 0`, static backend). |
| `imapceph_auth_cache_entries{op}` | Entries the cache holds, by operation; bounded by `auth.cache.max_entries` each. Expired entries are dropped when they are next asked for and on the flush log's poll (every 60 s). |
| `imapceph_auth_cache_flushes_total{scope,source}` | Flushes, by `scope` (`account`, `all`) and `source`: `local` (this server's admin API), `remote` (an entry of the flush log another server wrote, or this server's own when another came between) and `resync` (everything, after a lost watch, a failed first read of the flush log or a gap in it). A steady `resync` rate means the watch on `auth-flush` keeps failing. |
| `imapceph_auth_backend_seconds{op}` | Latency of the Nauthilus calls behind the cache (one per miss, however many callers shared it; a bearer login is up to three requests in one observation). |
| `imapceph_jmap_delayed_submissions_total{outcome}` | Delayed JMAP submissions ([JMAP](jmap.md#delayed-sending)), by `outcome`: `scheduled` (a client created one), `sent` (the relay took it), `canceled` (a client or the admin API canceled it), `failed` (refused by the relay, its copy gone, or given up after `retry_attempts`), `unknown` (the sending was interrupted — the conversation broke while the message was transferred, or the server stopped while sending and the sending timeout passed — and it is not sent again), `retried` (a temporary failure before the message was transferred; tried again after a backoff) and `conflict` (the worker found the record changed after sending: the sending timeout was shorter than the conversation, or the account was purged; its outcome is not recorded). Counted in the server that did it. `unknown` and `conflict` should stay near 0; a rising `unknown` after restarts is the at-most-once rule at work. No account labels. |
| `imapceph_jmap_submission_queue_entries{state}` | Entries of the submission queue (`all`) and of those the ones due now (`due`), counted at most once per `jmap.submission.delayed.poll_seconds` by the server holding worker partition 0, up to 100 000; absent in servers that do not hold partition 0. A `due` that stays above 0 means no worker sends (no relay, `worker: false` everywhere, a worker that cannot take its lease). `unowned` is set by every worker each round: the entries overdue by more than a poll in partitions this server does not hold. Their holder may just be busy; persistently above 0 means probably no server holds them (servers × `max_partitions` below `partitions`). |
| `imapceph_jmap_push_partitions_held` | Push partitions this push pod holds (0.52.0, [JMAP: Push partitions](jmap.md#push-partitions)). The sum over the fresh push pods should be 64. |
| `imapceph_jmap_push_partitions_unowned` | Partitions no fresh push pod holds, as this pod last read the membership; every pod reports it, alert on the maximum above 0 for two minutes. Above 0 for long means the pods have no capacity left (`jmap.push.max_watched_accounts` or `max_partitions` too small for the pod count) or disagree about the membership (clocks, see the NTP prerequisite). |
| `imapceph_jmap_push_partition_events_total{event}` | What happened to this pod's partitions: `taken`, `taken_orphan` (taken over the share: orphaned or contested), `released` (given up at shutdown), `shed` (given up to even the load), `lost` (the lease's renewal failed), `expired` (the holder stopped the partition itself before the TTL was up, without a successful renewal, and released it), `rotated` (moved to the new connection after a key rotation), `rotate_failed`, `members_stale` (a cadence without a fresh membership: nothing taken or shed). `lost` rising for long means a lock object or an OSD in trouble. |
| `imapceph_jmap_push_handover_seconds` | Histogram: from taking a partition's lock to the end of the first pass of every watcher of it. p99 above 10 s: raise `jmap.push.start_rate` or add pods. |
| `imapceph_jmap_push_gap_seconds` | Histogram: from the orderly release of a partition by its previous holder (the time in its membership record) to the take by this pod. Orderly hand-overs only; p99 above 10 s means the control notifies or the membership are slow. |
| `imapceph_jmap_push_watched_accounts` | Accounts with push subscriptions this pod watches (counted once their state is read). Against `go_memstats_heap_inuse_bytes` it gives the memory per account, which sizes `jmap.push.max_watched_accounts`. |
| `imapceph_jmap_push_sent_total{outcome}` | Web Push requests by outcome: `delivered`, `gone` (the subscription is destroyed), `slow` (the push service asked to wait; tried again after the backoff), `refused`. |
| `imapceph_jmap_push_delay_seconds` | Histogram: from the watch event to the push it led to; shows the coalescing (one second) plus the load. |
| `imapceph_jmap_push_pending` | Subscriptions with a backlog (an acknowledgement behind the account state), as counted at the last save of each watched account. Not 0 while push services ask to wait; rising for long means a push service down. |
| `imapceph_jmap_push_scan_gets_total{cause}` | Subscription records a scan read, by cause: `changed` (the checksum in the index value moved; about the rate of subscription changes), `nosum` (an index value without checksum, written by a front-end before 0.52.0; should fall to 0 after the rollout), `full` (the full pass of `jmap.push.full_scan_seconds`). |
| `imapceph_jmap_push_verifyq_age_seconds` | Age of the oldest verification waiting, set by the pod that sends them (the holder of the smallest held partition). Above 60 s: nobody holds a partition, or the push services do not answer. |
| `imapceph_jmap_push_lock_op_seconds{op}` | Histogram of the lock operations of the partition leases: `take`, `renew`, `release`. A tail of seconds shows a lock object in peering; the loop never waits on it, the partition alone does. |
| `imapceph_jmap_limit_total{limit,outcome}` | JMAP requests that met a full limit, by `limit` (`requests`, `uploads`, `downloads`, `event_sources`, `per_ip`) and `outcome`: `waited` (got a slot within `jmap.limit_wait`) and `rejected` (answered 429, or `serverUnavailable` for a `Blob/upload` call). A rising `rejected` means a client sends more at once than its account may run and queue. |

In `commands_total`, POP3 counts only selected events; there `DELE` counts
deleted messages. ManageSieve counts only the authentication outcomes
(`command="AUTHENTICATE"`, `outcome` `ok`, `failed` or `tempfail`). With IMAP, auth events can be counted in addition to the
protocol response. Using the sum of all outcomes as the denominator of an
error rate would mix different events. In the current histogram, IMAP `UID`
commands are recorded together under `UID`.

Missing series are not zero values. In particular, many labeled series only
come into existence with the first event. For quantiles, the histogram
buckets have to be summed across the selected pods before the quantile is
computed; per-pod quantiles must not be averaged. Without observations,
quantiles are not meaningful.

## Traces and access

One trace is started per unit of work: an IMAP, POP3 or ManageSieve
command (`imap.FETCH`, `pop3.RETR`, `managesieve.PUTSCRIPT`; an unknown
command is `managesieve.UNKNOWN`), a delivery (`lmtp.deliver`,
`delivery.deliver`), an admin request (`admin.<operation>`, the
operation label carries no account name), a JMAP request and a
maintenance job (`job.<kind>` with `job.id`, and `job.account` when
personal attributes are on). The store reports its own work below them,
so a trace shows where the time of a command actually goes:

```
imap.FETCH
├─ store.get_entries    rados.oid, number of UIDs
└─ store.blob_read      guid, bytes
```

A JMAP request is one span per HTTP request — `jmap.session`,
`jmap.api`, `jmap.upload`, `jmap.download`, with `http.method`,
`http.status_code`, `net.peer` and, once authenticated, `jmap.user` — and
below `jmap.api` one span per method call, named after the method
(`jmap.Email/query`) with `jmap.method`, `jmap.account` (the account the
call works on: the user's own, another owner's or `~public`) and, when
the call failed, `jmap.error` (the JMAP error type). Only a server-side
failure (`serverFail`, `serverUnavailable`, `serverPartialFail`, or an
HTTP status of 500 and above) marks a span as an error; a client's
mistake is an attribute. The store views of the request — the user's own
and every shared namespace a call opens — hang their operations below
the call that runs them:

```
jmap.api
├─ jmap.Email/query      jmap.account
│  ├─ store.fts_search
│  └─ store.get_entries
└─ jmap.Email/get
   └─ store.blob_read
```

The event source (`/jmap/eventsource/`) is not traced: it stays open for
hours, and a span that long says nothing. Neither are the health
endpoint and a call of an unknown method. No JMAP attribute carries a
credential, a filter value, a mailbox name or anything of a message.

Store spans: `store.deliver`, `store.blob_read`, `store.blob_write`,
`store.fts_index`, `store.fts_search`, `store.fts_dict` (the dictionary
keys of new or vanished terms, with their number), `store.header_read`,
`store.header_cas`, `store.list_entries`, `store.get_entries`,
`store.flags`, `store.copy`, `store.move`, `store.expunge`, and
`rados.omap_cas` for the central compare-and-swap loop (the one behind
most omap writes; `store.header_cas` is the mailbox header in
particular). The names match the labels of the RADOS metrics where both
exist. A failed operation carries the error and an error status on its
span.

The protocol layers hand the context of the running command to the store
view (`model.RequestScoped`), including the views of other users' shared
mailboxes. The store only ever creates **child** spans of a sampled
command: work with no command above it — a background notifier, a scrub
walking a whole namespace — produces no spans at all, so a single job
cannot turn into millions of traces. A store that does not implement the
interface produces no store spans either.

One command or job contributes at most 1000 store spans. A job that
walks a namespace would otherwise put millions of children into a single
trace, which the batch processor drops and the backend refuses; when the
limit is reached, the root span carries `store.spans_truncated=true` and
the rest of the work is not traced. The job's own progress and
checkpoints remain the way to follow a long run.

Store spans deliberately carry no mailbox names, only object ids, counts
and sizes; mailbox names are user content. Personal attributes — the
account (`imap.user`, `pop3.user`, `managesieve.user`, `lmtp.user`,
`delivery.account`, `jmap.user`, `jmap.account`, `job.account`), the
admin client (`admin.principal`), the IMAP mailbox (`imap.mailbox`), the
client address (`net.peer`, IMAP and JMAP) and the text of an error,
which can echo client input or mailbox names — are only added with
`telemetry.user_attributes: true` (env `TELEMETRY_USER_ATTRIBUTES`,
default off). Off, a failed span records the error's type (`error.type`)
instead of its text, a trace holds no personal data and the collector
filter accepted on 2026-09-15 (below) is a second line of defence, not
the only one; on, the collector configuration and the access rights have
to implement the applicable rules for this data. ManageSieve spans carry
the command name and, if switched on, the account, never arguments:
`AUTHENTICATE` carries credentials, `PUTSCRIPT` and `CHECKSCRIPT` the
script. Credentials belong neither in dashboards nor in export files or
operational reports.

## Operational responsibility and acceptance

Enabling this in production belongs in the authoritative manifests of the
Kubernetes owner repository. Scrape annotations in this source project
alone are no proof of collection by a Prometheus server. The metrics owner
is responsible for the connection to `metrics.roessner-net.de`, for the data
sources and for the operator dashboard in the Grafana folder **Mail**.

A complete acceptance comprises ready pods, current Prometheus series for
every expected target, an actual service trace in Tempo as well as the
stored and rendered dashboard. A read-only IMAP command can verify the
trace path without a test delivery; with sampling, several commands may be
necessary. Missing live evidence explicitly stays open.

## Production acceptance on 2026-09-15

- Namespace `imap`, deployment `imap-server`: two ready pods; both central
  Prometheus targets `up=1` after the final rollout.
- Scrape labels: `job="kubernetes-pods"`, `namespace="imap"`,
  `app_kubernetes_io_name="imap-server"`, `pod`.
- OTLP: `http://192.168.0.102:4318`, `http/protobuf`, sampling `0.1`.
- [Mail Operator in Grafana](https://metrics.roessner-net.de/d/imapcephpoc-mail-operator/imapcephpoc-c2b7-mail-operator):
  version 2, 21 panels including section headings and interpretation;
  API readback, rendered rate/latency/LMTP panels and logs/Tempo links verified.
- A fresh, real trace after the final activation (13:58:38 UTC,
  `5e9e1e204e89112703b8ea9f284236f2`, `imap.STARTTLS`) was verified
  independently in Tempo by the Kubernetes owner: none of the five
  user/mailbox/peer attributes named above. The preceding synthetic filter test
  was confirmed by both owners.
- CPU/RAM series are currently not present in the central Prometheus.
  The dashboard shows the gap. Missing POP3/error series are not
  artificially set to zero. Rate queries use at least four minutes;
  this window was verified against existing series.

During the first filter rollout, the activation replaced the host file
atomically but did not force a new collector container. The still healthy
collector therefore kept exporting unfiltered traces. The service export was
rolled back, the collector was deliberately recreated, and the effect of the
filter was verified synthetically before the activation was explicitly
approved again. Older traces that are already stored are not cleaned up
retroactively by this.

Reproducible dashboard source in the metrics owner repository (since 0.13.0):
`grafana/tools/build-shardpost-operator-dashboard.py` and
`grafana/dashboards/shardpost-operator.dashboard.json`, dashboard UID
`shardpost-mail-operator`
([Grafana](https://metrics.roessner-net.de/d/shardpost-mail-operator/shardpost-c2b7-mail-operator),
since 2026-09-18; the old `imapcephpoc-mail-operator` has been backed up and removed). The queries cover the old and the new labels
(`namespace` `imap|shardpost`, `service.name` `imapcephpoc|shardpost`).
Kubernetes owner contract: `shardpost/telemetry-patch.yaml` and
`reports/2026-09-15-imapcephpoc-observability.md`.


## Structured session logs from 0.11.3

`slog` writes JSON to stdout; Kubernetes uses `LOG_LEVEL=info`.
`connection_open`, `auth_success`/`auth_failed`, `tls_established` and
`connection_close` make the connection lifecycle visible. `session_id`
ties the events together; `remote` is the effective PROXY client address.
Director health checks produce connection logs as well. Logs are not
filtered by the OTLP sampling rate.

Debug adds the known command names and durations, without arguments or
mail contents. For troubleshooting with a client, 0.18.0 introduced the
time-limited protocol trace per account (`event=protocol_trace`, see
[Admin API](admin-api.md#protocol-trace-for-an-account)). Details, limitations and a Loki example are in the
[configuration documentation](configuration.md), the dated evidence in the
[logging report](reports/2026-09-16-logging.md).
