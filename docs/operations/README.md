# Operations and releases

[Documentation index](../README.md) · [Configuration](../configuration.md)

## Dependencies and responsibility boundaries

The active profile is `deploy/overlays/croessner-nauthilus`; since 0.13.0 the
namespace and the deployment are named `shardpost` and `shardpost-server`. The profile expects Rook
pools and cephx access, cert-manager/TLS, Nauthilus backchannel access,
director backend access and the configured learning and relay services.
None of this is required by the server itself; see
[running without Kubernetes](#running-without-kubernetes).

The secrets are provided by the responsible infrastructure procedures.
The server reads Rook access with restricted RBAC. Renaming the
application changes pools, certificate names and director routes only together
with a data copy; see [naming migration](../decisions/project-name.md).

The base overlay uses static test credentials. `make deploy` selects the
`croessner-nauthilus` operational profile. Even so, verify the intended target
before applying. `make undeploy` also deletes Rook resources
and must not be used as a rollback.

## Read-only first diagnosis

```bash
kubectl --context croessner -n shardpost get pods -l app.kubernetes.io/name=shardpost-server
kubectl --context croessner -n shardpost rollout status deployment/shardpost-server --timeout=30s
kubectl --context croessner -n shardpost logs -l app.kubernetes.io/name=shardpost-server --since=15m --tail=500
kubectl --context croessner -n shardpost get deployment shardpost-server \
  -o jsonpath='{.spec.template.spec.containers[0].image}'
```

Do not dump whole secrets or environments into reports. The client IP,
the user and the mailbox can be personal data in operational logs. When
sharing, use only the events needed and, where appropriate, pseudonymized
correlation data.

| Symptom | Distinguish first |
|---|---|
| No client connection in the log | Level, correct pod/time range, director routing, a new versus an already running session |
| Only the director/pod IP | PROXY emission, trusted CIDRs and the actual access path |
| Authentication failed | `auth_failed` versus a temporary backend/OIDC error |
| Ready, but mail service disrupted | Readiness checks Ceph, not the whole auth/Sieve path |
| IDLE updates late | Watch/notify, journal/MODSEQ synchronization, polling |
| Learning delayed | Prepared/commit receipt, worker lease, budget, retry, dead, external helper |
| Storing fails | Quota, Ceph latency/errors, intents, RADOS and backend metrics |

At `info`, new connections, login, TLS setup and close become
visible. Regular director health checks also produce lifecycle logs.
`debug` adds command names and durations; a rollout is required for level
changes. `remote` shows the effective client, `session_id` ties events together.

## Release procedure

1. Record the starting image including its digest, the configuration, the
   replica count and any open operational anomalies. Secure a rollback path for
   the image and, where applicable, the ConfigMap.
2. Check the changed paths, the matching race/unit tests and the complete Linux
   build. For data model changes, additionally run real Ceph abort tests.
3. Build a new immutable version tag. Run the runtime `config check`.
   The publication target and the rollout must be within the authorized scope.
4. Publish the image and read back the registry digest. Pin the deployment to
   `tag@sha256:...`; carry the release overlay along.
5. Change the image and the intended settings in one controlled update.
   On competing changes, abort and compare again. Do not blindly
   apply a large overlay over an unknown live state.
6. Wait for the rolling update; check Ready, restarts and the actual `imageID`
   of every replica. The scrub CronJob should match the approved server state.
7. Test the affected user path. When the director/auth path is shared, check
   both designated test accounts and the relevant protocols. SMTP
   test deliveries are a side effect of their own.
8. Check logs/metrics and, where applicable, traces, remove temporary
   resources, and document the result and the remaining gaps.

For the logging release, only the image and `LOG_LEVEL` were changed in the pod
spec; the ConfigMap, the pools and the director stayed unchanged. That is an
example of a limited release, not a universal migration procedure.

## Rollback and data compatibility

For a pure code or logging change, the previous digest can be restored with
the saved settings. A blind `rollout undo`
is only suitable when the selected revision really contains the desired
complete state. The CronJob and the declarative overlay are part
of the rollback, so that a later apply does not roll forward again.

For changes to persistent formats, to the program names of stored jobs, to the
chunk size or to the index mapping, an old binary is not a sufficient rollback.
Define a compatibility strategy for old and new writers beforehand.
Do not rename or recreate pools and namespaces when rebranding.

A PROXY rollback requires a sequence: first disable the director's emission and
let its rollout finish, then remove the receiver's trust. Otherwise
the header can reach the server as a protocol command.

## Account directory backfill

Servers with the account directory register an account when they create
its registry (see
[storage](../architecture/storage.md#store-wide-sorted-indexes-and-the-account-directory)).
Accounts that already existed are entered by a backfill, run once the
rollout is complete and no older server is left. **The backfill after the
rollout is mandatory**, not an optimisation: besides the missing entries
it repairs what the mixed fleet left behind. A purge that a new server
checkpointed after phase 4 and an old server then took over and finished
leaves the account `purging` (the old server does not know phase 5), and
a purge run entirely by an old server leaves a stale `active` entry.
Neither heals by itself; the backfill (or `shardpostctl account repair`
per account) does.

```sh
shardpost index backfill accounts             # same image and configuration as the server
shardpost index backfill accounts --restart   # ignore the position of an interrupted run
```

It takes the cls_lock `backfill` on `ix.accounts/lock` in `~accounts`
(a second run at the same time fails with "another account backfill is
running"), lists the namespaces of the index pool once, stats each
registry and writes the missing entries as `active` with the registry's
last change as `created`. Existing entries stay as they are, so it may be
repeated. It then applies the repair rules to every entry (see
[admin API](../admin-api.md#the-account-directory)): entries left
`purging` are removed or activated, `active` entries older than ten
minutes whose registry it did not find are removed as stale. The stat of
each registry uses one index-pool context whose namespace it switches,
not a full account view.
At the end it records a completion marker with the start and end times
and the counts in the directory head (`mark:backfill.accounts`). Every 500
namespaces it records its position; a run that was interrupted continues
from there and keeps the original start time, unless `--restart` is
given. Only a backfill that started after the last server without the
directory stopped makes the directory complete; if in doubt, run it again
with `--restart`.

The backfill lists the pool (it shows as `list_users` in
`imapceph_rados_listings_total`), so it costs as much as a scrub's
namespace listing plus one stat per account. Run it like the scrub: one
at a time, outside peak hours.

A purge that failed or was cancelled can leave an account `purging`.
`shardpostctl account repair <account>` (role `repair`) clears it for one
account; the next backfill does it for all of them. Until then a new
purge of that account fails at once with "run shardpostctl account
repair". `shardpostctl account directory <account>` (role `diagnose`)
shows the entry, whether the registry exists and the backfill marker.

`hack/directory-smoke.sh` checks the directory on a cluster after a
rollout: it creates a throwaway account, checks its entry, purges it and
checks that the entry is gone, runs the backfill and prints its marker.

The completion marker is also what batches over all accounts wait for
(`shardpostctl plan -all`, the nightly batch scrub): until it exists they
are refused, and `shardpost scrub` lists the index pool instead of
reading the directory. `shardpostctl account list` (role `diagnose`,
scope of all accounts) pages through the directory.

**Rollback** is safe: older servers ignore `~accounts`, and an older
purge does not set `purging`.

## Admin store backfill

Servers with `admin.kv_index` `write` or `read` (the default) keep hint
indexes and due queues next to the admin records (jobs, plans,
idempotency keys, JMAP push subscriptions; see
[storage](../architecture/storage.md#admin-store-hint-indexes-and-due-queues)).
Records that older servers wrote are not in them. After the rollout of
the version that writes them, once no older server is left, run the
backfill:

```sh
shardpost index backfill kv             # same image and configuration as the server
shardpost index backfill kv --restart   # ignore the position of an interrupted run
```

It takes the cls_lock `backfill` on `ix.kv/lock` in `~admin` (a second
run at the same time fails with "another admin store backfill is
running"), lists the admin namespace once (it shows as
`admin_kv_backfill` in `imapceph_rados_listings_total`) and, for every
record, writes its hint index entry from the record as it is and the
queue entries the record needs: a job its claimable position (unless it
ended) and its expiry, an idempotency key the expiry of its job (ten
minutes from now when the job was never stored), a plan its expiry.
Every write sets what the record implies, so it may be repeated. Every
500 records it records its position in the head of `ix.job`
(`mark:backfill.kv.progress`); an interrupted run continues from there
and keeps the original start time, unless `--restart` is given. At the
end it records `mark:backfill.kv` with the start and end times and the
counts (records handled by this run, indexed, queued). It refuses to
run with `admin.kv_index: off`. Its lock lives three minutes and is
renewed every minute, so a renewal that waits out the OSD op timeout
still lands in time (the account backfill's lock likewise).

`shardpost index show kv [<key>...]` (read-only, same image and
configuration; `--config` goes before `index`) prints as JSON the index
mode, the backfill marker, the number of entries of both queues (all
and due now) and, for each admin store key given (`job/<id>`,
`plan/<id>`, `idem/<hash>`, `jmap/push/acct/<account>`), whether the
record exists, its hint index entry and its queue entries; a job brings
the idempotency key its record names along. It reads every queue entry:
a diagnosis tool, not a routine.

`hack/kvqueue-smoke.sh <throwaway-account>` checks this on a cluster
after a rollout: it runs the backfill, shows the marker, runs a
read-only `shardpostctl check` with an idempotency key twice (one job),
verifies that the ended job has no claimable entry and that it and its
idempotency key have their expiry entries at `Finished + retention`,
and prints the queue metrics.

**`read` is the default.** `read` is correct on a cluster whose
backfill completed after the last server without the indexes (a version
before 0.38.0, or one with `admin.kv_index: off`) stopped. A server
configured for `read` that finds no completion marker logs a warning at
start ("admin.kv_index=read without a completed …") and behaves as in
`write` mode (listing) until the marker exists; it looks for it every
30 seconds, so the gate keeps a cluster without the backfill on `write`,
and a backfill run after the rollout switches the running servers to
`read` without a restart. The server cannot tell when the last older
server stopped: if in doubt, run the backfill again with `--restart`
once no older server is left. Going back to `write` is always possible.

**New installations** run `shardpost index backfill kv` and
`shardpost index backfill learning` once after the first deployment:
with no records they complete at once and set the completion markers,
so `read` mode is reached (without them the servers stay on `write`
behaviour and log the warning at every start).

**Expiry.** With `write` or `read`, one server (the holder of the lock
`ix.jobs/expiry/lock`) deletes jobs and their idempotency keys
`jobs.retention_days` after they ended, and plans that long after they
expired, every `jobs.expiry_seconds`. In `write` mode the listing
cleanup of earlier versions keeps running beside it on every runner
poll; in `read` mode it does not run, and no running path lists the
admin namespace. Idempotency keys used to stay forever; records from
before the backfill expire once it ran.

**Rollback** is safe: older servers ignore the `ix.*` objects and keep
reading by listing; the records are unchanged. Queue entries of jobs an
older server ended are removed by the readers of this version later.

## Learning queue backfill

Servers with `imapsieve.learning.index` `write` (the default) or `read`
keep a due queue and an owner index of the learning jobs in the
namespace `~learning` of the index pool (see
[storage](../architecture/storage.md#learning-queue-due-queue-and-owner-index)).
In `write` mode the worker, the admin API's learning list and the
account purge still list the namespace `~learning` of the data pool (the
worker every second). Rollout order:

1. Deploy the version (default `write`). The learning worker lock is the
   one older versions take, so only one worker runs during the update.
2. Once no older server is left, run the backfill with the image and
   configuration of the server:

   ```sh
   shardpost index backfill learning             # once after the rollout
   shardpost index backfill learning --restart   # ignore the position of an interrupted run
   ```

   It takes the cls_lock `backfill` on `ix.learning/lock` in `~learning`
   (a second run fails with "another learning queue backfill is
   running"), lists the namespace once (`learning_backfill` in
   `imapceph_rados_listings_total`), reads every job again and writes
   its queue entry (its next attempt, or the dead position) and its
   owner entry. Every write sets what the record implies, so it may be
   repeated; every 500 jobs it records its position
   (`mark:backfill.learning.progress` in `ix.learning/head`), and an
   interrupted run continues from there. At the end it records
   `mark:backfill.learning`. It refuses to run with
   `imapsieve.learning.index: off`.
3. Set `imapsieve.learning.index: read` on every server. A server in
   `read` mode without the marker logs a warning and behaves as in
   `write` mode until it exists.

The backfill is also the repair: an owner or queue entry whose write
failed after its job was written is counted in
`imapceph_kv_index_errors_total{collection="learning/owner"}` (or
`collection="learning"`); run `shardpost index backfill learning`
again when that counter rises. It also removes queue entries whose job is
gone (at the dead position, or due for longer than ten minutes).

`shardpost index show learning [<id>...]` (read-only) prints as JSON
the configured and the effective mode, the backfill marker, the number
of queue entries (all, due now, dead), the number of owner entries and,
for each job id given, the record, its queue entries and whether its
owner entry exists.

`hack/learning-smoke.sh` runs the backfill on a cluster, shows the
marker and the queue sizes and checks the admin API's learning list; it
enqueues nothing (a learning job runs the configured learning program
on a real message).

**Rollback** to `write` is always possible; to an older version as
well: older servers ignore the index objects. A job they rewrite or
delete leaves its queue entry behind, which the readers of this version
move or remove.

## Nightly scrub

From this version the nightly scrub runs as a batch job on the servers
instead of a loop in the CronJob's pod: the CronJob only submits it.

```sh
shardpost scrub --batch      # same image and configuration as the server
```

`shardpost scrub --batch` plans the batch scrub over every account of the
account directory with `skip_idle` and submits it through the admin
store (it needs the Ceph credentials, not the admin socket), then exits;
the job runners of the servers (`jobs.enabled`, the default) run it. It
submits nothing while a batch scrub of an earlier night has not ended
(queued, running or cancel requested), so a scrub that takes longer than
a day is not started twice. With `scrub.dry_run` it submits a batch of
`index_check` instead (no repairs, no skipping). `scrub.users` does not
go with `--batch` (the command fails); use `shardpost scrub` for chosen
accounts. Without a completed backfill of the account directory it falls
back to the scrub run in its own process over a listing of the index
pool, as before, and logs a warning.

The servers run the batch with `scrub.parallel` accounts at once and
`scrub.grace_seconds`; an account whose last batch scrub was complete and
found nothing and that did not change since is skipped, for at most
`scrub.interval_days` (7) (see
[storage](../architecture/storage.md#batches-over-all-accounts-and-the-batch-scrub)).
The first night after the change scrubs every account and records its
state; the nights after scrub only what changed. Follow it with
`shardpostctl job list -state running`, `shardpostctl job wait <id>` and
the metric `imapceph_batch_accounts_total{kind="scrub"}`; the result names
the first 50 accounts that failed or had findings. A batch scrub that
needs to stop is cancelled with `shardpostctl job cancel <id>`; it stops
between two accounts, and the next night starts from the beginning of the
directory (the skipped accounts cost one directory read and, at most, a
few omap reads each).

**The CronJob change to make** (`deploy/base/server/scrub-cronjob.yaml`,
owned by the deployment; this version does not change `deploy/`):

```yaml
          containers:
            - name: scrub
              image: docker.io/chrroessner/shardpost:<this version>
              args: ["scrub", "--batch"]
              env:                      # unchanged: CEPH_*, ROOK_NAMESPACE
                - name: SCRUB_DRY_RUN
                  value: "false"
              resources:
                requests:
                  cpu: 50m
                  memory: 64Mi
                limits:
                  memory: 256Mi
```

`SCRUB_GRACE_SECONDS` in the CronJob no longer matters for the batch (the
servers' `scrub.grace_seconds` applies, 3600 by default); `SCRUB_PARALLEL`
and `SCRUB_INTERVAL_DAYS` belong into the servers' configuration, not
into the CronJob. The pod runs for seconds, so `activeDeadlineSeconds:
600` may be added. Roll the CronJob out only after every server runs this
version, and plan no batch (`shardpostctl plan -all`) before: an older
server that claims a batch job does not know the kind `batch` and fails
it ("unknown job kind"). Rollback: set the
CronJob's `args` back to `["scrub"]`; a batch scrub already submitted is
cancelled with `shardpostctl job cancel`.

`shardpost scrub` without `--batch` still scrubs in its own process:
`scrub.users`, else every account of the directory (once its backfill
completed), else every namespace of an index pool listing.

## Scrub without listings

From 0.42.0 every write that can leave debris records an intent, and the
scrub can find what a crash left from the intents instead of listing the
account's namespace in both pools (see
[storage](../architecture/storage.md#intents-debris-that-a-scrub-finds-without-listing)).
Where it looks is `scrub.orphan_source`; the release ships with
`shadow`: the scrub lists and repairs from the listing as before, but
also works out what the intents name and reports where they differ.
Nothing has to be done for the rollout itself, and servers of different
versions may run side by side (older servers ignore the intents; their
writes leave none, which the listing still covers).

**Before the switch to `intents`**, after every server runs 0.42.0:

1. Let the nightly batch scrub run for a few nights. Watch
   `imapceph_scrub_shadow_differences_total{kind}`:
   `orphan_extra` (blobs the intents take for orphans that the listing
   does not) must stay 0; `orphan_missed`, `unknown_missed` and
   `marks_missed` count debris without an intent — debris from before
   0.42.0, which the listing removes in the same runs (or the sweep, see
   below). They must fall to 0 once that is gone. The scrub report of an
   account names samples (`Shadow.Samples`).
2. `hack/r3-smoke.sh` checks a cluster: it creates and deletes a
   mailbox with messages on the throwaway account and scrubs it in shadow
   mode; the report must show no difference.

**Only when every server runs 0.42.0 or later.** Servers do not report
their versions to each other, so nothing enforces this: an older server
writes no removal intents, and a message it moves or expunges can then
look like an orphan to a scrub without listing, which deletes it.

**The switch** is `scrub.orphan_source: intents` (or
`SCRUB_ORPHAN_SOURCE=intents`) in the servers' configuration (the batch
scrub runs there) and in the scrub CronJob's environment (for a scrub
without `--batch`). From then on `imapceph_rados_listings_total` stays
flat for `scrub_orphans` and `scrub_unknown_index`. Rollback: back to
`shadow` or `listing`; the intents keep being written either way.

`scrub.intent_grace_seconds` (3600) is how old an intent must be before
the scrub takes it for the leftover of a crash; it must exceed the
longest write (a large delivery, an import step), which is far below
the default.

**Debris from before 0.42.0.** With `intents` the scrub no longer finds
what older servers left without an intent (blobs of crashed deliveries
and of JMAP uploads, mailbox objects of crashed creations, the private
`\Seen` marks of deleted public folders). The listing removes it while
the scrub runs in `shadow`; what remains, and the objects of accounts
purged by older versions, is the sweep's work:

```sh
shardpost scrub --sweep             # report only (JSON on stdout)
shardpost scrub --sweep --repair    # delete what is older than scrub.grace_seconds
shardpost scrub --sweep --restart   # drop a sweep that stopped and start over
```

Run it once after every server runs 0.42.0, as a Kubernetes Job with the
scrub CronJob's image, configuration and Ceph credentials, first without
`--repair` (the report names the remnants by class: `blob`, `mailbox`,
`marks`, `other`, `namespace` for objects of a namespace without
registry, with samples), then with it. It lists both pools in every
namespace (the cost of one scrub listing per pool, not per account),
spills the object names into `~accounts` and evaluates them account by
account; a sweep that stops (a restart of the Job, a lost lease) goes on
from its checkpoint when it is run again. `imapceph_rados_listings_total{site="sweep"}`
counts its listings. It needs `mon 'allow r'` (it reads each pool's
`pg_num`), which the server's CephClient has. A namespace without
registry is deleted only when the account directory says it is being
purged or a purge of 0.42.0 or later recorded it (`ix.purged`); any
other one is reported and left alone: `registry_missing` (the directory
holds it as active: a lost registry), `unregistered` (no directory
entry: an account that only kept Sieve scripts before 0.42.0, or what a
purge of an older version left; remove the latter by hand once
checked). A repairing sweep forgets, when it ends, the purges it cleaned
after (`purged_forgotten`). See the
[manual page](../man/man1/shardpost-scrub.1).

**The purge without listing.** `purge.listing` (default `true`) keeps the
purge of earlier versions, which lists the account's namespaces; it also
counts the objects the data model does not name
(`imapceph_scrub_purge_unnamed_objects_total`). Once that stays at 0 for
the purges of accounts the sweep has been through, set
`purge.listing: false` (`PURGE_LISTING=false`) in the servers'
configuration: the purge then deletes what the data model names and
lists nothing (`purge_objects` stays flat). Debris of an account that
was never swept stays behind; the purge records the account, and the
next repairing sweep removes it. Rollback: back to `true`.

## Auth cache

Every server keeps the logins and lookups Nauthilus answered for a
short time (`auth.cache`, see
[configuration](../configuration.md#auth-cache-auth)). A change in LDAP
therefore reaches ShardPost only when the entry ends, unless the cache
is flushed:

```sh
shardpostctl auth flush user@example.org   # one account (role repair)
shardpostctl auth flush                    # everything
```

The flush drops every entry whose login name, lookup name or canonical
account is the given name, compared without regard to case, on the
server that answers at once, and records it in the flush log (the
object `auth-flush` in the admin namespace). The other servers apply it
when they are notified and at the latest on their next poll, 60 seconds
later; the command does not wait for them. It reaches neither the
Director's routing cache (up to 30 s) nor the cache of Nauthilus itself:
a change that has to take effect at once also needs Nauthilus' own flush.
The flush is in the audit log (`flush_account`).

What a change costs until it takes effect:

| Event | Effect in ShardPost | Also |
|---|---|---|
| Password change | The old password keeps working until its entry ends (`ttl`, 30 s); the new one works at once (it is a miss). | Nauthilus' own cache |
| Account locked in LDAP | Takes effect after `ttl`, at once with `auth flush <account>`. | Only after a Nauthilus flush; the Director up to 30 s |
| Token revoked before `exp` | Accepted until `ttl`. | The Director up to 30 s |
| Token expired | At once: the entry ends at `exp`. | — |
| Quota or attribute change (`rnsMSQuota`, display name) | After `ttl` (logins) or `lookup_ttl` (LMTP), at once with `auth flush`. | Nauthilus' own cache |
| Alias or account removed | LMTP keeps accepting until `lookup_ttl` (a cached positive lookup), at once with `auth flush`. | Nauthilus' own cache |
| New account, mail over LMTP | Refused as unknown until `lookup_negative_ttl` (at most 5 s by default), accepted after; the admin API (diagnosis, import, deliveries) goes past the cached "unknown" and stores what it finds. | Nauthilus' LDAP negative cache (20 s) |
| Account moved to another shard | A window of up to `lookup_ttl` plus the Director's 30 s. | Runbook: `auth flush <account>` on the source and the target shard; hold the account before the pin for at least that window. |
| Brute force | Nauthilus sees every distinct failed attempt; identical failed attempts that arrive together count as one. With `negative_ttl` identical repeats within it do not reach Nauthilus at all. | — |

The secrets are only kept as HMACs under a key that lives in the
process; a restart drops every entry. A master-user login is never
cached, only the lookup of its target.

**Emergency switch.** `auth.cache.ttl: 0` (or `AUTH_CACHE_TTL=0`) switches
the whole cache off — logins and lookups — on the next rollout: every
login and lookup asks Nauthilus again, as before the cache existed. It is
the switch for a suspected stale or wrong answer that a flush does not
explain; Nauthilus then carries the full load again, so watch
`imapceph_auth_backend_seconds` and Nauthilus' own in-flight limit. The
cache needs no migration and keeps nothing on disk: a rollback to a
version without it is possible at any time (the flush log object stays
behind, unused).

Follow it with `imapceph_auth_cache_total` (hit rate and the load on
Nauthilus), `imapceph_auth_cache_flushes_total` and, for JMAP clients
that send many requests at once, `imapceph_jmap_limit_total` (see
[observability](../observability.md)). `hack/auth-smoke.sh` checks a
rollout: logins hit the cache, a flush makes the next login a miss,
and parallel JMAP requests get no 429 or 503.

## Scrub, backup and observability

Scrub can change data. For an investigation, first define a dry run and the
scope. It repairs defined inconsistencies but does not reconstruct
lost mail content. Evidence of backup/restore has to cover both pools and
the associated durable state.

Prometheus, Loki and Tempo make different statements. A missing
series is not zero; a sampled trace is not every connection.
The server sets some user/mailbox/peer attributes in spans;
the documented redaction happens in the external collector. It must not be
assumed to be automatically given server-side when the telemetry is
rebuilt. See the [observability contract](../observability.md).

## Running without Kubernetes

ShardPost needs a Ceph cluster, not Kubernetes and not Rook. The server
binary talks to the Kubernetes API in exactly one place: when `ceph.key`
is empty, it fetches the cephx key and the monitor list from Rook
(`internal/rookcreds`). With `ceph.key` set, that path is skipped and the
`rook` section has no effect. Several servers coordinate through RADOS
alone (watch/notify for IDLE and NOTIFY, RADOS locks, job leases in the
pools), so native servers on VMs or hardware can share one pool pair just
like pods do. `shardpostctl` and `shardpost-load` have no Kubernetes
dependency either. Only `contrib/ucm-cert-renew` is Kubernetes-specific.

What is Rook-specific is the deployment under `deploy/`: the pools are
`CephBlockPool` resources, the cephx identity is a `CephClient`, and the
RBAC, the `ROOK_NAMESPACE` variable and the mounted service account token
exist only to read the Rook secret. A native installation replaces them
with the steps below.

### Ceph side

The index pool must be replicated, because the mailbox state lives in
omap. The data pool holds write-once blobs and can be erasure coded
without `allow_ec_overwrites`. The equivalent of `deploy/base/ceph/pools.yaml`:

```bash
ceph osd pool create shardpost-index replicated
ceph osd pool set shardpost-index size 3
ceph osd erasure-code-profile set shardpost-ec21 k=2 m=1 crush-failure-domain=host
ceph osd pool create shardpost-data erasure shardpost-ec21
ceph osd pool application enable shardpost-index shardpost
ceph osd pool application enable shardpost-data shardpost
```

The cephx identity needs the same capabilities as the `CephClient` in
`deploy/base/ceph/cephclient.yaml`; `x` on the OSDs is required for object
classes (`cls_lock` for the POP3 maildrop lock):

```bash
ceph auth get-or-create client.shardpost \
  mon 'allow r' \
  osd 'allow rwx pool=shardpost-index, allow rwx pool=shardpost-data'
```

The printed key is the value for `ceph.key`. Pool names and the user are
configurable (`ceph.index_pool`, `ceph.data_pool`, `ceph.user`).

### Host side

Each server host needs:

- `librados2` in the version of the cluster. The image builds against
  Ceph 20 (Tentacle); `aes256k` cephx keys require a Tentacle client. On
  EL9 the package comes from `centos-release-ceph-tentacle`.
- Network access to the monitors (3300 for msgr2) and to every OSD. The
  server reads and writes objects on the OSDs directly.
- The binaries. Build them on a host with `librados-devel` and `gcc`
  exactly as the `Dockerfile` does (`CGO_ENABLED=1 go build -tags tentacle
  ./cmd/shardpost`; `shardpostctl` and `shardpost-load` build without cgo),
  or copy them out of the release image.
- The external programs under `/usr/local/lib/shardpost/programs` and
  their Python venv, but only when `extprograms` refers to them (learning,
  quota warnings, quota flag). The `Dockerfile` shows the layout.

The server does not read `/etc/ceph/ceph.conf` or a keyring; every
librados option comes from the ShardPost configuration.

### Configuration

The configuration file is passed with `--config` or `SHARDPOST_CONFIG`.
The storage part of a native node:

```yaml
ceph:
  user: shardpost
  mon_host: "[v2:192.0.2.11:3300],[v2:192.0.2.12:3300],[v2:192.0.2.13:3300]"
  index_pool: shardpost-index
  data_pool: shardpost-data
  # key: set through CEPH_KEY, see below
```

`ceph.key` has no `*_file` counterpart. Keep it out of the YAML and pass
it as `CEPH_KEY` from an environment file readable only by root; systemd
reads that file before it drops privileges. Environment values override
the YAML.

Without a key the server tries Rook and fails at startup with
`fetch ceph credentials: service account token: ...`. That message outside
Kubernetes means `CEPH_KEY` did not reach the process.

Check the result before the first start. `config check` validates the
configuration and reads the secret files; it neither connects to Ceph nor
needs the key:

```bash
sudo -u shardpost shardpost --config /etc/shardpost/shardpost.yaml config check
```

### systemd unit

```ini
# /etc/systemd/system/shardpost.service
[Unit]
Description=ShardPost mail server
After=network-online.target
Wants=network-online.target

[Service]
User=shardpost
Group=shardpost
EnvironmentFile=/etc/shardpost/ceph.env
Environment=GOMEMLIMIT=700MiB
ExecStart=/usr/local/bin/shardpost --config /etc/shardpost/shardpost.yaml
# SIGTERM starts the drain: readiness turns false, the listeners close
# and sessions get server.drain_seconds; the shutdown gives up 5 s later.
KillSignal=SIGTERM
TimeoutStopSec=40
Restart=on-failure
RestartSec=5
MemoryMax=1G
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
RuntimeDirectory=shardpost

[Install]
WantedBy=multi-user.target
```

`/etc/shardpost/ceph.env` holds one line, `CEPH_KEY=...`, with mode 0600
and owner root. Keep `TimeoutStopSec` above `server.drain_seconds` plus
5 seconds. The default ports are above 1024; for 143/993 add
`AmbientCapabilities=CAP_NET_BIND_SERVICE` or leave the public ports to
the Director. With `admin.socket` under `/run/shardpost`, `shardpostctl`
must run as the `shardpost` user, since the socket admits only callers
with the server's own uid.

The memory values follow the pod: librados alone holds about 150 MiB,
`server.memory_budget_bytes` comes on top.

### Scrub

The Kubernetes CronJob becomes a timer on one node only; the CronJob runs
with `concurrencyPolicy: Forbid`, and a native setup should not run
several scrubs at once either.

```ini
# /etc/systemd/system/shardpost-scrub.service
[Unit]
Description=ShardPost scrub

[Service]
Type=oneshot
User=shardpost
Group=shardpost
EnvironmentFile=/etc/shardpost/ceph.env
ExecStart=/usr/local/bin/shardpost --config /etc/shardpost/shardpost.yaml scrub --batch
```

```ini
# /etc/systemd/system/shardpost-scrub.timer
[Timer]
OnCalendar=*-*-* 03:17:00

[Install]
WantedBy=timers.target
```

`scrub --batch` submits the batch scrub the servers run (see
[Nightly scrub](#nightly-scrub)); without `--batch` the timer scrubs in
its own process. `scrub.grace_seconds` and `scrub.dry_run` apply as
described above.

### What Kubernetes did and who does it now

| Kubernetes | Native |
|---|---|
| Readiness and liveness probes | The Director or load balancer checks `/readyz` and `/healthz` on `server.http_addr`. `/readyz` checks Ceph, not the auth path. |
| Service and endpoints | The Director lists the hosts as backends; set `server.proxy_protocol_trusted_cidrs` to the Director addresses when it sends PROXY. |
| Rolling update | Stop, replace and start one node at a time; wait for `/readyz` before the next. |
| Rook key rotation without restart | See below. |
| TLS secret from cert-manager | Certificate files on the host; `tls.cert`/`tls.key` point to them. The admin mTLS files are re-read on change, the listener certificate at startup. |
| Prometheus scrape annotation | A static scrape target for `/metrics` on `server.http_addr`. |

### Key rotation

The in-process rotation (`server.key_poll_seconds`) is tied to the Rook
watcher and does not run with a static key. With Rook, a rotated key
needs no restart: new sessions use the new connection, and the
long-lived views (the admin store of the job runner, of JMAP and of the
push worker) are moved to it; requests that run finish on the old one,
and the expiry worker's lock is released and taken again; the rotation
waits for a lock renewal in flight, up to the OSD op timeout (60 s) per
held lock (see
[storage](../architecture/storage.md#admin-store-hint-indexes-and-due-queues)). Changing the key of the
existing identity in place invalidates it for every node at once. The
interruption-free way is a second identity with the same capabilities:
create `client.shardpost2`, switch the nodes one by one (`ceph.user` and
`CEPH_KEY`, then restart), and remove `client.shardpost` once no node
uses it.
