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

Going back behind 0.43.0 needs an empty delayed submission queue (see
[Delayed submissions](#delayed-submissions)). Going back behind 0.45.0
needs every account back on layout 2 of the full-text index (see
[Full-text layout 3](#full-text-layout-3)). Going back behind 0.47.0 is
safe while `ceph.system_pool` is empty and no system init or migration
ran; after one, no image before 0.47.0 may run against the cluster (it
neither knows the fence nor the system pool), and the way back is a
revert of the placement (see [Taking it back](#taking-it-back)).

A PROXY rollback requires a sequence: first disable the director's emission and
let its rollout finish, then remove the receiver's trust. Otherwise
the header can reach the server as a protocol command.

## Push partitions

Since 0.52.0 the JMAP push workers share 64 partitions
([JMAP: Push partitions](../jmap.md#push-partitions),
[configuration](../configuration.md#push-worker-partitions-jmappush)).
Component `deploy/components/jmap-push`: two replicas with a rolling
update (`maxSurge 1`, `maxUnavailable 0`, `minReadySeconds 30`), a
termination grace period of 45 s (an end takes about 25 s, up to 40–45 s
while the membership object hangs), a spread over the nodes and a
disruption budget of one.

**Prerequisite: NTP on every node.** The pods compare the clocks of
each other's membership records with their own: a pod whose clock is
behind by more than `jmap.push.lease_seconds` is invisible to the others
(its partitions count as orphaned; the locks protect them, the others
only try in vain), one whose clock is ahead counts as alive for longer.
Both converge once the clocks agree.

**How many pods.** No HPA: the replicas follow the accounts with push
subscriptions, about `1.3 × accounts / jmap.push.max_watched_accounts`
and at least two (200 000 accounts: six, up to eight). Every scaling
moves partitions; from n to n + 1 pods the new one has its share after
`rebalance_settle_seconds` plus `rebalance_seconds` per partition moved
(one to two pods: about seven minutes; six to seven: about a minute and
a half). Each move costs the accounts of that partition one hand-over
(at most about ten seconds without a push, at most one push repeated).

**The way over from the single pod (0.51.0 and before).** Roll out in
this order: first every front-end, then the push deployment. Every
front-end must run 0.52.0 before the push deployment rolls: a 0.51.0
front-end that is still running writes subscriptions without the
checksum and without the notify, so the partitioned workers keep a stale
checksum for that account until the hourly full pass. The front-ends of
0.52.0 write the checksum into the index values and notify the workers;
while old and new front-ends run, the checksums are not to be trusted
and the workers fall back to the hourly full pass. Apply the
push deployment with `replicas: 1` first: the new pod waits while the
old one holds the lease record (`event=push_legacy_wait`), is ready
nonetheless, Kubernetes ends the old pod, which releases, and within two
seconds the new pod writes the marker and takes the 64 partitions. The
old pod's last stop loses what it had in flight, as every rollout did
before; roll it out in a quiet hour. Check with `shardpostctl server
list` (`PARTITIONS` 64) and `imapceph_jmap_push_partitions_held`, then
set `replicas: 2` and watch the partitions move to 32/32.

**Way back to 0.51.0 (the push deployment only).** With the rolling
update in the manifest the way back is a full rollout: the 0.51.0 pod
starts beside the new ones, waits (the marker), is ready, and the new
pods end one by one; the last one deletes the marker as it ends, and the
old pod takes the lease at its next renewal (≤ 30 s). Set `replicas: 1`
before the image change to end exactly one new pod. After a crash of the
last new pod the old one waits for the marker to expire (≤ 90 s). The
state records stay readable (the acknowledgements and the checksums are
ignored by the old pod). Rolling the front-ends back has no effect on
the workers beyond the checksums.

**A hard end.** A pod killed without its grace period leaves its leases
to expire: the others take its partitions within the lease plus a
cadence (about 45 s at most) and keep them for `orphan_hold_seconds`. A
pod restarted under the same name after a crash waits for its old record
to expire and starts without a right to its old partitions.

**Alerts** (`docs/observability.md`): `imapceph_jmap_push_partitions_unowned`
above 0 for two minutes; the p99 of `imapceph_jmap_push_gap_seconds` or
of `imapceph_jmap_push_handover_seconds` above 10 s;
`imapceph_jmap_push_verifyq_age_seconds` above 60 s;
`imapceph_jmap_push_partition_events_total{event="lost"}` rising for
long (a lock object or an OSD in trouble); `event=push_scan_listing`
in the logs (no effective `admin.kv_index: read`: run `shardpost index
backfill kv`).

`shardpost system migrate` refuses while `jmap/push/lease` has a time in
the future: the marker of the partitioned pods counts like the lease of
the single one, so the runbook of the migration ("push to 0, exit 2
right after is normal") holds unchanged.

## Job runners

Since 0.52.0 the background workers of the servers — the job runner
with the expiry and `fts_due` workers, the upload expiry, the delayed
submission worker and the learning worker — can run in pods of their
own: `shardpost runner` (role `runner`), component
`deploy/components/runner` (four pods, up to eight, a disruption budget
of one, a network policy that admits the health and metrics port alone).
The servers then run with `WORKERS_RUN=false` and are front-ends: they
keep the jobs API, the registration, the auth flush and every writer of
the queues ([configuration](../configuration.md#background-workers-workersrun)).
The push workers stay a deployment of their own (another egress, the
VAPID secret, a scale of their own). croessner keeps its two combined
pods and no runner: with two pods a third brings no relief, and a single
runner would be a new single point. The separated topology is checked
with `make test-docker` and a time-boxed smoke test.

**What the runner is.** It reads the servers' configmap (the system pool
included: its manifest sets no `CEPH_SYSTEM_POOL`, which would win over
the configmap) and the learning Secret, needs no TLS, VAPID or Nauthilus
secrets (its load leaves the secret files of the authentication and of
the push worker unread; a relay password file, which the delayed
submission worker sends with, must be mounted), opens no protocol listener and no admin
listener, and registers with the role `runner`. Diagnosis runs through a
server pod as before: `kubectl exec ... shardpostctl job list`,
`shardpostctl server list` (the runners' registrations show there, with
`WORKERS true` on combined servers and `false` on front-ends). It checks
the placement of the store's own namespaces every minute like a server
and drains when it is lost. Its termination grace period is 60 s: jobs
are handed over (`event=job_release`), running learning jobs get
`imapsieve.learning.shutdown_seconds`, and its delayed submission
partitions are named in a notify so that another worker takes them at
once instead of at its next poll.

**Switching an existing cluster.** First apply the `runner` component
(two to four pods) and wait until its pods are ready: the runners take
the leases beside the servers; nothing moves yet. Then add the component
`deploy/components/runner-frontends`, a separate apply: the servers roll
out with `WORKERS_RUN=false`:
each old pod hands its jobs over (release plus notify, the runners take
them at once), the leases of the expiry, `fts_due` and upload workers go
to the runners at their next cadence (within a minute), the learning
lease after `lock_retry_seconds`, and the submission partitions at once
(the notify names them). Nothing is lost: the queues are durable, and a
delayed submission goes at most one poll late. A new cluster starts
separated. **The way back** is the reverse: remove the component
`runner-frontends` (the servers take the workers up again), then the
`runner` component. Always "able to take first, then give up".

**Mixed versions and the way back of 0.52.0.** `--pods` of `shardpostctl
fts layout3|dictfilter|online enable` counts servers and runners since
0.52.0. A server before 0.52.0 that answers the check counts runners
not: it reports too few and refuses, the safe side; so runners come
only when every server has 0.52.0. After an image rollback of the
servers `--pods` counts servers alone again, and an enable that was
computed with runners refuses: determine `n` anew (front-ends alone).
Delete the runner deployment before the rollback: an older image does
not know the command `runner` and refuses to start.
`imapsieve.learning.concurrency` goes into the configmap only once every
pod runs 0.52.0, and out of it before a rollback.

**Sizing.** `jobs.max_parallel` 8 per runner through `JOBS_MAX_PARALLEL`
(the configmap's value would reach the front-ends, where it is without
effect); the real bounds stay cluster-wide (`fts.build_max_running`, the
account locks, the batch fan-out in the pod). Whether the slots suffice
shows `imapceph_job_claim_wait_seconds`. The learning programs'
`extprograms` `max_parallel` has to reach `imapsieve.learning.concurrency`
(8 on a large cluster), or the program phase bounds the jobs; the server
warns at startup. Delayed sending with 16 partitions keeps
`max_partitions` 0, so that a failed runner's partitions are taken by
the others.

**HPA.** Front-ends may scale on CPU once `workers.run` is false (their
CPU follows the sessions); a scale-down waits for the drain
(`DRAIN_SECONDS`). Runners get no HPA on CPU: jobs come in bursts, and a
scale-down in the middle of a batch costs a hand-over; a later scaling
on backlog (due jobs, `imap_imapsieve_learning_lag_seconds`) with a
stabilization of 15 minutes or more is possible. Push gets no HPA either
(see [Push partitions](#push-partitions)).

## Delayed submissions

Delayed JMAP sending (0.43.0, [JMAP](../jmap.md#delayed-sending),
[storage](../architecture/storage.md#delayed-submissions)) is off until
`jmap.submission.delayed.max_delay_seconds` is set. It needs the admin
store indexes (`admin.kv_index` `write` or `read`) and a relay; every
server with a relay runs the worker unless
`jmap.submission.delayed.worker` is `false` or the server is a front-end
(`workers.run: false`, see [Job runners](#job-runners)), in which case
the runners send.

**Rollout.** Roll the release out to every server first, then set
`max_delay_seconds`. A server before 0.43.0 refuses `HOLDFOR` and
`HOLDUNTIL`, so a client that reads the capability from a new server
and lands on an old one gets `invalidProperties`; nothing is sent twice
or lost.

**Watching.** `imapceph_jmap_delayed_submissions_total{outcome}` and
`imapceph_jmap_submission_queue_entries{state="due"}` (see
[observability](../observability.md#reading-the-metrics-correctly)). A
`due` above 0 for longer than a poll means no worker sends. The log line
`sendqueue: partition lease taken` names the server that holds a
partition; `submission interrupted while sending` marks a submission
that is final with an unknown result.

**An account's submissions.** `shardpostctl submission list <account>`
(role `diagnose`) shows them; `shardpostctl submission cancel <account>
<id>` (role `support`) cancels a pending one for the user
([manual page](../man/man1/shardpostctl-submission.1)).

**At most once.** A submission is never sent twice. One whose server
stopped while sending, or whose conversation with the relay broke while
the message was transferred, is final with `delivered` `unknown` after
`sending_timeout_seconds` (one hour) and is not sent again; the user
sees that in `deliveryStatus` and decides. The timeout has to exceed the
longest conversation with the relay (ten minutes for its answer to the
message plus a step timeout per command); a shorter one reports live
conversations as unknown (`outcome="conflict"`), never sends twice.
A temporary failure before the transfer — the relay refusing for now or
unreachable, but also the account directory, the registry or the
message copy not readable — is retried with a backoff and counts
against `retry_attempts`; when they are used up the submission fails
(`delivered` `no`). A Ceph outage longer than the backoffs therefore
fails submissions that were due in it; a dial the shutdown cancels does
not count.

**Partitions.** The servers running the worker times `max_partitions`
must reach `partitions`; otherwise a partition nobody holds is never
sent. To survive the loss of one server, (servers − 1) ×
`max_partitions` must reach it. Keep `max_partitions` at 0 (every
server may hold every partition, the default) unless there is a reason
to spread them by hand. `imapceph_jmap_submission_queue_entries{state="unowned"}` above 0
and the log line `due submissions of partitions this server does not
hold are overdue` show it.

**Stopping a server.** A server that stops starts no new conversation,
waits for the running ones (up to the relay's timeouts, ten minutes for
its answer to a message; give the pod a grace period to match, or
accept the next point), and gives its partition leases back with a
notify that names them; another worker takes them at once (before
0.52.0 at its next poll). A server that is killed while
sending (SIGKILL) is safe for at most once: the submission ends as
unknown after the sending timeout and is not sent again. A server that
dies keeps its leases until the cls_lock runs out (three minutes).

**Mixed versions.** While any server before 0.43.0 runs, no message
copies may exist: keep `max_delay_seconds` at 0 until the last old
server is gone, and do not go back while the queue holds anything. Old
servers know nothing of the copies, records and queue entries:

- the scrub of an old server (`scrub.orphan_source` `listing` or
  `shadow`) takes a copy for an orphan blob and deletes it after two
  runs — the submission then fails;
- the purge of an old server leaves the copies, the records and the
  queue entries of the purged account behind; a server of 0.43.0 or
  later would then find them — it checks the account directory before
  sending and fails a submission of an account that is being purged or
  has neither a directory entry nor a registry, but nothing else stops
  it;
- `shardpost scrub --sweep --repair` of an old server takes the copies
  for remnants and deletes them;
- an old server neither sends nor cancels (`cannotUnsend`) a queued
  submission.

**Rollback.** Servers before 0.43.0 do not send queued submissions and
do not clean them up. Before going back:

1. Set `max_delay_seconds` to 0 on every server (no new delayed
   submissions; the workers keep sending what is queued).
2. Wait until the queue is empty: `imapceph_jmap_submission_queue_entries{state="all"}`
   is 0, and `shardpostctl submission list -all` finds no record (it
   lists the admin namespace once per page and also finds records
   whose copy or queue entry was lost). A submission due far ahead can be canceled with
   `shardpostctl submission cancel` (tell the user).
3. Then roll back.

What a rollback leaves behind with a non-empty queue: the records and
queue entries in `~admin`, and the message copies with their `sub:`
intents in the accounts. Nothing is sent until a server of 0.43.0 or
later runs again, which then sends what has become due (late). The
copies expire seven days after their release time, and a scrub of an
older server lists them as orphans and deletes them after two runs.

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
counts its listings. Run it with the servers' configuration: full-text
shards (`fts/`, `ftsh/`, `ftsd/`) above `server.fts_shards` in an
account are reported (`fts`) and never deleted, since they may be live
ones of servers configured with more; after lowering `fts_shards` on
purpose, run `fts_rebuild` for the account and remove the shards above
the new count with `rados rm`. A namespace whose account is created
again while the sweep looks at it is counted as `recreated` and keeps
everything; no object written later than `scrub.sweep_clock_margin_seconds`
(60) before the sweep read the namespace is taken for a remnant, so the
servers' clocks must agree (NTP) to well within that margin. It needs `mon 'allow r'` (it reads each pool's
`pg_num`), which the server's CephClient has. A namespace without
registry is deleted only when the account directory says it is being
purged, or a purge of 0.42.0 or later recorded it (`ix.purged`) and the
directory holds no entry (an active entry always wins); any other one is reported and left alone: `registry_missing` (the directory
holds it as active: a lost registry), `unregistered` (no directory
entry: an account that only kept Sieve scripts before 0.42.0, or what a
purge of an older version left; remove the latter by hand once
checked). When a repairing sweep ends it removes every purge record
older than its start less its grace period (`purged_forgotten`),
except those of namespaces in which it left something (an object
younger than the grace period, a failed delete, a namespace it
skipped), which stay for the next run. Registering the name again, and
a directory repair that activates the account or removes its stale
entry, remove the record as well. Before the sweep deletes anything in
a namespace without registry it checks again that the account was not
created meanwhile. See the
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

## Full-text layout 3

Layout 3 of the full-text index ([storage](../architecture/storage.md#full-text-index-layout-3))
writes a handful of objects per delivery instead of nearly all 288 of
layout 2. Servers before 0.45.0 cannot read it, so it is switched on in
steps, and only a cluster mark lets a server create it. Since 0.46.0
`fts.layout` defaults to `3`: a server creates layout 3 indexes as soon
as the mark is set, and layout 2 indexes as long as it is not. Each
server logs the result at startup and whenever it sees the mark change
(`full-text layout of new indexes`, with `configured`, `layout3_mark`
and `effective`); the gauges `imapceph_fts_layout_configured` and
`imapceph_fts_layout_effective` show the same.

**A new installation (0.46.0 or later).** Every server already has
layout 3 configured (the default), so:

1. Roll out every server and wait three minutes (see step 2 below).
2. `shardpostctl fts layout3 enable -pods <number of running pods>`.

That is all: every index created from then on has layout 3, and there
are no layout 2 accounts to convert. Accounts migrated in later get
layout 3 as well, sized by the import (`shardpostctl migrate`).

**An existing installation.** A cluster that runs a version before
0.45.0, or has layout 2 accounts, goes the steps below. A cluster that
has the mark set but should keep creating layout 2 indexes sets
`fts.layout: 2` explicitly before it rolls out 0.46.0: until then the
default `2` did that, from 0.46.0 on the mark alone decides.

**Switching it on.**

1. Roll out 0.45.0 (or later) to every server. Every server registers
   itself at startup and then every minute (`servers/<instance>` in the
   admin store), whether or not it runs jobs or an admin listener.
2. Wait three minutes after the rollout: a replaced pod leaves the
   registration of its predecessor fresh for that long, and `enable`
   would count one pod too many.
3. `shardpostctl fts layout3 status` lists the registrations; check that
   every running pod is fresh and reports `fts_layout3`.
4. `shardpostctl fts layout3 enable -pods <number of running pods>`. It
   refuses when the fresh registrations are not exactly that many and
   names them. `-force` skips the check (audited) — only when you are
   sure no older server runs.
5. From 0.46.0 on nothing to do: the default `fts.layout` is `3`, and
   new accounts get layout 3 from the moment the mark is set. Before
   0.46.0 (or with an explicit `fts.layout: 2`), set `fts.layout: 3` in
   the overlay and roll it out.
6. Convert the existing accounts, with little parallelism
   (`jobs.batch_parallel`), at a quiet time: `shardpostctl plan -all
   -layout 3 fts_layout`, `shardpostctl apply <plan-id>`. Each account
   is locked for its conversion: deliveries into it fail temporarily
   (LMTP 451, the MTA retries), new IMAP, POP3 and ManageSieve logins
   are refused and JMAP answers 503 with `Retry-After`, for the
   duration of its job. The time grows with the account: about 3.5 s
   for 50–70 messages (bench), about 3.5 minutes for an account of
   36 000 messages and 1.8 GB (croessner, 2026-10-02). Measure it with
   `shardpost bench measure --convert` first ([bench](bench.md)) and
   convert large accounts when their owners are least likely to be
   online.

From then on the `fts_due` worker (on the server that holds its lease,
with `fts.auto_reshard`) reshards an account whose objects outgrew
`fts.shard_target_keys`; `imapceph_fts_reshard_due_total` and
`imapceph_fts_layout_runs_total{mode="reshard"}` show it. An import
reshards between its batches itself (`shardpostctl migrate`): the import
lock would keep the jobs out until the import ends. The migrator talks
to the same pod through the socket of its sidecar, where the runs of the
reshard endpoint live; on another pod it works, too, only slower (a
stopped run is seen after its 90 s heartbeat aged).

**Findings and repairs.**

- A delivery or search of one account keeps failing with "the
  full-text index is being converted": a conversion whose run ended
  without clearing it. `shardpostctl fts layout show <account>` shows
  its token; `shardpostctl fts layout clear <account>` removes it when
  its run (job or import) has ended. The scrub job does the same, and
  so does every job or import that takes the account lock.
- "full-text index object missing, repair with fts_rebuild"
  (`imapceph_fts_shard_missing_total`, scrub `FTSShardsMissing`): an
  object of the current generation is gone; `shardpostctl plan
  fts_rebuild <account>`.
- Scrub `FTSMixedLayout`: layout 2 objects in a layout 3 account, written
  by a server before 0.45.0 after a wrong way back: `fts_rebuild`.
- Scrub `FTSVersionDrift`: see
  [below](#dictionary-filter-and-attached-headers-0500).
- `imapceph_fts_shards_capped_total{kind}` rises: an account's objects of
  that kind outgrew their target at the cap (`fts.shards_max` for
  `postings` and `headers`, `fts.dict_shards_max` for `dictionary`); the
  log line names the generation. Raise the cap; it takes effect after a
  restart of the pods. From then on the next delivery into the account
  finds room below the cap and queues it, and the `fts_due` worker
  reshards it (the account is locked for the job). To choose the time
  yourself, reshard it by hand at a quiet time right after the restart:
  `shardpostctl plan -due-only fts_layout <account>` and `apply -wait`.
  Raise the threshold at which Ceph reports large omap objects
  (`osd_deep_scrub_large_omap_object_key_threshold`, 200 000 keys) only
  if a target was raised beyond about 133 000 keys.

**Key counts.** `shardpostctl fts stats <account>` prints the shape of
an account's index and its terms and dictionary keys, in total and per
message, from the counters of the posting objects (S + 1 reads, no
lock). Ceph's large omap threshold is per object: compare the mean
dictionary keys per object and the largest posting object with 200 000.
`-exact` counts every object's keys instead (thousands of reads on a
large account, during deliveries, that transfer about its whole index
omap with the values; one object at a time unless `-parallel` says
otherwise; give `-timeout` room). On real
accounts that is all: the terms themselves are never read for
statistics. Only for test accounts named in `fts.stats_test_accounts`
does `-classes` count the terms by field, class and length and project
the dictionary for every identifier bound — the measurement of the
bench corpus with identifiers and of a test account filled with
`shardpost-load` ([bench](bench.md)); for any other account it is
refused.

**The registrations.** They are deleted 24 hours after their last
heartbeat by the expiry worker of the job system; with
`admin.kv_index: off` or `jobs.enabled: false` on every server only
`status` and `enable` delete them. With `admin.kv_index` `write` or
`off`, `status` and `enable` list the admin namespace (rare, and started
by an operator).

**Going back.** In order:

1. `fts.layout: 2` on every server (new indexes in layout 2); since
   0.46.0 this has to be set explicitly, because the default is `3`;
2. `shardpostctl plan -all -layout 2 fts_layout` and apply it;
3. `shardpostctl fts layout3 disable` — it removes the mark, waits five
   seconds and exits 0 only when no account has layout 3 any more
   (`list` shows the others);
4. roll the image back.

In an emergency, when an older server already runs against layout 3
accounts (IMAP search is slow but right there, JMAP search finds too
little): export `shardpostctl fts layout3 list`, run `fts_rebuild` for
those accounts under the older version, and let the sweep remove the
layout 3 objects later under 0.45.0 or newer.

### Separate dictionary caps (0.49.0): rollout and way back

Since 0.49.0 the dictionary objects have a cap and a target of their own
(`fts.dict_shards_max`, 512; `fts.dict_target_keys`), `fts.shards_max`
(now 128) bounds the posting and header objects only, and the trigger
of the writers is silent at a cap. A version before 0.49.0 knows one cap
for all three kinds: it computes D with `fts.shards_max`, and its
trigger checks no cap. A generation whose D is above that version's
`fts.shards_max` counts as due for it, and a `due_only` job of that
version (its `fts_due` worker, `plan -due-only`) **shrinks** it: a full
copy under the account lock, and objects above their target afterwards.

**Upgrade without an overlay entry.** A cluster that does not set
`fts.shards_max` gets the new default 128 (32 before): accounts whose
posting or header objects stopped at 32 because of the old cap, and
every account whose dictionary was capped, may now be due, and their
writers queue them on the next delivery — each reshard locks its
account. To choose the time:

1. Set `fts.auto_reshard: false` in the overlay before the rollout.
2. Roll out the image.
3. Find the candidates: the plan of `shardpostctl plan -due-only
   fts_layout <account>` is a dry run that shows the target the counts
   ask for (`due`, `capped`); `shardpostctl fts stats <account>` shows
   the counts (`fts layout3 list -verify` the shapes). A batch plan
   does not list targets per account.
4. Apply the large accounts one by one at a quiet time, then the rest
   as a batch, `shardpostctl plan -all -due-only fts_layout`, with
   little parallelism (`jobs.batch_parallel`; an account that is not
   due ends as a no-op after a short lock); then set
   `fts.auto_reshard: true` again. To keep the old behaviour instead, set
   `fts.shards_max: 32` and `fts.dict_shards_max: 32` explicitly.

**Order of overlay and image.** A version that does not know a
configuration key refuses to start (the configuration is read strictly).

- Rollout: first the image, then the new keys (`fts.dict_shards_max`,
  `fts.dict_target_keys`, `fts.convert_*`, `fts.stats_test_accounts`)
  into the overlay.
- Way back: first the new keys out of the overlay, then the image.
  `fts.shards_max` is known to every version.

**Against the shrinking (one of them is required):**

- **a:** `fts.auto_reshard: false` on every pod for the duration of the
  rollout and the way back, `true` again afterwards; meanwhile plan
  `-due-only` jobs only from pods of the new version, or after the
  rollout. Recommended for both.
- **b:** go back only with `fts.shards_max` at least the largest D in
  the store. `shardpostctl fts layout3 list -verify` prints the largest
  D of each page; the older version then computes D within it and
  shrinks nothing, at the price of posting and header objects that may
  grow up to the same count. Use it when the way back takes longer.

**The croessner overlay.** It sets `fts.shards_max: 128` since V0
(2026-10-03), when that cap applied to the dictionary of the largest
account (S/D/H 32/128/1). From 0.49.0 on the same value is the default
and bounds S and H only; D is bounded by `fts.dict_shards_max` (512 by
default, no overlay entry needed). Keep `fts.shards_max: 128` in the
overlay: it is the default anyway, and it is measure b for a way back
to 0.48 as long as no dictionary has more than 128 objects. The largest
account keeps its shape: its counts ask for 32/128/1 under both
versions. Set `fts.stats_test_accounts` (for example
`rbdpoc2@exampleserver.de` for the measurement) only after the image runs
everywhere.

**Changing a cap.** A cap takes effect after a restart of the pods.
The next delivery into an account whose objects outgrew their target
below the new cap queues it, and the `fts_due` worker reshards it (the
account is locked for the job). To choose the time for a large account,
run the job by hand right after the restart (`shardpostctl plan
-due-only fts_layout <account>`, `apply -wait`) or set
`fts.auto_reshard: false` until then.

### Dictionary filter and attached headers (0.50.0)

Two settings make the substring dictionary of large accounts smaller
([storage](../architecture/storage.md#full-text-identifier-filter)):
`fts.dict_filter: ids` gives a body identifier (letters and digits, at
least `fts.dict_id_min_runes` characters, default 25) one start entry
instead of one key per trigram; `fts.attached_headers: nosig` leaves
the signature and trace fields of attached messages out of the body
text. Both are off by default, and with the defaults nothing changes:
no new key, no other marker, the same search results. What they change
for the users is in [protocols](../protocols.md#search-sort-and-thread):
an identifier is no longer found by a part from its inside, and the
signature fields of forwarded and bounced messages are not searchable.

Both are properties of an index generation (its layout record shows
them: `shardpostctl fts layout show <account>` prints `dict_filter
ids/25` and `attached_headers nosig`). A generation gets them when it is
created — a fresh index, `fts_rebuild`, `fts_layout` — and only while
the cluster mark `fts/dictfilter` is set, because a server before 0.50.0
must not search such an index (it does not trust it and reads the
messages instead).

**Order of overlay and image.** The three keys are new: an older version
refuses to start with them. Rollout: first the image, then the keys.
Way back: first the keys out, then the image.

**Enabling it (croessner: N = 25 with `nosig`).**

1. Roll out 0.50.0 to every pod and wait three minutes.
2. `shardpostctl fts dictfilter status`: every fresh pod reports
   `fts_dict_filter`.
3. Put into the overlay and roll it out (the pods restart):

   ```yaml
   fts:
     dict_filter: ids
     dict_id_min_runes: 25
     attached_headers: nosig
   ```

   From now on IMAP `BODY`/`TEXT` check messages without the signature
   fields of attached headers; the existing indexes still have them (a
   superset, still trusted). New generations stay unfiltered until the
   mark is set.

   Choose `fts.dict_id_min_runes` once: a generation keeps the N it was
   built with, so a later change makes every filtered generation due for
   `plan -all -due-only fts_layout` — a full copy (P5) of each, under its
   account lock. And `nosig` is practically one-way at scale: going back
   to `all` means `fts_rebuild` of every account built with `nosig`,
   each reading all its messages under the account lock, and until then
   IMAP reads the messages of those accounts.
4. `shardpostctl fts dictfilter enable -pods <number of running pods>`.
   New accounts get both properties from now on.
5. Convert the existing accounts. `nosig` changes the indexed text, so
   it takes `fts_rebuild`, which reads every message again and builds
   the new generation with both properties; `fts_layout` would keep the
   old text (it copies the postings). The rebuild holds the account
   lock for its whole duration — deliveries into the account fail
   temporarily (LMTP 451, the MTA retries), new logins are refused —
   and IMAP searches of the account read the messages until it is done.
   1. The test account first, and measure the time:
      `shardpostctl plan fts_rebuild rbdpoc2@exampleserver.de`,
      `shardpostctl apply -wait <plan-id>`; then `shardpostctl fts
      layout show rbdpoc2@exampleserver.de` (both properties, `V` =
      `4`) and `shardpostctl fts stats rbdpoc2@exampleserver.de` (keys
      per message).
   2. The largest account at a quiet time, the same way
      (`de10000@srvint.net`).
   3. The rest as a batch with little parallelism
      (`jobs.batch_parallel`): `shardpostctl plan -all fts_rebuild`,
      `shardpostctl apply <plan-id>`.

   Only the filter, without `nosig`, needs no rebuild:
   `shardpostctl plan -all -due-only fts_layout` converts exactly the
   accounts whose filter differs from the configuration (the dictionary
   is derived from the postings; an account that is not due ends as a
   no-op after a short lock), `-dict-filter ids` forces it for one
   account. The plan of an account shows the target with the estimated
   dictionary objects (a sample of the posting keys; the job logs the
   factor).
6. Check: `shardpostctl fts layout3 list -verify` shows the properties of
   every account; `imapceph_fts_dict_keys_written_total{entry="prefix"}`
   grows with the deliveries; `rados listomapkeys … | wc -l` of the
   dictionary objects and `ceph df detail` (`stored_omap`) before and
   after; `ceph health` without `LARGE_OMAP_OBJECTS`.

**Way back.** In order:

1. Overlay: `fts.dict_filter: none` and `fts.attached_headers: all`
   (keep the keys for now), roll it out. IMAP checks the full text
   again; it does not trust the indexes built with `nosig` and reads the
   messages of those accounts (slow, but right) until step 2 — which
   for `nosig` is a rebuild of every such account (each reads all its
   messages under its account lock), so plan the time accordingly.
2. `shardpostctl fts layout3 list -verify` lists the accounts with
   `dict_filter` or `attached_headers`. Those with `attached_headers
   nosig` need `fts_rebuild` (exact: it reads the messages again); those
   with the filter only `shardpostctl plan -all -due-only fts_layout`
   (exact: P5 derives every trigram key from the postings, also of the
   messages delivered while the filter was on). A rebuild with the
   default settings takes both away.
3. `shardpostctl fts dictfilter disable` — removes the mark, waits five
   seconds, reads the presence index (its entries carry whether a
   generation has either property) and exits 0 only when no generation
   has either (65 otherwise, with the accounts).
4. Remove the three keys from the overlay, roll it out, then roll the
   image back (for a version before 0.49.0 see the shrinking above).

In an emergency, when an older server already runs against such
accounts: it does not trust them (IMAP slow, but right; JMAP finds
identifiers only as whole words and still misses the signature fields).
Export `shardpostctl fts layout3 list -verify -json` while 0.50.0 still
runs, and rebuild those accounts with `fts_rebuild` under the older
version.

**Scrub finding `fts_version_drift`** (`imapceph_fts_version_drift_total`):
a version marker that does not fit its generation. A `3` left by a
version before 0.50.0 that converted a filtered generation (back to
layout 2, or a reshard that dropped the filter from the record) is set
to `2` — the dictionary is whole. A `2` on a generation with the
filter or `nosig` (an older writer gave it its first ids while it was
empty, so an older reader would trust it) is set to `3` or `4`. A `4`
whose record lost the text preparation is only reported; `shardpostctl
plan fts_layout <account>` writes the record anew.

### Online reshard and rebuild (0.51.0)

Since 0.51.0 `fts_layout` and `fts_rebuild` can run *online*, without
the account lock ([storage](../architecture/storage.md#online-runs-of-the-full-text-index)):
the target generation is built beside the current one while deliveries,
logins and sessions go on, and one compare-and-swap switches.

**What users notice.** Nothing, with two exceptions: a search during the
run reads the layout record once more, and one search at the switch
starts over in the new generation. When an account gets so many
deliveries that the run does not catch up within
`fts.build_catchup_rounds`, deliveries and appends of that account wait
up to `fts.build_hold_ms` (2 s) once before the switch — slower, never
refused. Without the mark, or with an older server in the cluster, the
runs are offline as before (LMTP 451, refused logins, sessions ended for
the whole job).

**The mark `fts/online`.** Rollout:

1. Roll out 0.51.0 to every pod; wait three minutes (a replaced pod's
   registration stays fresh that long).
2. `shardpostctl fts online status`: every fresh pod reports
   `fts_online_build`.
3. `shardpostctl fts online enable -pods <number of running pods>`.
4. Optionally `fts.online_build: auto` in the overlay (after the image,
   see below): plans without `-online`/`-offline` — also those of the
   `fts_due` worker — then run online. With the default `off` only
   `-online` runs online.

A job runs as its plan decided (the plan records `online`). `plan
-online` without the mark is refused with 409; on a layout 2 account
with 400 (conversions between the layouts stay offline). During a rollout
there are no online runs.

**Way back.**

1. `shardpostctl fts online disable`: new plans run offline (`-online`
   409); a planned online job that has not started ends without a change
   (`deferred: mark_off`, no failed job; the `fts_due` worker plans it
   again, offline).
2. Wait for the online runs that started: `shardpostctl fts layout show
   <account>` shows `next_online` while one runs (`fts layout3 list
   -verify` for all accounts), or `shardpostctl fts layout clear
   <account>`.
3. Remove the new keys from the overlay, roll it out, then roll the
   image back.

In an emergency (the image is back while an online `next` is there):
older writers meet a hard freeze and wait, then fail temporarily, until
the jobs scrub or `fts layout clear` (which an older version knows)
removes the run; its `w:` keys stay as harmless leftovers that an older
version never reads (the scrub of 0.51.0 removes them later). Generations
an online run switched to are ordinary layout 3 generations.

**The reshard window** (`fts.reshard_window`, `fts.reshard_window_tz`).
The `fts_due` worker plans its reshards inside the window, at most
`fts.build_max_running` at a time, and only when the estimate
(`fts.build_ms_per_message_copy` × documents) fits into what is left of
it; the others wait for the next window start plus a stable spread per
account. An account whose largest object holds
`fts.reshard_urgent_factor` (1.8) times its target is planned at once,
in or out of the window and whatever `build_max_running` and the
estimate say (near 2·T, where Ceph reports large omap objects). A batch of `fts_layout` or `fts_rebuild` keeps the window
too: outside it the job shows `waiting for the reshard window`, gives
itself back until the next window starts and goes on from its
checkpoint; `-anytime` ignores the window. A run whose estimate is longer
than the whole window does not start (`window_too_small`, metric
`imapceph_fts_build_window_too_small_total`; the worker looks again at
the next window start plus the account's spread, so once a day, and a
batch fails the account): plan the account on its own (a
single plan runs at once) or widen the window. A run that overruns its
window ends normally; runs over several nights come with a later stage.
On the days the clocks change the window is an hour shorter or longer.

**Enabling the dictionary filter and `nosig` on croessner through
online rebuilds.** Instead of step 5 of the runbook above (offline
rebuilds, each locking its account for minutes):

1. 0.51.0 on every pod, `fts online enable -pods <n>` as above.
2. Overlay (after the image): the window and, for the reshards of the
   worker, `auto`:

   ```yaml
   fts:
     online_build: auto
     reshard_window: "02:00-05:00"
     reshard_window_tz: Europe/Berlin
   ```

3. The dictionary filter as in steps 1–4 of the runbook above: the
   overlay with `dict_filter: ids`, `dict_id_min_runes: 25`,
   `attached_headers: nosig`, then `fts dictfilter enable -pods <n>`.
4. The protocol-level smoke test on the test account:
   `hack/online-smoke.sh` runs an online `fts_rebuild` of
   `rbdpoc2@exampleserver.de` while it delivers at a low rate, keeps an
   IMAP session logged in and tries POP3 and JMAP logins, and checks
   that no delivery failed, no login was refused, the session got no
   `BYE` and the logs show no lock for the account (credentials from the
   secret `imaptest-creds`, never printed). Then measure the test
   account's own run:
   `shardpostctl plan -online fts_rebuild rbdpoc2@exampleserver.de`,
   `shardpostctl apply -wait <plan-id>`. The result shows `mode
   reindex`, `rounds`, `caught_up`, `hold_ms`, `orphan_ids`;
   `shardpostctl fts layout show` both properties, `V` = `4`.
5. `de10000@srvint.net` the same way (a single plan runs at once,
   whatever the window; at a quiet time anyway), then `~public`.
6. The rest as a batch inside the window: `shardpostctl plan -all
   -online fts_rebuild`, `shardpostctl apply <plan-id>`. Accounts without
   a layout 3 index count as `unsupported` (none on croessner), a busy
   account is tried again at the end.
7. Check: the LMTP log without `delivery deferred, account locked`, the
   IMAP log without `session closed, account locked`,
   `imapceph_fts_build_runs_total{result="ok"}` per account,
   `ceph df detail` before, during (two generations of an account at a
   time) and after.

**Scrub finding `fts_build_leftover`:** `w:` keys or `w#` of an online
run that the layout record no longer names (a crashed run, or one an
older version cleared). The scrub removes them with the repair class
`uncovered`, keeping those of the run `L.next` names; they are harmless
either way.

## Online quota recount and mailbox lock

Since 0.52.0 two more maintenance jobs run without the account lock
(temp/16): `quota_recalc` with the mark `quota/online` recounts the
usage beside the writers ([quota](../quota.md#recounting-the-usage)),
and `mailbox_delete` with the mark `mailbox/lock` locks only the
mailboxes it removes. Without the marks both behave as before 0.52.0.

**What users notice.** Nothing of a recount. Of a mailbox deletion only
the mailboxes it removes: a write into one is refused (IMAP `NO
[UNAVAILABLE] Mailbox is being deleted`, JMAP `forbidden` or
`invalidProperties` on `mailboxIds`), a Sieve `fileinto` into one goes
to INBOX (`imapceph_lmtp_fileinto_redirected_total{reason="locked"}`),
and a session that has one selected is closed with `BYE [UNAVAILABLE]
Selected mailbox is being deleted` — at once when it idles, otherwise at
its next command. No 451, no refused login, no `BYE` for anybody else.

**The mark `quota/online`.** Rollout:

1. Roll out 0.52.0 to every pod; wait three minutes.
2. `shardpostctl quota online status`: every fresh pod reports
   `quota_online_recount`.
3. `shardpostctl quota online enable -pods <number of running pods>`.
4. Optionally `quota.online_recount: auto` in the overlay: plans without
   `-online`/`-offline` then run online; `plan -all quota_recalc -online`
   recounts every account without a lock.

A server before 0.52.0 does not capture its usage changes: with the mark
set too early the counters a run switches to would be wrong (a following
run corrects them; no mail is lost). `plan -online` without the mark is
refused with 409.

A run that ends `busy` (an open reservation older than the barrier, an
import or scrub on the account, a switch that did not go through on an
account with dozens of registry writes a second) is planned again with
a backoff; `imapceph_quota_recount_runs_total{result="busy"}` is no
alarm. Alarms: `result="error"`, `imapceph_quota_recount_dead_intents_total`
above 0 (a crash or an operation hanging for longer than
`quota.recount_dead_intent_seconds`, default 600 s: look for the intent
in the registry, the scrubber removes it after
`scrub.intent_grace_seconds`), and a growing
`imapceph_quota_recount_hold_seconds`. A reservation intent that stands
without being removed (a hanging pod) keeps every run `busy` with
`reason="intent"` and the key in the report for up to the dead-intent
time; after it the run goes through.

Way back: `shardpostctl quota online disable` (new plans offline), wait
for the running jobs or cancel them (the cancel clears the run key),
remove the `quota.recount_*` keys from the overlay, roll the image back.
Leftovers after an emergency rollback (a run key, captures) are harmless
to an older version, which never reads them; the scrub of 0.52.0 removes
them later (`quota_capture_leftover`).

**The mark `mailbox/lock`.** Rollout as above with `shardpostctl mailbox
lock status|enable -pods <n>` (capability `mailbox_lock`) and optionally
`jobs.mailbox_delete_online: auto` (needs `jobs.settle_seconds` of at
least 7: the time IMAP sessions take to see the lock). A server before
0.52.0 does not see the lock and would deliver into a mailbox that is
being deleted — with the mark set too early such a mail is lost with the
mailbox.

The job locks the targets (M1), waits `jobs.settle_seconds` and, at most
`jobs.mailbox_lock_barrier_seconds`, for the operations that started
before the lock (M3), checks the plan (a delivery into a target since the
plan is `ErrDrift`, the mail stays), unregisters the targets (M5) and
frees the account for account locks (M6, phase `destroying`) before it
releases blobs and shards (M7). A `busy` run (an import holds the
account, a scrub repairs it, another `mailbox_delete` locks a target, a
MOVE into or out of a target stands — also an abandoned one, which the
scrub completes after `scrub.intent_grace_seconds` or `shardpostctl
account scrub` at once) is planned again. The plan's fingerprint leaves
`HIGHESTMODSEQ` out online: flag changes until the lock are no drift.

Way back: `shardpostctl mailbox lock disable`, wait for the running jobs
or cancel them (the cancel unlocks their mailboxes), remove the keys from
the overlay, roll the image back. A `lock` field an older version finds
in a record is ignored and dropped when it rewrites the record; a job of
0.52.0 that meets such a record stops without deleting (fail safe).

## Pools

Three pools (`deploy/base/ceph/pools.yaml`):

| Pool | Holds | Type |
|---|---|---|
| `shardpost-index` | the accounts' index objects (omap) | replicated |
| `shardpost-data` | message blobs, learning copies | erasure coded or replicated |
| `shardpost-system` | the store-wide state once `ceph.system_pool` names it: admin store, account directory, queues, locks, server metadata | replicated |

Deleting a `CephBlockPool` deletes the pool and everything in it. Protect
all three against that:

```bash
for p in shardpost-index shardpost-data shardpost-system; do
  ceph osd pool set "$p" nodelete true
done
```

The system pool is prepared by the manifests but not used while
`ceph.system_pool` is empty (the default; the store-wide state then
stays in the index pool as before 0.47.0). The cephx caps of the
`CephClient` include it already: they must allow the pool before any
process runs with `ceph.system_pool`. A cluster is switched only with
the system tools (0.48.0): `shardpost system init --pool <pool>` for a
new cluster, `shardpost system migrate --to <pool>` in a maintenance
window for an existing one (see
[Moving the store-wide state](#moving-the-store-wide-state)). The bench
uses a system pool of its own (`--system-pool`, see [Bench](bench.md)).

What a process says about the placement (see
[Storage: placement](../architecture/storage.md#system-pool-and-placement)):

| Message | Meaning, what to do |
|---|---|
| `the fence in the index pool … says it lives in pool …; set ceph.system_pool` | the state moved to a system pool; this process has an old configuration |
| `system pool … is not initialized and the index pool holds no system state` | a new cluster: run `shardpost system init --pool <pool>` |
| `… holds the system state of an existing cluster: run "shardpost system migrate --to …"` | an existing cluster: migrate it in a maintenance window |
| `the initialization of system pool … is incomplete` | an init stopped: run it again |
| `a migration of the system state … is under way` | wait for the migration |
| `inconsistent placement: …` | fence and mark disagree: look at both before anything else |
| `… was withdrawn (mark retired …)` | the placement was reverted: clear `ceph.system_pool` |
| log `event=system_placement_lost` | a running server or push worker read a contradiction and ends after draining; it will not start again until the configuration matches |
| log `event=system_placement_unknown`, `imapceph_system_placement{state="unknown"} 1` | the check could not read; a running server or push worker goes on, and one that starts starts anyway (its periodic check decides later). A Ceph problem, not a placement one |
| `placement unknown: …` at the start of a `shardpost` command | a short-lived command (scrub, backfill, sweep) could not read the placement and does not start; run it again once Ceph answers |

Set `ceph.system_pool` on the server, the push worker and the scrub
CronJob together: a process whose setting does not match refuses to
start instead of writing beside the others. The manifests set the
environment variable `CEPH_SYSTEM_POOL` (empty until the migration), and
an environment variable wins over `server.yaml` even when it is empty:
change the value in the manifests, not in the configuration file.

### Pool sizes for a large cluster

For a cluster of about a million accounts and some hundred server pods
(data on HDD, index and system pool on NVMe or SSD), under these
assumptions, to be confirmed before production: about 300 HDD OSDs with
NVMe DB/WAL for the data, about 48 NVMe OSDs for index and system, at
least three racks, 100–200 PG replicas per OSD.

| Pool | Type | Device class | Failure domain | `pg_num` | Autoscaler |
|---|---|---|---|---|---|
| `shardpost-system` | replicated 3, `min_size` 2 | nvme | rack (else host) | 128 | either `pg_autoscale_mode on` with `pg_num_min 128` and `bulk false`, or `off` with `pg_num 128` |
| `shardpost-index` | replicated 3, `min_size` 2 | nvme | rack | 2048–4096 | `bulk true` |
| `shardpost-data` | EC 4+2 (at least six failure domains) or 8+3 | hdd (with NVMe DB) | host or rack | 8192–16384 | `bulk true` |

- **System pool:** at most about 5 GB of data at a million accounts.
  The roughly 600 hot objects (queue heads, leases, the auth flush log)
  should spread over many primaries: 128 PGs × 3 replicas on 48 OSDs are
  about 8 more per OSD; the autoscaler alone would give a pool this
  small very few. A CRUSH rule of its own (`replicated_nvme_rack`). No
  pool quota; alert at 50 GiB used.
- **Index pool:** layout 3 of the full-text index has about 10–40
  objects per account, about 3 × 10⁷ objects and 2–20 TB of omap;
  2048 × 3 / 48 is about 128 replicas per OSD.
- **Data pool:** EC 4+2 with 8192 PGs is about 164 shards per OSD on
  300 OSDs.

Such a cluster runs with `admin.kv_index: read` and
`imapsieve.learning.index: read` (the defaults); a new one gets the
backfill markers of its empty indexes from `shardpost system init`, so
nothing lists from the first start.

`ceph.client_options` sets the client's objecter throttles
(`objecter_inflight_ops`, default 1024; `objecter_inflight_op_bytes`,
default 100 MiB): a pod that is full waits in its own client, unseen by
Ceph. `imapceph_rados_inflight_ops` shows the timed operations in flight
of a process.

## Moving the store-wide state

The system tools ([shardpost-system(1)](../man/man1/shardpost-system.1))
put the store's own namespaces into the system pool and take them back.
They are commands of the server binary and run as the Job
[`hack/system-job.yaml`](../../hack/system-job.yaml): the server's image,
ServiceAccount and Ceph credentials, `CEPH_SYSTEM_POOL` empty (the tools
take the system pool from `--pool`/`--to` or from the fence and refuse a
configured one that names another pool). Every run is one Job; each
prints its report as JSON on standard output and ends with an exit code:

| Exit | Meaning |
|---|---|
| 0 | done, or nothing to do |
| 1 | an error, a lost lease, an interruption: run it again, it continues |
| 2 | a precondition does not hold (a running process, a lease held by another tool, a target that is not empty, surplus objects, a missing `--confirm`); the report names it |
| 3 | a check found the pools different from what they must be (a digest, an object that must not be there) |

To run one command, put its arguments into the Job and wait for it.
With a small helper (bash or zsh; the kubectl prefix is a function, not
a variable such as `k="kubectl …"; $k …`, which only bash splits into
words — zsh runs it as one command name and fails):

```bash
# k: kubectl against the shardpost namespace of croessner.
k() { kubectl --context croessner -n shardpost "$@"; }

# system_job '<JSON args>': one run of the system tools; prints the
# report and the exit code.
system_job() {
  k delete job shardpost-system --ignore-not-found --wait
  sed "s|^          args: .*|          args: $1|" hack/system-job.yaml | k apply -f -
  until k get job shardpost-system -o jsonpath='{.status.succeeded}{.status.failed}' | grep -q 1; do sleep 5; done
  k logs job/shardpost-system | sed -n '/^{$/,/^}$/p'
  k get pod -l job-name=shardpost-system \
    -o jsonpath='exit {.items[0].status.containerStatuses[0].state.terminated.exitCode}{"\n"}'
}
```

Pin the image of `hack/system-job.yaml` to the release that runs the
cluster (0.48.0 or later, by digest) before the first run.

**The tool lease.** A tool that writes holds a lease in the fence for as
long as it works: a second one is refused at once (exit 2, naming the
holder). The holder renews it every 30 s and stops every write once it
could not for 90 s (exit 1). The lease of a tool that died is taken over
with `--wait` after its holder was seen idle for three minutes on the
taker's clock. The lease protects against a tool that crashed, not
against one that was stopped: a frozen process can send operations it
prepared once it wakes up. Therefore:

- `--wait` (a takeover) only when the Job or pod of the old holder is
  known to be gone (`kubectl get pod -l job-name=shardpost-system`);
- never suspend or pause the Job of a tool; delete it (SIGTERM: the
  tool stops writing and gives its lease up) and apply it again;
- run every host with NTP: the lease does not depend on the clocks, but
  a holder whose last renewal lies more than 30 s ahead of the taker's
  clock is refused;
- the tools keep the OSD operation timeout at 60 s or below and refuse
  `ceph.client_options` that set more.

Fencing the old holder at the OSDs (`ceph osd blocklist add` of its
client address before a takeover, which needs a mon cap to write for
the tools' Ceph client) would close the gap of a stopped holder; it is
an optional hardening for large clusters and not implemented.

### A new cluster

Create the pool (`pools.yaml`, `nodelete`, the caps of the `CephClient`),
then run `system_job '["system", "init", "--pool", "shardpost-system"]'`
before any server, and set `CEPH_SYSTEM_POOL` to `shardpost-system` on
the server, the push worker and the scrub CronJob (or `ceph.system_pool`
in `server.yaml` for a process whose env list an overlay replaces, as
the croessner overlay does for the server; check what each one gets
with `kubectl kustomize <overlay> | grep -A1 CEPH_SYSTEM_POOL` and the
`server.yaml`). A server with
`ceph.system_pool` on a pool without the init refuses to start, naming
the command. The init writes the completion markers of the empty
indexes, so `admin.kv_index: read` and `imapsieve.learning.index: read`
read without a listing from the first start.

### An existing cluster (croessner)

About ten minutes without service; LMTP deliveries wait in the MTA's
queue. The steps:

**Before, in normal operation:**

0. Every host runs NTP, also the one the Job runs on. Read the rules of
   the tool lease above.
1. 0.47.0 or later runs everywhere: the server, the push worker and the
   image of the scrub CronJob. Push, scrub and backfills register
   (`kubectl exec -i deploy/shardpost-server -c server -- shardpostctl
   server list` shows their roles).
2. The caps include the system pool, the pool `shardpost-system` is
   `HEALTH_OK`, `nodelete` is set on all three pools.
3. The check: `system_job '["system", "migrate", "--to",
   "shardpost-system", "--check"]'`. Target and probe must pass (exit
   0). The running registrations, the push lease, the watchers and the
   worker locks of the running servers (`ix.fts_due/lock`,
   `ix.uploads/lock`, `ix.jobs/expiry/lock`, `ix.submission/p<n>/lock`,
   `learning-worker`, …) it reports are expected now: in `--check` they
   are findings with `"blocking": false`. The migration itself refuses
   every one of them.
4. Look at how the overlay hands the system pool to each process, so
   that the switch in the window reaches all three. In
   `deploy/overlays/croessner-nauthilus` the server takes its
   configuration from `server.yaml` only: a later patch replaces the
   server container's env list, so a `CEPH_SYSTEM_POOL` env patch for
   the server has no effect there. The push worker and the scrub
   CronJob take the env patch (the environment wins over `server.yaml`).
   Check both before the window:

   ```bash
   kubectl kustomize deploy/overlays/croessner-nauthilus | grep -A1 CEPH_SYSTEM_POOL
   grep -n system_pool deploy/overlays/croessner-nauthilus/server.yaml
   ```

**In the window:**

1. Suspend the CronJob:

   ```bash
   kubectl --context croessner -n shardpost patch cronjob shardpost-scrub -p '{"spec":{"suspend":true}}'
   ```

2. Scale the push worker and the servers to zero and wait until their
   pods are gone. No bench, backfill or tools process may run either:

   ```bash
   kubectl --context croessner -n shardpost scale deployment shardpost-push --replicas=0
   kubectl --context croessner -n shardpost scale deployment shardpost-server --replicas=0
   kubectl --context croessner -n shardpost wait --for=delete pod -l app.kubernetes.io/name=shardpost-push --timeout=5m
   kubectl --context croessner -n shardpost wait --for=delete pod -l app.kubernetes.io/name=shardpost-server --timeout=5m
   kubectl --context croessner -n shardpost get pods
   ```

3. Migrate, and keep the report:

   ```bash
   system_job '["system", "migrate", "--to", "shardpost-system"]' | tee migrate-report.json
   ```

   Exit 2 right after the scale-down is normal: the push worker's lease
   stays valid for up to 90 s, job leases for up to 60 s, registrations
   without an orderly end count for up to three minutes. Run it again
   after 90 s; if it stays at exit 2, look at the holders and watchers
   the report names. Exit 1: run it again (it continues). Exit 3: stop
   and take the run back with `abort` (below). Then compare source and
   copy once more: `system_job '["system", "verify"]'` (exit 0).

4. Switch the configuration. For the server, set `ceph.system_pool` in
   the overlay's `server.yaml` (as done on 2026-10-02; an env patch for
   the server is overwritten, see step 4 before the window):

   ```yaml
   ceph:
     system_pool: "shardpost-system"
   ```

   For the push worker and the CronJob, `CEPH_SYSTEM_POOL` as a patch in
   `deploy/overlays/croessner-nauthilus/kustomization.yaml`:

   ```yaml
   - patch: |
       apiVersion: apps/v1
       kind: Deployment
       metadata:
         name: shardpost-push
         namespace: shardpost
       spec:
         template:
           spec:
             containers:
               - name: push
                 env:
                   - name: CEPH_SYSTEM_POOL
                     value: shardpost-system
   - patch: |
       apiVersion: batch/v1
       kind: CronJob
       metadata:
         name: shardpost-scrub
       spec:
         jobTemplate:
           spec:
             template:
               spec:
                 containers:
                   - name: scrub
                     env:
                       - name: CEPH_SYSTEM_POOL
                         value: shardpost-system
   ```

   Then check that all three get it (the push worker and the CronJob in
   the env, the server in its ConfigMap) and look at the diff:

   ```bash
   kubectl kustomize deploy/overlays/croessner-nauthilus | grep -A1 CEPH_SYSTEM_POOL
   kubectl kustomize deploy/overlays/croessner-nauthilus | grep 'system_pool:'
   kubectl --context croessner diff -k deploy/overlays/croessner-nauthilus
   ```

5. Start again: the apply brings the servers and the push worker back to
   their replicas; then resume the CronJob:

   ```bash
   make deploy
   kubectl --context croessner -n shardpost rollout status deployment/shardpost-server --timeout=5m
   kubectl --context croessner -n shardpost rollout status deployment/shardpost-push --timeout=5m
   kubectl --context croessner -n shardpost patch cronjob shardpost-scrub -p '{"spec":{"suspend":false}}'
   ```

6. Smoke test:
   - `shardpostctl server list`: roles, and the system pool of every
     registration;
   - `shardpostctl job list`: the old jobs are there;
   - `shardpostctl account list`: all 33 accounts;
   - `shardpostctl fts layout3 status`: the mark and the registrations,
     from the new pool;
   - GETMETADATA `""` `/shared/*`: the server entries from `~server`;
   - a new test account (its first delivery): its directory entry is in
     the system pool (`rados -p shardpost-system -N '~accounts' ls`
     grows; `account list` shows it), not in the index pool;
   - `hack/auth-smoke.sh`, `hack/kvqueue-smoke.sh`,
     `hack/learning-smoke.sh`;
   - an LMTP delivery and a JMAP upload;
   - `system_job '["system", "verify", "--source-only"]'` (exit 0).
7. A report under `docs/reports/<date>-system-pool-cutover.md`.

**After seven days or more,** once the owner confirmed it, delete the
quarantine: `system_job '["system", "cleanup", "--run", "<run>",
"--confirm", "<run>"]'` (the run is in the report and in `system status`).
The fence and the poisoned heads stay for good.

From the migration on, no image before 0.47.0 may run against the
cluster: it knows neither the fence nor the system pool. Should one run
nevertheless, it fails on the poisoned index heads (format 1000) instead
of reading an empty place, but what it writes as plain records lands at
the old place, where `verify --source-only` finds it.

### What stays at the old place

After the migration the store's namespaces of the index pool hold the
fence `~admin/system.moved`, the poisoned index heads the fence lists
(`heads`, with their digest `fenced_digest`), the sweep's objects under
`~accounts/sweep/` and, until `cleanup`, the quarantine
`<namespace>.moved/<run>` (for example `~admin.moved/<run>`) with the
old objects. `verify --source-only` checks exactly that. Anything else
there was written by a process that does not know the system pool: find
out which one before anything else (the object names say what it is:
`kv.servers/<instance>`, `kv.jmap/push/lease`, …), then delete them with
`verify --source-only --delete-surplus --confirm <run>`; `abort`,
`revert` and `cleanup` take the same option and refuse without it.

### Taking it back

- **Before the servers use the system pool** (the check failed, exit 3,
  or the window has to end): `system_job '["system", "abort", "--run",
  "<run>"]'`. Before the quarantine (phases M1 to M3) it only reverts the
  fence. From M4 on it brings the old place back to the digest of M4
  from the quarantine and the saved heads, retires the mark and reverts
  the fence; it refuses a system pool that changed since M4 (use
  `revert`). The quarantine is kept until `cleanup` (after its minimum
  age) or the next run. An abort that stopped continues when run again.
  Run on a run it aborted before, it checks the old place against the
  digest of M4 again and repairs it from the quarantine; `cleanup`
  refuses to delete that quarantine while the old place differs.
  `--delete-copies --confirm <run>` also deletes the copies in the
  system pool (the run state and the mark stay as the record of the
  run); otherwise the next run deletes them when its `--confirm` names
  this run, together with the quarantine. Then start the processes without `CEPH_SYSTEM_POOL`.
- **After the switch**, in a maintenance window like the migration:
  stop every process, then `system_job '["system", "revert", "--run",
  "<run>", "--confirm", "<run>"]'`. The system pool is the truth: its
  content is copied back, the poisoned heads overwritten, the index pool
  checked against it, the quarantine deleted, the mark retired, the fence
  reverted. Then clear `CEPH_SYSTEM_POOL` everywhere and start again. A
  revert that stopped continues when run again. A native init is
  reverted the same way.
- **A new run after `abort` or `revert`** finds the copies of the old
  one in the system pool and refuses until `--confirm <old run>` names
  it; it then deletes them and the old quarantine.

## Measuring the cost per account

`shardpost bench` fills two bench pools of its own (three with
`--system-pool`) with accounts and messages and reports objects, bytes
and RADOS operations per account and per delivery (scaling plan, stage
R4). It refuses the production pools, the system pool included. How to create and delete the bench pools, the cephx caps it
needs and the Job are in [Bench](bench.md); the options in the
[manual page](../man/man1/shardpost-bench.1).

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
without `allow_ec_overwrites`. The system pool (optional, see
[Pools](#pools)) is replicated as well. The equivalent of
`deploy/base/ceph/pools.yaml`:

```bash
ceph osd pool create shardpost-index replicated
ceph osd pool set shardpost-index size 3
ceph osd erasure-code-profile set shardpost-ec21 k=2 m=1 crush-failure-domain=host
ceph osd pool create shardpost-data erasure shardpost-ec21
ceph osd pool create shardpost-system replicated
ceph osd pool set shardpost-system size 3
for p in shardpost-index shardpost-data shardpost-system; do
  ceph osd pool application enable "$p" shardpost
  ceph osd pool set "$p" nodelete true
done
```

The cephx identity needs the same capabilities as the `CephClient` in
`deploy/base/ceph/cephclient.yaml`; `x` on the OSDs is required for object
classes (`cls_lock` for the POP3 maildrop lock):

```bash
ceph auth get-or-create client.shardpost \
  mon 'allow r' \
  osd 'allow rwx pool=shardpost-index, allow rwx pool=shardpost-data, allow rwx pool=shardpost-system'
```

The printed key is the value for `ceph.key`. Pool names and the user are
configurable (`ceph.index_pool`, `ceph.data_pool`, `ceph.system_pool`,
`ceph.user`).

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
  system_pool: shardpost-system   # the same on every node and process
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

### JMAP push workers and job runners

The push worker (`shardpost push`, see [JMAP](../jmap.md)) is a process
of its own with the same configuration file and the VAPID key. Run it on
two or more hosts: they share the 64 push partitions through RADOS
leases, and a stopped or failed host's partitions move to the others
(planned within 10 s, after a crash within 45 s). Its instance name is
`<hostname>-push`, so one push worker per host.

```ini
# /etc/systemd/system/shardpost-push.service
[Unit]
Description=ShardPost JMAP push worker
After=network-online.target
Wants=network-online.target

[Service]
User=shardpost
Group=shardpost
EnvironmentFile=/etc/shardpost/ceph.env
ExecStart=/usr/local/bin/shardpost --config /etc/shardpost/shardpost.yaml push
# The orderly end hands the partitions over in waves (up to about 40 s
# when the membership object is slow).
KillSignal=SIGTERM
TimeoutStopSec=45
Restart=on-failure
RestartSec=5
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true

[Install]
WantedBy=multi-user.target
```

Job runners (`shardpost runner`, see [Job runners](#job-runners)) are
optional: without them every server runs the background workers. With
them, the servers get `WORKERS_RUN=false` (an `Environment=` line in
`shardpost.service`), but only once the runners run, and the way back
removes it first. One runner per host (instance `<hostname>-runner`); it
opens no listener but `server.http_addr` for health and metrics, and
needs no TLS, VAPID or Nauthilus secrets.

```ini
# /etc/systemd/system/shardpost-runner.service
[Unit]
Description=ShardPost job runner
After=network-online.target
Wants=network-online.target

[Service]
User=shardpost
Group=shardpost
EnvironmentFile=/etc/shardpost/ceph.env
ExecStart=/usr/local/bin/shardpost --config /etc/shardpost/shardpost.yaml runner
# Jobs are handed over at the end; running learning jobs get
# imapsieve.learning.shutdown_seconds.
KillSignal=SIGTERM
TimeoutStopSec=60
Restart=on-failure
RestartSec=5
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true

[Install]
WantedBy=multi-user.target
```

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
