# Server configuration

The server loads its configuration once, before it opens Ceph, authentication and the listeners. Libraries: Viper and go-playground/validator.

```sh
shardpost --config /etc/shardpost/server.yaml
shardpost --config /etc/shardpost/server.yaml config check
shardpost --config /etc/shardpost/server.yaml scrub [--batch]
shardpost --config /etc/shardpost/server.yaml index backfill accounts [--restart]
shardpost --config /etc/shardpost/server.yaml index backfill kv [--restart]
shardpost --config /etc/shardpost/server.yaml index show kv [<key>...]
shardpost --config /etc/shardpost/server.yaml index backfill learning [--restart]
shardpost --config /etc/shardpost/server.yaml index show learning [<id>...]
```

`scrub` scrubs in its own process (`scrub.users`, else every account of
the account directory, else every namespace of an index pool listing);
`scrub --batch` submits the batch scrub the servers run and exits (see
[Scrub](#scrub-scrub) and
[Operations](operations/README.md#nightly-scrub)).
`index backfill accounts` fills the account directory once after a
rollout (see [Operations](operations/README.md#account-directory-backfill));
it has no configuration keys of its own. `index backfill kv` fills the
admin store's hint indexes and due queues (see
[Operations](operations/README.md#admin-store-backfill)); it uses
`admin.kv_index` (refuses `off`) and `jobs.retention_days`. `index show
kv` prints their state and that of the given admin store keys
(read-only). `index backfill learning` fills the learning queue's due
queue and owner index (see
[Operations](operations/README.md#learning-queue-backfill)); it uses
`imapsieve.learning.index` (refuses `off`) and
`imapsieve.learning.queue_shards`. `index show learning` prints their
state and that of the given learning jobs (read-only).

Alternatively, `SHARDPOST_CONFIG` determines the file path. `IMAP_CONFIG`
is kept as a fallback as long as `SHARDPOST_CONFIG` is not set. An
explicit `--config` takes precedence over both variables. An explicitly
empty `SHARDPOST_CONFIG` disables the old fallback. Precedence: built-in defaults, explicit YAML file, explicitly set legacy env variables. There is no implicit file lookup and no automatic reload. Changes in Kubernetes are applied through a controlled deployment rollout. Go runtime settings such as `GOMEMLIMIT` remain environment variables; additional standardized OTEL SDK options are still evaluated by the SDK.

The sections are `server`, `ceph`, `rook`, `tls`, `nauthilus`, `auth` (see [auth cache](#auth-cache-auth)), `smtp`, `sieve`, `scrub`, `logging`, `telemetry`, `imapsieve`, `extprograms`, `quota`, `admin`, `fts`, `limits` (see [account limits](#account-limits-limits)) and `jmap` (see [JMAP](jmap.md); without `jmap.addr` that front-end is not started). The complete field and env mapping is documented in `internal/config/config.go`, `internal/config/imapsieve.go`, `internal/config/extprograms.go`, `internal/config/quota.go`, `internal/config/admin.go`, `internal/config/fts.go`, `internal/config/limits.go`, `internal/config/auth.go` and `internal/config/jmap.go` (including `jmap.push`); `extprograms`, the learning rules, the quota rules and warnings as well as the admin roles and clients are configurable via YAML only (see `docs/imapsieve.md`, `docs/quota.md` and `docs/admin-api.md`).

```yaml
server:
  imap_addr: ':1143'
  imaps_addr: ':1993'
  pop3_addr: ':1110'
  pop3s_addr: ':1995'
  lmtp_addr: ':2424'
  http_addr: ':8080'
  auth_backend: nauthilus
  insecure_auth: false
  memory_budget_bytes: 268435456
ceph:
  user: shardpost
  index_pool: shardpost-index
  data_pool: shardpost-data
rook:
  namespace: rook-ceph
nauthilus:
  transport: grpc
  grpc_addr: nauthilus.auth.svc.cluster.local:9444
  oidc_client_secret_file: /run/secrets/nauthilus/client-secret
tls:
  cert: /tls/tls.crt
  key: /tls/tls.key
sieve:
  addr: ':14190'
  global_dir: /etc/sieve
  before_dir: /etc/shardpost/sieve/before
  after_dir: /etc/shardpost/sieve/after
telemetry:
  protocol: http/protobuf
  endpoint: http://collector:4318
  sample_ratio: 0.1
  user_attributes: false   # account, mailbox and client address in spans
```

`server.insecure_auth` (default `false`) decides whether IMAP, POP3,
ManageSieve and LMTP accept authentication on a connection without TLS.
Turn it on only for an isolated test profile; JMAP has its own switch,
`jmap.insecure_auth` (default `false`).

The `rook` section applies only while `ceph.key` is empty: the server then
reads the key and, if `ceph.mon_host` is empty, the monitor list from the
Rook-managed secret and ConfigMap through the Kubernetes API and follows key
rotations. With `ceph.key` set, `rook` is ignored and no Kubernetes access
happens; see [running without Kubernetes](operations/README.md#running-without-kubernetes).

### Pools (`ceph`)

| Key | Environment | Default | Meaning |
| --- | --- | --- | --- |
| `ceph.index_pool` | `CEPH_INDEX_POOL` | `shardpost-index` | replicated pool of the accounts' index objects (omap) |
| `ceph.data_pool` | `CEPH_DATA_POOL` | `shardpost-data` | pool of the message blobs (may be erasure coded) |
| `ceph.system_pool` | `CEPH_SYSTEM_POOL` | empty | replicated pool of the store-wide state: the index part of the namespaces `~admin`, `~accounts`, `~learning` and `~server` (admin store, account directory, queues, locks, server metadata). Empty keeps them in the index pool, as before 0.47.0. Must differ from both other pools (by name here, by pool id when the store opens) |
| `ceph.chunk_size` | `CEPH_CHUNK_SIZE` | 4 MiB | chunk size of the message blobs |
| `ceph.client_options` | – | empty | further librados options of the connection, YAML only, values as strings: `objecter_inflight_ops` and `objecter_inflight_op_bytes` (the client's throttles of operations and bytes in flight; librados defaults 1024 and 100 MiB), `rados_osd_op_timeout` and `rados_mon_op_timeout` (seconds; the server sets 60 and 30). Other options are refused |

`ceph.system_pool` is not a switch to flip: a cluster has one placement,
and the data says which. A server, the push worker and every other
`shardpost` command check it when they open the store (two reads, no
listing):

- without `ceph.system_pool`, a fence in the index pool that says the
  state has moved refuses the start (`set ceph.system_pool`);
- with it, the system pool must carry the mark of this index pool and
  cluster, written by `shardpost system init` (a new cluster) or
  `shardpost system migrate` (an existing one); otherwise the start is
  refused with the command to run.

A server and the push worker repeat the check every minute and end
(after draining) when it reads a placement that contradicts the
configuration; a check that cannot read logs a warning and goes on (a
server or push worker that cannot read at startup starts in the state
`unknown`; a short-lived command does not start). Set the key on the
server, the push worker and the scrub CronJob together. The Kubernetes
manifests set `CEPH_SYSTEM_POOL` (empty until the migration), and an
environment variable that is set wins over the file even when it is
empty: change it in the manifests, not in `server.yaml`. The system
tools themselves (`shardpost system …`) do not check the placement and
take the system pool from their options or from the fence; they run
with the key empty and refuse a value that names another pool. They also
refuse a `ceph.client_options` with `rados_osd_op_timeout` above 60 and
raise the objecter throttles for their parallel copies
([shardpost-system(1)](man/man1/shardpost-system.1)).
See [Storage: placement](architecture/storage.md#system-pool-and-placement)
and [Operations](operations/README.md#moving-the-store-wide-state).

```yaml
ceph:
  index_pool: shardpost-index
  data_pool: shardpost-data
  system_pool: shardpost-system   # only after "shardpost system init" or "migrate"
  client_options:
    objecter_inflight_ops: "4096"
    objecter_inflight_op_bytes: "268435456"
```

### Global Sieve scripts

`sieve.global_dir` holds the scripts a user script can pull in with
`include :global`. `sieve.before_dir` and `sieve.after_dir` hold scripts
that run for **every** delivery (LMTP and the local delivery API) around
the account's active script, the way Pigeonhole runs `sieve_before` and
`sieve_after`:

- Every regular file `<name>.sieve` of a directory is a script; they run
  in name order (`10-spam.sieve` before `50-lists.sieve`). Other files
  and names starting with a dot are ignored. The directory is read for
  every delivery, so a changed ConfigMap takes effect without a restart.
- The order is: before scripts, the account's active script, after
  scripts. The next script runs only while the message is still kept
  (implicit or explicit `keep`); that keep is handed on instead of being
  carried out, and the actions of all scripts add up. A script that files
  the message elsewhere, discards or rejects it ends the sequence. So an
  after script only sees what the user's script left in INBOX, and a
  `fileinto "INBOX"` in a user script — unlike `keep` — ends the
  sequence too.
- An explicit `keep` is an action of its own, as within one script: once
  a script asked for it, no later script takes it away. A later
  `fileinto` or `discard` cancels only the implicit keep, so the message
  still lands in INBOX (and, with a `fileinto`, in the other mailbox as
  well); a later `reject` fails with an action conflict and the message
  stays kept. Policy decisions that must hold whatever the user's script
  does — a reject, a discard of known spam — therefore belong in
  `sieve.before_dir`, not in `sieve.after_dir`.
- A header edit (`editheader`) of one script is what the next one sees.
- A script that fails at runtime is skipped as if it had kept the message
  and logged; the others still run, and so do the other scripts of a
  directory when one file cannot be read. At startup and for
  `config check` a directory or file that cannot be read or a script that
  does not compile is an error.
- The flags of a keep are handed on with it: they are what the flag
  variable (`imap4flags`) of the next script starts with. So a script
  that marks the message (`addflag`, `keep :flags`) and keeps it does not
  lose the mark because a later script keeps the message too, and a later
  script can still remove it (`removeflag`, `setflag`, `keep :flags`).
- The startup check compiles the global scripts without an account, so
  one that uses `include :personal` is refused; `include :global` works.
- The environment variables `SIEVE_BEFORE_DIR` and `SIEVE_AFTER_DIR` win
  over the file when they are set, even to an empty value.
- The scripts run with the rights of the recipient. A `fileinto` into a
  public folder needs the `p` right of that account; without it the
  message stays in INBOX. That is how list mail is sorted into public
  folders for the accounts that may post there and left alone for
  everybody else.
- IMAPSieve does not run them, and neither does a delivery with
  `sieve=false` or into a named mailbox.

`shardpostctl sieve check` runs the same sequence, see
[admin API](admin-api.md).

### Delivery headers

Every copy LMTP or the local delivery API stores gets
`Return-Path: <envelope sender>` and `Delivered-To: <recipient>` above
its `Received` field, like Dovecot's LMTP; Return-Path fields the message
brought along are removed. Details in
[Sieve during delivery](imapsieve.md#delivery-headers).

| Key | Env | Default | Meaning |
| --- | --- | --- | --- |
| `server.lmtp_return_path` | `LMTP_RETURN_PATH` | `true` | Add `Return-Path` (null sender `<>`) and remove the message's own. |
| `server.lmtp_delivered_to` | `LMTP_DELIVERED_TO` | `final` | `final`: `Delivered-To` with the envelope recipient; `none`: no field. Other values are a startup error. |

### Sieve settings (`sieve.settings`)

`sieve.settings` (`SIEVE_SETTINGS`) holds the Sieve implementation
settings as `key=value;key=value`, under Pigeonhole's key names where
Pigeonhole has the setting. Among them, with their defaults:

| Key | Default | Meaning |
| --- | --- | --- |
| `sieve_vacation_min_period` | `1d` | Shortest vacation period; `0` lets `:days 0`/`:seconds 0` answer every message. |
| `sieve_vacation_max_period` | `60d` | Longest vacation period; `0` is no maximum. |
| `sieve_vacation_default_period` | `7d` | Period without `:days`/`:seconds`; between minimum and maximum. |
| `sieve_reject_mode` | `mdn` | `reject` sends a rejection message (MDN) and accepts the message; `protocol` refuses it in the protocol like `ereject`. |
| `rejection_subject` | `Rejected: %s` | Subject of the rejection message. |
| `rejection_reason` | `Your message to <%t> was automatically rejected:%n%r` | Text of the rejection message. |
| `postmaster_address` | `postmaster@%d` | Sender of rejection messages and of notifications without `:from`; `%d` is the recipient's domain. |

A `;` inside a value is written `\;`, a backslash before it or at the
end of a value `\\`; in YAML, put the whole value in single quotes (in
double quotes every backslash has to be doubled). The startup and
`config check`
refuse periods that do not parse or
contradict each other, an unknown `sieve_reject_mode` and a
`postmaster_address` that is no address. What these settings do is
described in [Sieve during delivery](imapsieve.md#sieve-during-delivery).

Unknown keys, mistyped YAML values, invalid env numbers, invalid boolean values, contradictory TLS pairs and explicit secret files that are missing or read as empty cause a startup error. Env numbers are always interpreted as decimal. A secret file takes precedence over the corresponding direct secret value. Error messages never print values.

`config check` validates the configuration structure, reads the referenced secret files and compiles the global Sieve scripts of `sieve.before_dir` and `sieve.after_dir`. It is no proof of connectivity to Ceph, Nauthilus, SMTP or the external programs; program files are checked when they are invoked. Certificates are loaded when the server starts afterwards.

A running server looks every `tls.reload_seconds` (default 60; `0` loads once) whether the files of `tls.cert`/`tls.key` and `jmap.tls_cert`/`jmap.tls_key` changed and loads the renewed pair, so a certificate that cert-manager renews in a mounted Secret reaches the listeners without a restart; a renewal is recognised by the files' content, and the reload logs the new certificate's serial and expiry. A renewed pair that does not load is logged and the previous certificate stays in use. Mount the Secret as a volume, not with `subPath`: a `subPath` mount never sees an update. The kubelet itself takes up to about a minute to update a mounted Secret. Without a file path, the existing env-based deployments remain usable.

## Client IP behind the Director

`server.proxy_protocol_trusted_cidrs` controls whether client addresses from
PROXY protocol v1/v2 are adopted ahead of the actual mail protocol/TLS. For
this to work, the Director must also set `haproxy.enabled: true` for the
backends in question. The setting applies jointly to IMAP, POP3, ManageSieve,
LMTP and JMAP; the list is global, one trust boundary for all listeners.

A proxy that reaches a listener through a Kubernetes NodePort has its
address rewritten on the way: with `externalTrafficPolicy: Cluster` the pod
sees a node address (the Calico tunnel address from another node, the
node's own address from the same node), so the header is honoured for some
connections and dropped for others. For JMAP behind HAProxy use
`externalTrafficPolicy: Local` and list the proxy addresses here, and let
the network policy admit the JMAP port only from those addresses — every
peer in the list can name any client address.

The production overlay uses the verified Calico pod network
`172.16.0.0/12` together with `network-policy.yaml`. The mail ports are
reachable only for Director pods in the `director` namespace and for the
controlled test tools `mailtest`/`imaptest` in the `shardpost` namespace.
These test tools are explicitly part of the proxy trust boundary. HTTP
health and metrics remain reachable. Whenever the Calico pools change, the
CIDR list has to be reviewed as well; individual dynamic Director pod IP
addresses are not recorded permanently.

Without a PROXY preamble, direct connections in which the server greets
first can take up to five seconds of detection time. That is why the
Director sends the preamble to the LMTP backend as well. Direct operator
probes need a matching timeout or have to send PROXY protocol themselves.

## Structured operational logs

`slog` writes JSON to stdout. `logging.level` (YAML) and `LOG_LEVEL`
(the environment takes precedence) accept `debug`, `info`, `warn`, `error`;
the default and the Kubernetes rollout use `info`. Invalid values prevent
startup.

- `info`: connection start and end, authentication success and rejection,
  deliveries and the server lifecycle.
- `debug`: additionally the known command names and durations for IMAP,
  POP3 and ManageSieve. `handler_error` denotes a handler error, not
  every negative protocol status. IDLE appears once the command has ended.
- `warn`: temporarily disrupted authentication, connection limits and the
  existing fallback messages.
- `error`: storage errors, failed deliveries and internal errors.

Sessions carry `proto`, a random `session_id`, `remote` and `local`.
`remote` holds the client address supplied by the trusted PROXY sender;
for direct access it holds the TCP peer. Successful logins include the
canonical `user`. The session ID stays the same across IMAP
UNAUTHENTICATE, only the user binding is removed.
`connection_close` carries the session duration in milliseconds. For LMTP,
the backend session begins after LHLO. The `tls` field describes TLS to
the backend, not a TLS channel already terminated at the Director.

Not even debug logs command arguments, SASL data, passwords, tokens,
script texts or mail contents. The only exception is the time-limited
protocol trace that is switched on explicitly per account
(`event=protocol_trace`, see [Admin API](admin-api.md#protocol-trace-for-an-account)):
it contains the protocol lines after login, with literals recorded only as
a size. Unknown commands are named `UNKNOWN`. The logs contain client IP
addresses and account names that are necessary for operations; these fields are not used as metric labels.

Example for Loki (replace the client IP with the public IP you are looking for):

```logql
{namespace="shardpost", app_kubernetes_io_name="shardpost-server"} | json | remote =~ "203\\.0\\.113\\.10:.*"
```

For further correlation, filter by `session_id`. At `info`, NOOP/FETCH/IDLE
produce no individual command logs; open, login and close remain visible.
Connections that were already closed before the rollout cannot be
reconstructed after the fact. Changing the level requires a restart.

### Protocol trace (`trace`)

| Key | Default | Meaning |
| --- | --- | --- |
| `trace.max_line_bytes` | 2048 | logged length of a line (80–65536) |
| `trace.default_duration_seconds` | 1800 | duration when the call does not specify one |
| `trace.max_duration_seconds` | 86400 | upper bound for every activation (60–604800) |
| `trace.recheck_seconds` | 60 | how often a traced session re-reads the switch |

YAML only. It is switched on through the Admin API (role `trace`), see
[Admin API](admin-api.md#protocol-trace-for-an-account).

### Namespaces

| Key | Default | Meaning |
| --- | --- | --- |
| `server.shared_prefix` | `Shared/` | client-visible prefix of the mailboxes other accounts share, addressed as `<prefix><account>/<mailbox>` (must end with the separator); empty takes the namespace away from clients |

A prefix is one level and ends with the separator, for example
`Other Users/` (Exchange, older Dovecot setups) or `shared/` (Dovecot's
own default) — a server whose users come from another one keeps their
names by setting what they are used to. Changing the
prefix renames what clients see: the mailboxes and their permissions
stay as they are, but every client re-discovers the folders under the
new name and treats them as new ones locally — local caches, folder
settings and client-side filter rules that name a folder path have to
be redone. Subscriptions survive: they are kept per account and per
mailbox, not by name.

A prefix may contain non-ASCII characters. Like every mailbox name it
reaches IMAP clients that enabled neither `UTF8=ACCEPT` nor `IMAP4rev2`
in modified UTF-7 (`Öffentlich/` is `&ANY-ffentlich/` in `NAMESPACE`
and `LIST`), all other clients in UTF-8.

With an empty prefix no client can reach a shared mailbox, although the
permissions remain in the store and the admin API still shows them. The
same prefix switches the shared accounts of JMAP: with it empty, the
JMAP session lists none (see [JMAP: Sharing](jmap.md#sharing)); so does
an empty `server.public_prefix` for the public account.

**Before changing a prefix on a running server**, check that no account
owns a mailbox below the new one:

```sh
shardpostctl mailbox list <account> | grep -E '^(Shared|Public)/'
```

Such a mailbox would still be listed but could no longer be opened: its
name resolves to the other namespace. Rename it first
(`shardpostctl mailbox rename <account> <old> <new>`); the server also
logs a warning, with the mailbox name, when it lists one. Creating,
renaming or importing a mailbox below a prefix is refused by the admin
API for the same reason; over IMAP such a name never reaches the
account's own namespace at all, so `CREATE "Shared/Sub"` answers
`NONEXISTENT` instead.

While a namespace is switched off its prefix is not reserved, so an
account can create a mailbox there in the meantime. Switching it back on
is therefore the same migration step as changing it: check first.

Version 0.26.1 changed the default from `Other Users/` to `Shared/`. A
server that was running before it keeps the old name by setting
`server.shared_prefix: "Other Users/"` explicitly.

### Share notifications

| Key | Default | Meaning |
| --- | --- | --- |
| `server.share_notifications_max` | 50 | share notifications an account keeps (at most 200); a new one beyond it removes the oldest |
| `server.share_notifications_retention_seconds` | 2592000 | how long one is kept (30 days) |

A change of the rights an account holds on another account's mailbox
or on a public folder — through IMAP `SETACL` or `DELETEACL`, the admin
API or JMAP `shareWith` — leaves a notification in that account (RFC
9670 3), which a JMAP client reads as `ShareNotification` (see
[JMAP: Sharing](jmap.md#share-notifications)). The notifications of an
account are one server-managed METADATA entry of its own namespace,
bounded by these two settings and by the 64 KiB of one entry; a second
change of the same mailbox replaces the first. Writing one is best
effort: an ACL change never fails because of it, and a failure is
logged (`share notification not written`).

### What an account starts with, and what it is subscribed to

| Key | Default | Meaning |
| --- | --- | --- |
| `server.autocreate` | the five below | mailboxes every account gets besides its INBOX |

```yaml
server:
  autocreate:
    - { name: Drafts,  special_use: "\\Drafts" }
    - { name: Sent,    special_use: "\\Sent" }
    - { name: Trash,   special_use: "\\Trash" }
    - { name: Junk,    special_use: "\\Junk" }
    - { name: Archive, special_use: "\\Archive" }
    - { name: Projekte, subscribe: false }
```

Each entry has a `name`, an optional `special_use` attribute (RFC 6154,
so a client finds the mailbox whatever it is called) and `subscribe`,
which defaults to true. This is Dovecot's `auto = create` versus
`auto = subscribe`, in one place instead of one block per mailbox. An
empty list (`autocreate: []`) leaves an account with its INBOX.

Two rules decide when this happens:

- A mailbox is created when the account logs in and does not have it, so
  a mailbox added to the list reaches the accounts that log in
  afterwards, not only new ones. A mailbox the account deleted comes
  back the same way.
- A mailbox is subscribed **only when it is created**, never afterwards.
  An account that unsubscribed one keeps it unsubscribed; the server
  does not argue with its user.

Subscriptions are per account everywhere else too: a mailbox of another
account carries its subscription in the pointer, a public folder in the
reading account's registry by mailbox id, so a folder that is renamed
stays subscribed. `shardpostctl mailbox subscribe` sets the flag for an
account from outside, which is how a migration hands over what the old
server had.

### Public folders

| Key | Default | Meaning |
| --- | --- | --- |
| `server.public_prefix` | `Public/` | client-visible prefix of the public namespace (must end with the separator); empty switches public folders off entirely, for IMAP and JMAP alike; without the delimiter it names the public account in a JMAP session |
| `server.public_quota_bytes` | 0 | limit of the public namespace as a whole, 0 = unlimited |
| `server.public_quota_msgs` | 0 | the same as a message count |

Public folders live in the store namespace `~public` and belong to no
account, so they do not count against a user's quota. They are created
and granted through the admin API, see
[Admin API: public folders](admin-api.md#public-folders).

### Maintenance jobs (`jobs`)

| Key | Default | Meaning |
| --- | --- | --- |
| `jobs.enabled` | true | runner in this server (all servers share the jobs) |
| `jobs.poll_seconds` | 5 | interval between scans for new jobs; with `admin.kv_index: read` a runner is also woken at once when a job is submitted or handed over at shutdown, and the poll is the fallback |
| `jobs.lease_seconds` | 60 | how long a claim holds without renewal (another server may take over afterwards); the expiry worker's lock lives this plus 120 s |
| `jobs.retention_days` | 14 | retention of finished jobs, their idempotency keys and plans (after `expires`) |
| `jobs.expiry_seconds` | 60 | how often the expiry worker (one server of the cluster, `admin.kv_index` `write` or `read`) deletes the jobs, idempotency keys and plans whose retention ended, and how often the other servers try to take its lock (5–3600) |
| `jobs.plan_ttl_seconds` | 3600 | how long a plan stays valid |
| `jobs.max_parallel` | 2 | concurrent jobs per server |
| `jobs.settle_seconds` | 10 | how long an exclusive job waits after setting the maintenance lock, so that running deliveries can finish and open IMAP/POP3 sessions notice the lock (they check it at most every 5 s when a command starts and disconnect with BYE) |

| `jobs.batch_parallel` | 2 | how many accounts a batch over all accounts works on at once in the server that runs it (1–32; a batch scrub uses `scrub.parallel`) |

YAML only. For operating them see [Admin API: maintenance jobs](admin-api.md#maintenance-jobs-stage-8c)
and [batches over all accounts](admin-api.md#batches-over-all-accounts).

### Scrub (`scrub`)

| Key | Env | Default | Meaning |
| --- | --- | --- | --- |
| `scrub.users` | `SCRUB_USERS` | "" | accounts `shardpost scrub` scrubs, comma-separated; empty: every account (not with `--batch`) |
| `scrub.grace_seconds` | `SCRUB_GRACE_SECONDS` | 3600 | objects younger than this are left alone (operations in flight); the batch scrub uses the servers' value; with `scrub.orphan_source: intents` and for `scrub --sweep --repair` at least 600 apply |
| `scrub.dry_run` | `SCRUB_DRY_RUN` | false | report only; `scrub --batch` then submits a batch of `index_check` |
| `scrub.parallel` | `SCRUB_PARALLEL` | 2 | accounts a batch scrub works on at once in the server that runs it (1–32) |
| `scrub.interval_days` | `SCRUB_INTERVAL_DAYS` | 7 | a batch scrub with `skip_idle` scrubs an idle account again at the latest after this many days (1–365) |
| `scrub.orphan_source` | `SCRUB_ORPHAN_SOURCE` | `shadow` | where a scrub finds what a crash left behind: `listing` lists the account's namespace in both pools (before 0.42.0), `shadow` lists and repairs from the listing and reports where the intents differ, `intents` reads the intents only and lists nothing; `intents` only when every server runs 0.42.0 or later (nothing checks it: messages that older servers move or expunge would look like orphans) |
| `scrub.intent_grace_seconds` | `SCRUB_INTENT_GRACE_SECONDS` | 3600 | how old an intent must be before a scrub takes it for the leftover of a crash (600–604800) |
| `scrub.sweep_clock_margin_seconds` | `SCRUB_SWEEP_CLOCK_MARGIN_SECONDS` | 60 | how far before reading a namespace `scrub --sweep` draws the line after which no object is a remnant, when that read is later than now less the grace period (1–3600); object mtimes have whole seconds and come from the writer's clock |
| `scrub.sweep_spill_shards` | `SCRUB_SWEEP_SPILL_SHARDS` | 256 | spill objects per pool of a new `shardpost scrub --sweep` (1–4096); more keep each smaller for a cluster with many objects |
| `purge.listing` | `PURGE_LISTING` | true | a purge lists the account's namespaces and deletes what it finds, as before 0.42.0, and reports what the data model does not name; `false` deletes what the data model names and lists nothing (see [operations](operations/README.md#scrub-without-listings)) |

`scrub.parallel`, `scrub.interval_days`, `scrub.orphan_source` and
`scrub.intent_grace_seconds` take effect in the servers that run the
batch, not in the pod that submits it. For the switch from `shadow` to
`intents` see [operations](operations/README.md#scrub-without-listings).

### IMAP watches (`server.imap_max_watches`)

| Key | Env | Default | Meaning |
| --- | --- | --- | --- |
| `server.imap_max_watches` | `IMAP_MAX_WATCHES` | 8 | change watches one IMAP connection follows (2–1024): IDLE takes one, which is always kept free for it; NOTIFY one, two with a selected mailbox of another account, or one per notified mailbox when the account object cannot be watched (`NOTIFY SET` then answers `NO [LIMIT]` if they do not fit, and the earlier NOTIFY state stays); beyond it IDLE polls |

The sessions of a process share their watches: one RADOS watch per
mailbox or account object however many sessions follow it (see
[storage](architecture/storage.md#notifications-and-search) and the
metric `imapceph_imap_watches`).

### Admin clients with the scope of all accounts

`admin.clients[].all_accounts: true` (YAML only) lets an mTLS client
list the account directory and plan, apply and cancel batches over every
account, each with its role; it excludes `admin.clients[].accounts`.
Socket callers always have it. See
[Admin API](admin-api.md#listeners-and-identity).

### Support tools (`admin.search_scan_limit`)

| Key | Default | Meaning |
| --- | --- | --- |
| `admin.search_scan_limit` | 50000 | a search request examines at most this many messages (role `content`); a longer search resumes with its cursor |

YAML only. See [Admin API: support tools](admin-api.md#support-tools-stage-8d).

### Admin store indexes (`admin.kv_index`)

| Key | Default | Meaning |
| --- | --- | --- |
| `admin.kv_index` | `read` | `off`: no hint indexes, no due queues, no expiry worker (the behaviour before the indexes); `write`: keep them up to date while readers still list the admin namespace (and the listing cleanup of old jobs and plans runs on every runner poll); `read`: also read from them (job runner, job lists, expiry of old records, push subscription scan) and list nothing on a running path. Environment: `ADMIN_KV_INDEX` |
| `admin.kv_queue_shards` | 4 | shard objects of a due queue (1–8) when its first writer creates it; an existing queue keeps its count |

`read` is the default. It is correct on a cluster whose
`shardpost index backfill kv` completed after the last server without
the indexes (before 0.38.0, or with `admin.kv_index: off`) stopped.
Without the completion marker a server configured for `read` logs a
warning and behaves as in `write` mode until the marker exists (it
looks for it every 30 seconds), so the gate keeps such a cluster on
`write` until the backfill ran. A new installation runs the backfill
once as well (it finds no records and completes at once). Set the same
mode on every server (a server in `read` mode does not see what a server
in `off` mode writes). Back to `write` is always possible. See
[Storage: admin store](architecture/storage.md#admin-store-hint-indexes-and-due-queues)
and [Operations](operations/README.md#admin-store-backfill).

### Learning queue index (`imapsieve.learning.index`)

| Key | Default | Meaning |
| --- | --- | --- |
| `imapsieve.learning.index` | `read` (`write` before 0.47.0) | `off`: no due queue, no owner index (the behaviour before); `write`: keep them up to date while the learning worker, the admin API's learning list and the account purge still list the namespace `~learning` of the data pool; `read`: also read from them (the worker is woken by a notify instead of listing every second). Environment: `LEARNING_INDEX` |
| `imapsieve.learning.queue_shards` | 4 | shard objects of the due queue (1–8) when its first writer creates it; an existing queue keeps its count |
| `imapsieve.learning.lock_retry_seconds` | 20 | how often a server that does not hold the learning worker lease tries to take it, with a jitter of ±50 % (1–3600; environment `LEARNING_LOCK_RETRY_SECONDS`). The holder works on once a second and when notified; a lease lost to a key rotation is taken again at once |

`read` takes effect only once `shardpost index backfill learning`
completed (with a start after the last server without the index
stopped) or `shardpost system init` wrote the completion marker of the
empty queue of a new cluster. Without the marker a server in `read`
mode logs a warning and behaves as in `write` mode until the marker
exists, so the default `read` changes nothing on a cluster that has not
run the backfill. A cluster of the size of millions of accounts runs
with `admin.kv_index: read` and `imapsieve.learning.index: read`: in
`write` mode the learning worker lists the namespace `~learning` of the
data pool every second, a walk over every placement group of the
largest pool.
See [Storage: learning queue](architecture/storage.md#learning-queue-due-queue-and-owner-index)
and [Operations](operations/README.md#learning-queue-backfill).

### Account limits (`limits`)

Upper bounds on what one account can grow. They keep the omap objects
that hold an account's state (the mailbox registry, a mailbox header,
the METADATA object) far below the size at which Ceph reports
`LARGE_OMAP_OBJECTS` and one account slows down the OSD that serves it.
The defaults are far above what accounts reach in normal use; `0`
switches a limit off.

| Key | Default | Meaning |
| --- | --- | --- |
| `limits.mailboxes` | 10000 | mailboxes of an account. INBOX is never refused. Environment: `LIMITS_MAILBOXES` |
| `limits.mailbox_names` | 50000 | distinct names an account ever created a mailbox under. The registry keeps the last UIDVALIDITY of every such name for good (so that a mailbox recreated under the same name never reuses one); a name used before can always be used again, RENAME does not count. Environment: `LIMITS_MAILBOX_NAMES` |
| `limits.keywords_per_mailbox` | 1000 | user keywords one mailbox knows. The keyword list of a mailbox only grows (it is what PERMANENTFLAGS and JMAP show), so the limit counts every keyword ever used in the mailbox; keywords it already knows and system flags are always accepted. Environment: `LIMITS_KEYWORDS_PER_MAILBOX` |
| `limits.metadata_entries` | 1000 | METADATA entries (RFC 5464) of an account: server and mailbox entries, shared and private, together. Only the number counts (each value is bounded to 64 KiB on its own). Entries the server keeps for itself (`/private/vendor/shardpost/jmap/...`: JMAP identities, vacation response, Sieve scripts, share notifications) count but are never refused. Environment: `LIMITS_METADATA_ENTRIES` |
| `limits.shared_pointers` | 10000 | mailboxes of other accounts shared with an account (the pointers ACL grants write into the grantee's registry). A grant that would add one more is refused on the granting side, before the ACL changes; changing the rights of an existing grant, `anyone` and public folders need no pointer. Environment: `LIMITS_SHARED_POINTERS` |

What a client sees when it reaches a limit is listed in
[Protocols: account limits](protocols.md#account-limits). Delivery never
loses mail to a limit: a Sieve `fileinto` that would have to create a
mailbox beyond `limits.mailboxes` or `limits.mailbox_names` goes to
INBOX like any other failed `fileinto`, and keywords a Sieve script adds
beyond `limits.keywords_per_mailbox` are dropped (the message is stored
with its system flags). The autocreated mailboxes (`server.autocreate`)
are skipped for an account at its mailbox limit.

An account import (admin API) is held to the mailbox limits, which it
checks before it writes anything for a mailbox, but not to the keyword
limit: an import reproduces the source mailbox as it is, and refusing
it halfway would leave messages whose keywords the mailbox does not
list.

The keyword limit is checked in the same compare-and-swap as the change
that adds the keywords, so it holds exactly (a MOVE checks it before it
starts and then completes). The other limits count omap keys before the
write and are advisory: writers that race past the check together can
exceed a limit by one entry each.

### Full-text index (`fts`)

| Key | Default | Meaning |
| --- | --- | --- |
| `fts.fallback_charset` | `windows-1252` | charset of text that is not valid UTF-8 and has no charset label, is labelled US-ASCII or names a charset that is not known (body parts, file names, raw 8-bit header values); any ASCII-compatible charset of the WHATWG list, e.g. `iso-8859-15`, `iso-8859-2`, `koi8-r`; UTF-8, UTF-16 and `replacement` are refused |
| `fts.html` | `text` | what is made of a `text/html` part: `text` its visible text (tags, comments, `script`, `style`, `template` and `title` dropped, character references decoded, a line break at every block element), `raw` the markup as it is (what earlier versions indexed), `skip` nothing |
| `fts.imap_search` | `substring` | how IMAP `SEARCH` matches text: `substring` as RFC 3501 and RFC 9051 specify (the index narrows the candidates, every candidate is checked); `word` lets the index answer `SUBJECT`, `BODY`, `TEXT` and `HEADER Subject` alone, by word, stem and part of a word |
| `fts.verify_parallel` | 8 | how many messages one IMAP `SEARCH` checks against its text keys at the same time (1–64), each read under `server.memory_budget_bytes` |
| `fts.layout` | `3` | object layout of indexes created from now on: `2` the fixed shards of every version before 0.45.0, `3` the adaptive index generations (see [storage](architecture/storage.md#full-text-index-layout-3)); `3` takes effect only while the cluster mark is set (`shardpostctl fts layout3 enable`), otherwise new indexes stay in layout 2. The default is `3` since 0.46.0 (before: `2`), so a cluster without the mark keeps creating layout 2 indexes and the mark alone switches it; `2` keeps layout 2 even with the mark (the first step of the way back). The startup log line `full-text layout of new indexes` and the metrics `imapceph_fts_layout_configured` and `imapceph_fts_layout_effective` show the result. The layout of an existing index is the one it records; `fts_layout` jobs convert |
| `fts.shards_min` | 1 | posting, dictionary and header objects of a fresh layout 3 index without a size hint (1–4096) |
| `fts.shards_max` | 128 | upper bound of the posting and header objects (S, H) a reshard chooses (`shards_min`–4096; 32 before 0.49.0, when it bounded the dictionary objects as well); at the cap the trigger is silent and the objects grow past `fts.shard_target_keys` (metric `imapceph_fts_shards_capped_total{kind}`, a log warning, once per index generation and server) |
| `fts.dict_shards_max` | 512 | upper bound of the dictionary objects (D) a reshard chooses (`shards_min`–4096, since 0.49.0); at the cap as `fts.shards_max` |
| `fts.shard_target_keys` | 100000 | keys a layout 3 posting or header object should hold (T_p, 10000–1000000). A writer that sees an object with more than 1.5·T_p keys queues the account for a reshard, so a value above 133000 is accepted with a startup warning: from 200000 keys per object Ceph reports large omap objects unless `osd_deep_scrub_large_omap_object_key_threshold` is raised. Up to about 130000 is sensible for large accounts (fewer posting objects a delivery touches) |
| `fts.dict_target_keys` | 100000 | keys a layout 3 dictionary object should hold (T_d, 10000–1000000, since 0.49.0; warning above 133000 as `fts.shard_target_keys`): more than 1.5·T_d dictionary keys per dictionary object ask for a reshard. The writers decide per object, the plan of `fts_layout` by the sums, so `plan -due-only` can find an account due before its writers queue it (see [storage](architecture/storage.md#full-text-reshard)) |
| `fts.convert_buffer_keys` | 262144 | least number of keys the copy of a conversion (`fts_layout`, phase P5) holds before it writes them (16384–4194304, since 0.49.0); it holds up to 1024 per target object when that is more, so that a write carries about 1024 keys or more. The largest pending piece is written early when the buffer is full (`imapceph_fts_convert_buffer_keys`) |
| `fts.convert_writers` | 8 | writes of a conversion's copy in flight at a time (1–64, since 0.49.0) |
| `fts.stats_test_accounts` | empty | accounts whose index terms `shardpostctl fts stats -classes` may classify: `path.Match` patterns on the lower-cased account, such as `bench-*` or `rbdpoc2@exampleserver.de`. For every other account `fts stats` only counts keys and refuses `--classes`; terms are never printed (since 0.49.0) |
| `fts.auto_reshard` | `true` | the `fts_due` worker plans `fts_layout` jobs for the queued accounts (it runs with the job system) |
| `fts.frozen_wait_seconds` | 10 | how long one indexing (a delivery, an import batch, a removal) waits, once, for a conversion that froze the account's index before it fails with a temporary error (0–60; the undo of a delivery, the upload expiry and the scrubber's orphans never wait) |
| `fts.import_reshard_sync_seconds` | 60 | how long `POST /admin/v1/accounts/{account}/import/fts-reshard` works before it answers 202 and goes on in the background (1–90; below the 2 minute default client timeout of `shardpostctl`) |
| `fts.dict_min_terms` | 0 | reserved for a later stage (dictionary only from a vocabulary size); no effect yet |

YAML only. `server.fts_shards` keeps the object count of layout 2, which
is also the target of a conversion back from layout 3.

**The text.** One preparation serves the index, IMAP `SEARCH` keys
checked against the message, the JMAP `header` filter and the cached
preview, so they agree:

- Header values are unfolded and decoded: RFC 2047 encoded words in
  any charset of the WHATWG list, raw 8-bit text in
  `fts.fallback_charset`. The index has the header field names as
  well, because IMAP `TEXT` covers the whole header. The JMAP Text
  form of a header value (`header:…:asText`) is decoded the same way.
- Every `text/*` part is converted from its charset parameter to
  UTF-8. A `text/html` part whose label is missing, US-ASCII or
  unknown is read in the charset its `<meta charset>` or
  `<meta http-equiv="Content-Type">` names. A part labelled with a
  single-byte charset (ISO-8859-x, Windows-125x, KOI8, …) whose bytes
  are valid UTF-8 with at least one non-ASCII character is kept as
  UTF-8: that is UTF-8 under a wrong label, which is common, far more
  often than Latin-1 text that happens to be valid UTF-8.
- The file name of every part (`Content-Type` `name`,
  `Content-Disposition` `filename`, RFC 2231 in any charset, RFC 2047
  words) and an attached message (`message/rfc822`,
  `message/global`) — its header as `Name: value` lines and its body
  text — belong to the body text. Other MIME part header fields do not.
- Bounds: at most 4 MiB of text per message; a part is read up to
  4 MiB, a part other than HTML only as far as there is room left for
  its text, and all parts of a message together up to 8 MiB, before
  conversion. Words of more than 40 characters are indexed as
  overlapping pieces (see [storage](architecture/storage.md)).

**The preview** is cut from the text parts alone (no file names, no
attached messages), and of a `multipart/alternative` from one part: the
plain text where it has text, else the HTML text (RFC 8970). It follows
`fts.html`: with `text` an HTML part gives its visible text (without its
title), with `raw` its markup, with `skip` nothing, so an HTML-only
message has an empty preview.

**IMAP SEARCH and what it costs.** With `substring`, a text key matches
when its value is a substring of the decoded text, without regard to
case. The index supplies candidates — never fewer than the messages
that match — separately for the header and the body of a key, and
combines them over `AND`, `OR` and `NOT`. Each candidate is checked:

- the header against the decoded header the index keeps per message
  (read for 256 messages at a time, no message is read);
- the body by reading the message, only where the body candidates
  include it.

A word of the value narrows the candidates when the index can look it
up completely: a whole word always; the start of a word unless more
than 4096 index terms begin with it; the inside of a word in the
subject, the addresses and the body when it has three or more
characters and lies inside at most 4096 index terms. The inside of a word of another header
field cannot be looked up, so a `TEXT` or `HEADER <name>` value that
may begin inside a header word (any value not starting with a space or
punctuation) checks the stored header of every message of the mailbox —
a few omap reads per 256 messages, cheap next to reading them — and
reads only the messages the body index allows. A key with no word the
index can narrow by (`BODY ab`, a single letter) reads every message of
the mailbox. `fts.verify_parallel` messages are checked at once, each
under the memory budget; a message that is expunged meanwhile is no
match, and a read the budget refuses is retried before the command
fails. A search stops when its client closes the connection (noticed
without reading what the client sends; not in a session with
`COMPRESS=DEFLATE`) or the server closes it; the session then ends
without an answer. `word` costs index lookups only and answers what earlier versions
answered: all words of the value in any order, by stem and by part of
a word.

**Existing messages.** The index keeps what a message was indexed
with. An account indexed entirely by this version carries a version
marker in its index (set for a new account with its first message and
by `fts_rebuild` when it completes; a rebuild that stops midway leaves
the account without it). Without the marker, substring search does not
use the index at all — an older index (HTML markup, undecoded words, no
file names) cannot be trusted even for whole words — and checks every
message of the mailbox, reading the headers of messages without a
stored header from the messages themselves: correct, but slow, until
the rebuild. A changed `fts.fallback_charset` or `fts.html` reaches
messages stored before only when they are indexed again; until then
substring search can miss such a message where the index lacks its new
words. Removing a message indexed
under other rules leaves terms it was indexed with behind (harmless;
the rebuild drops them). Run the rebuild for every account after the
upgrade, see [Admin API: maintenance jobs](admin-api.md#maintenance-jobs-stage-8c):

```sh
shardpostctl plan fts_rebuild user@example.org
shardpostctl apply -wait <plan-id>
```

The rebuild does not recompute the cached preview of an existing
message. The index, including the stored headers, is in the index pool
and does not count against the quota.

### JMAP search bounds (`jmap.search`)

| Key | Env | Default | Meaning |
| --- | --- | --- | --- |
| `jmap.search.snippet_read_bytes` | `JMAP_SEARCH_SNIPPET_READ_BYTES` | 262144 | how much of each message, from its front, `SearchSnippet/get` reads to find a match in the body; a match beyond it gives no preview |
| `jmap.search.max_attachment_reads` | `JMAP_SEARCH_MAX_ATTACHMENT_READS` | 1000 | messages one `Email/query` may read to answer `hasAttachment` for a message stored by an earlier version, whose index record does not carry it; a query past the bound remembers what it read, starts a background fill of the namespace and answers `serverUnavailable` until the fill is through |
| `jmap.search.attachment_backfills` | `JMAP_SEARCH_ATTACHMENT_BACKFILLS` | 2 | background fills of `hasAttachment` that run at once in a process (one per namespace); a namespace that finds no free slot is filled when a query asks again |
| `jmap.search.attachment_backfill_seconds` | `JMAP_SEARCH_ATTACHMENT_BACKFILL_SECONDS` | 600 | how long one background fill may run; what it wrote stays, and the next fill goes on from there |

`0` takes the default. The maintenance job `attachment_backfill` fills an
account ahead of time ([Admin API](admin-api.md#maintenance-jobs-stage-8c)).
A `SearchSnippet/get` reads at most
`jmap.max_objects_in_get` messages, so one call reads at most that many
times `jmap.search.snippet_read_bytes`. See [JMAP: Search snippets](jmap.md#search-snippets)
and [JMAP: hasAttachment](jmap.md#hasattachment). The other `jmap.*` keys
are described in [JMAP: Configuration](jmap.md#configuration).

### JMAP load (`jmap.*`)

| Key | Env | Default | Meaning |
| --- | --- | --- | --- |
| `jmap.max_concurrent_requests` | `JMAP_MAX_CONCURRENT_REQUESTS` | 8 | API requests one account runs at once in one server; advertised as `maxConcurrentRequests` |
| `jmap.max_concurrent_upload` | `JMAP_MAX_CONCURRENT_UPLOAD` | 4 | uploads at once, `Blob/upload` included; advertised as `maxConcurrentUpload` |
| `jmap.max_concurrent_downloads` | `JMAP_MAX_CONCURRENT_DOWNLOADS` | 8 | blob downloads at once |
| `jmap.max_event_sources` | `JMAP_MAX_EVENT_SOURCES` | 4 | open event sources |
| `jmap.limit_wait` | `JMAP_LIMIT_WAIT` | 2 | seconds a request queues for a slot at a full limit (0–10; 0 answers 429 at once); event sources never wait |
| `jmap.retry_after` | `JMAP_RETRY_AFTER` | 1 | `Retry-After` in seconds of a 429 and of a 503 from an undecided authentication (1–3600) |
| `jmap.max_concurrent_per_ip` | `JMAP_MAX_CONCURRENT_PER_IP` | 0 | requests one client address has in flight, checked before authentication; an event source gives its slot back once it holds its own, so open streams do not count; 0 is off |

For the four limits `0` takes the default. A queue holds at most twice
its limit; a request past it is answered 429 at once. `jmap.limit_wait`
stays well below the response timeouts in front (the Director's
`backend_response_header`, two minutes by default, and HAProxy's
`timeout server`). With `jmap.max_concurrent_per_ip` set and
`server.proxy_protocol_trusted_cidrs` empty the server warns at startup:
behind a proxy every client then has the proxy's address. See
[JMAP: Load](jmap.md#load).

### Delayed sending

JMAP delayed sending (`FUTURERELEASE`, see [JMAP](jmap.md#delayed-sending)
and [operations](operations/README.md#delayed-submissions)):

| Key | Env | Default | Meaning |
| --- | --- | --- | --- |
| `jmap.submission.delayed.max_delay_seconds` | `JMAP_SUBMISSION_DELAYED_MAX_DELAY_SECONDS` | 0 | the longest delay a client may ask for with `HOLDFOR`/`HOLDUNTIL`, advertised as `maxDelayedSend` (0–31536000); 0 offers no delayed sending. Needs `jmap.submission.relay_addr` and `admin.kv_index` `write` or `read` |
| `jmap.submission.delayed.worker` | `JMAP_SUBMISSION_DELAYED_WORKER` | true | this server runs the submission worker (with a relay); it runs also with `max_delay_seconds` 0, so that what is queued is still sent |
| `jmap.submission.delayed.partitions` | `JMAP_SUBMISSION_DELAYED_PARTITIONS` | 1 | worker partitions: the accounts by hash, each partition with a lease of its own (1–64); all servers should use the same value (a difference costs only duplicate reads: the record is the fence) |
| `jmap.submission.delayed.max_partitions` | `JMAP_SUBMISSION_DELAYED_MAX_PARTITIONS` | 0 | partitions one server holds at most (0: all; at most `partitions`). The servers running the worker times `max_partitions` must reach `partitions`, or a partition nobody holds is never sent (counted as `imapceph_jmap_submission_queue_entries{state="unowned"}` and warned about); (servers − 1) × `max_partitions` for failover. Keep the default 0 unless there is a reason |
| `jmap.submission.delayed.poll_seconds` | `JMAP_SUBMISSION_DELAYED_POLL_SECONDS` | 30 | how often a worker looks at the queue without a notification, and tries to take free partitions (1–3600); a new submission wakes the workers at once, and a worker sleeps only until the earliest entry it saw is due |
| `jmap.submission.delayed.sending_timeout_seconds` | `JMAP_SUBMISSION_DELAYED_SENDING_TIMEOUT_SECONDS` | 3600 | how long a submission may stay in `sending` before it counts as final with an unknown result and is not sent again (1800–86400); has to exceed the longest conversation with the relay (see below) |
| `jmap.submission.delayed.concurrency` | `JMAP_SUBMISSION_DELAYED_CONCURRENCY` | 4 | conversations with the relay one worker holds at once (1–64) |
| `jmap.submission.delayed.retry_attempts` | `JMAP_SUBMISSION_DELAYED_RETRY_ATTEMPTS` | 5 | attempts of a submission whose relay failed temporarily before the message was transferred, or whose account directory, registry or message copy could not be read (1–20); the backoff starts at one minute and doubles up to 30 minutes |

The longest conversation with the relay is about ten step timeouts
(`jmap.submission.timeout_seconds`, 30 by default: connection,
greeting, EHLO, STARTTLS, XCLIENT, MAIL, DATA, the upload), plus the
upload of the largest message at 256 KiB/s (`jmap.submission.max_size`,
else `jmap.max_size_upload`: 200 s for 50 MiB), plus ten minutes for the
relay's answer to the message — 1100 s with the defaults — and up to one
step timeout more per recipient when the relay is that slow for each.
`sending_timeout_seconds` has to stay above it; a server warns at
startup when it does not (counting no recipients). A conversation that
outlasts it is reported as unknown, never sent twice.

### Nauthilus client (`nauthilus.timeout`, `nauthilus.http_max_idle_per_host`)

| Key | Env | Default | Meaning |
| --- | --- | --- | --- |
| `nauthilus.timeout` | `NAUTHILUS_TIMEOUT` | 10 | seconds one request may take: gRPC and HTTP calls to Nauthilus, the token introspection and the OIDC token endpoint (1–300) |
| `nauthilus.http_max_idle_per_host` | `NAUTHILUS_HTTP_MAX_IDLE_PER_HOST` | 32 | idle connections per host the one HTTP client keeps that the JSON API, the introspection and the token endpoint share (1–1024) |
| `nauthilus.max_wait` | `NAUTHILUS_MAX_WAIT` | 5 | the longest `Auth-Wait` (seconds) honoured after a refusal over HTTP |

A backchannel token Nauthilus refuses is renewed and the request sent
once more, but only when the time left of the login allows a second
attempt as long as the first. A login as a whole is bounded to 15 s in
IMAP, POP3 and ManageSieve.

### Auth cache (`auth`)

The cache sits in front of Nauthilus only (`server.auth_backend:
nauthilus`; the static test backend is never cached) and behind the
master-user check: `MasterUser{Cache{Nauthilus}}`. The master password is
never cached, the lookup of the master-user login's target is.

```yaml
auth:
  cache:
    ttl: 30                  # seconds; 0 switches the whole cache off
    negative_ttl: 0          # refusals of identical credentials; 0 = not cached
    max_entries: 10000       # per operation (logins, lookups)
    key_client_ip: true      # the client IP is part of a login's key
    lookup_ttl: 30
    lookup_negative_ttl: 5   # at most lookup_ttl; ignored with lookup_ttl 0
    leader_budget: 0         # seconds for one shared backend call; 0 = 2 × nauthilus.timeout
```

| Key | Env | Default | Range | Meaning |
| --- | --- | --- | --- | --- |
| `auth.cache.ttl` | `AUTH_CACHE_TTL` | 30 | 0–600 | how long a successful login is answered from memory, from when it was stored; a bearer token introspected with an `exp` ends there when that comes first. `0` switches off the cache altogether, logins and lookups — the emergency switch |
| `auth.cache.negative_ttl` | `AUTH_CACHE_NEGATIVE_TTL` | 0 | 0–10 | how long a refused login is answered from memory, for exactly the same credentials, protocol, TLS state and address; it stops a client that loops with a stale password. A temporary failure is never cached |
| `auth.cache.max_entries` | `AUTH_CACHE_MAX_ENTRIES` | 10000 | > 0 | entries of logins and of lookups, each; the least recently used go first |
| `auth.cache.key_client_ip` | `AUTH_CACHE_KEY_CLIENT_IP` | true | | the client's IP address is part of a login's key, so that address rules in Nauthilus see every address. Off, one entry serves an account from every address |
| `auth.cache.lookup_ttl` | `AUTH_CACHE_LOOKUP_TTL` | 30 | 0–600 | how long a lookup that found an account (LMTP recipient, master-user target, JMAP identity, admin API, push worker) is answered from memory; `0` caches no lookup |
| `auth.cache.lookup_negative_ttl` | `AUTH_CACHE_LOOKUP_NEGATIVE_TTL` | 5 | 0–`lookup_ttl` | how long a lookup that found no account is answered from memory; keep it at or below the Nauthilus LDAP negative cache (20 s). Ignored, and no unknown name cached, with `lookup_ttl` 0 |
| `auth.cache.leader_budget` | `AUTH_CACHE_LEADER_BUDGET` | 0 | 0–600 | seconds one backend call made for every waiting caller may take in all (a bearer login is up to three requests: introspection, identity lookup, user name check); `0` takes twice `nauthilus.timeout` |

What a login's entry is found by: an HMAC-SHA256, under a random key
that lives in the process only, over the mechanism, the user name, the
secret (password or token), the protocol, the TLS state, the local
address and port Nauthilus is told and, with `key_client_ip`, the
client's address. The login attempt ordinal is not part of it; PLAIN
and LOGIN with the same password are two entries. Results are copied
on the way in and out. A lookup's entry is found by the normalised name (without angle brackets, the domain in
lower case) and the protocol: Nauthilus picks its search per protocol,
and the `jmap` search returns more attributes.

Identical requests that arrive together share one backend call: the
first starts it on a context of its own, bounded by `leader_budget`
and carrying its trace span, and every caller, the first included, stops
waiting when its own context ends; the call goes on and its answer is
stored for the next. An `Auth-Wait` Nauthilus asks for over HTTP is
waited out once, by that call, and every caller waits with it. An answer
that a flush overtook while it was on its way is not stored, and a
caller that comes after a flush starts a call of its own.

Lookups of the admin API (diagnosis, imports, deliveries) go past a
cached "unknown" and store a found account, so an account created a
moment ago is seen at once.

Entries are flushed with `shardpostctl auth flush [<account>]` on every
server (see [Operations: Auth cache](operations/README.md#auth-cache),
with what each change in LDAP costs until it takes effect) and by a
restart. The metrics are `imapceph_auth_cache_*` and
`imapceph_auth_backend_seconds` ([Observability](observability.md)).
