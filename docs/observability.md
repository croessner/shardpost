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
| `imapceph_rados_op_seconds{op}` | Latencies of selected header/index operations: `header_read`, `header_cas`, `list_entries`, `get_entries`. Not a complete RADOS I/O profile. |
| `imapceph_rados_cas_retries_total{kind}` | Retries after compare-and-swap conflicts; `kind="index"` for the shard objects of the store-wide sorted indexes (account directory). |
| `imapceph_rados_listings_total{site}` | RADOS object listings by call site. A listing visits every placement group of the pool, so its cost grows with the whole pool, not with the namespace listed. Sites: `list_users` (namespace listing of the account backfill, and of `shardpost scrub` only while the account directory has no completed backfill), `scrub_orphans` and `scrub_unknown_index` (once per account and scrub with `scrub.orphan_source` `listing` or `shadow`, none with `intents`; a batch scrub with `skip_idle` skips them for idle accounts), `purge_objects` (twice per purge with `purge.listing: true`, the default; none with `false`), `sweep` (twice per `shardpost scrub --sweep`, more when a pool is listed again), `admin_kv` (with `admin.kv_index: write`: job runner every poll and cleanup on every server, push worker, admin API; with `read`, the default, only listings of a prefix that is not an indexed collection, since the runner and the push worker then read the queues and hint indexes and the cleanup does not run), `admin_kv_backfill` (`shardpost index backfill kv`, once per run), `learning_all` (with `imapsieve.learning.index` `write` or `off`: learning worker every second, admin API learning list; none with `read`), `learning_purge_owner` (per purge, not with `read`), `learning_backfill` (`shardpost index backfill learning`, once per run), `bench_inventory` (`shardpost bench measure`, twice per run) and `bench_guard` (`shardpost bench populate`, at most once per unmarked pool, stops at the first object), both in the bench pools only. No account labels. The aim is that routine paths stop listing; a rising rate at a site shows where they still do. |
| `imapceph_queue_ops_total{queue,op,outcome}` | Operations on the due queues (`queue` `jobs/claimable`, `jobs/expiry` or `learning`; `op` `move` — one write that removes and sets entries — or `due` — one read of every shard head; `outcome` `ok` or `error`). No account labels. |
| `imapceph_queue_stale_entries_total{queue,action}` | Queue entries a reader found not matching their record: `rescheduled` (moved to the record's position, e.g. after a crash between a heartbeat's record write and its queue move), `removed` (the record ended or is gone), `skipped` (within the ten-minute grace, its writer may still be on its way), `unparsable` (a key `Due` cannot parse; skipped and counted at every read, logged at debug level). A steady rate of `rescheduled` means writers fail between the two writes. |
| `imapceph_kv_index_errors_total{collection}` | Hint index writes of the admin store that failed after their record was written (`job`, `plan`, `idem`, `jmap/push/acct`), and index writes of the learning queue (`learning` — a queue move after the job's record write — and `learning/owner`). The record stands; `shardpost index backfill kv` repairs an admin store entry, and `shardpost index backfill learning` repairs the learning queue: a rising `collection="learning/owner"` means owner entries are missing, so the admin list and the purge in `read` mode miss jobs until that backfill ran. Readers move a queue entry that lags behind its job. Should stay 0. |
| `imap_imapsieve_learning_wakeups_total` | Learning worker passes a queue notify started (an enqueue or a retry; only with `imapsieve.learning.index: read`). The once-a-second poll is not counted. |
| `imapceph_job_wakeups_total{cause}` | Looks of a job runner for claimable jobs: `poll` (`jobs.poll_seconds`) or `notify` (a submit or a release at shutdown woke it through the claimable queue; only with `admin.kv_index: read`). |
| `imapceph_rados_notify_seconds` | Registered, but currently without a measuring call. Do not interpret it as a confirmed notify latency. |
| `imapceph_scrub_findings_total{kind}` | Findings in the respective scrubber process. A server scrape is no proof of a successful external scrub run. The batch scrub runs in the servers, so its findings show in their scrapes. |
| `imapceph_scrub_shadow_differences_total{kind}` | Differences between the listing and the intents in scrubs with `scrub.orphan_source: shadow`: `orphan_missed`, `unknown_missed`, `marks_missed` (debris the listing found that no intent names: from before 0.42.0, from an older server during a rolling update, or a write path without an intent), `orphan_extra` (blobs the intents take for orphans that the listing does not; must stay 0). See [operations](operations/README.md#scrub-without-listings). |
| `imapceph_scrub_purge_unnamed_objects_total` | Objects a purge with `purge.listing: true` found by listing that the account's data model does not name: debris from before 0.42.0 that a purge without listing would leave for the sweep. |
| `imapceph_batch_accounts_total{kind,outcome}` | Accounts a batch over all accounts handled, by the kind it runs (`index_check`, `scrub`, `fts_rebuild`, `attachment_backfill`) and outcome: `done`, `skipped` (idle, scrub with `skip_idle`), `gone` (purged meanwhile), `deferred` (locked, tried again at the end) and `failed`. Counted per page, in the server that ran it; a page a takeover repeats counts twice. The share of `skipped` in a nightly scrub is the effect of skip-if-idle. No account labels. |
| `imapceph_imap_watches{object}` | RADOS watches the IMAP watch hub of the pod holds, by `object` (`mailbox`: IDLE, `account`: NOTIFY). One per object however many sessions follow it, so it grows with the mailboxes and accounts in use, not with the sessions. |
| `imapceph_imap_watch_subscribers{object}` | Sessions following a watch of the hub; against `imapceph_imap_watches` it shows how much the hub shares. |
| `imapceph_imap_watch_rearms_total` | Watches the hub set up again after an error (the OSD dropped it, a key rotation). A burst after a rotation is expected; a steady rate is not. |
| `imapceph_imap_watch_refused_total` | Watch subscriptions refused because the connection was at its bound: `server.imap_max_watches` for IDLE, one less for NOTIFY (one stays free for IDLE). A refused NOTIFY mailbox watch makes `NOTIFY SET` answer `NO [LIMIT]` (the earlier NOTIFY state stays); a refused follow of a selected mailbox is covered by the 60 s look; IDLE is never refused. |
| `imapceph_auth_cache_total{op,result}` | Answers of the auth cache, by `op` (`authenticate`: logins of every protocol; `lookup`: LMTP recipients, master-user targets, JMAP identities, admin lookups, the push worker) and `result`: `hit` (a cached entry, positive or negative), `miss` (this caller asked Nauthilus), `shared` (this caller waited for an identical call another caller made) and `bypass` (an admin lookup that went past a cached "unknown"). `hit / (hit + miss + shared)` is the share of logins that did not reach Nauthilus on their own; `miss` is the load on Nauthilus. Absent when the cache is off (`auth.cache.ttl: 0`, static backend). |
| `imapceph_auth_cache_entries{op}` | Entries the cache holds, by operation; bounded by `auth.cache.max_entries` each. Expired entries are dropped when they are next asked for and on the flush log's poll (every 60 s). |
| `imapceph_auth_cache_flushes_total{scope,source}` | Flushes, by `scope` (`account`, `all`) and `source`: `local` (this server's admin API), `remote` (an entry of the flush log another server wrote, or this server's own when another came between) and `resync` (everything, after a lost watch, a failed first read of the flush log or a gap in it). A steady `resync` rate means the watch on `auth-flush` keeps failing. |
| `imapceph_auth_backend_seconds{op}` | Latency of the Nauthilus calls behind the cache (one per miss, however many callers shared it; a bearer login is up to three requests in one observation). |
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
