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

**Rollback** is safe: older servers ignore `~accounts`, and an older
purge does not set `purging`.

## Admin store backfill

Servers with `admin.kv_index` `write` (the default) or `read` keep hint
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

**When `read` may be switched on.** Only after a backfill completed
whose start lies after the last server without the indexes (an older
version, or one with `admin.kv_index: off`) stopped; if in doubt, run it
again with `--restart` first. Then set `admin.kv_index: read` on every
server. A server in `read` mode that finds no completion marker warns
at start ("admin.kv_index=read without a completed …"). Going back to
`write` is always possible.

**Expiry.** With `write` or `read`, one server (the holder of the lock
`ix.jobs/expiry/lock`) deletes jobs and their idempotency keys
`jobs.retention_days` after they ended, and plans that long after they
expired, every `jobs.expiry_seconds`. The listing cleanup of earlier
versions keeps running beside it in this version. Idempotency keys used
to stay forever; records from before the backfill expire once it ran.

**Rollback** is safe: older servers ignore the `ix.*` objects and keep
reading by listing; the records are unchanged. Queue entries of jobs an
older server ended are removed by the readers of this version later.

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
ExecStart=/usr/local/bin/shardpost --config /etc/shardpost/shardpost.yaml scrub
```

```ini
# /etc/systemd/system/shardpost-scrub.timer
[Timer]
OnCalendar=*-*-* 03:17:00

[Install]
WantedBy=timers.target
```

`scrub.grace_seconds` and `scrub.dry_run` apply as described above.

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
and the expiry worker's lock is released and taken again (see
[storage](../architecture/storage.md#admin-store-hint-indexes-and-due-queues)). Changing the key of the
existing identity in place invalidates it for every node at once. The
interruption-free way is a second identity with the same capabilities:
create `client.shardpost2`, switch the nodes one by one (`ceph.user` and
`CEPH_KEY`, then restart), and remove `client.shardpost` once no node
uses it.
