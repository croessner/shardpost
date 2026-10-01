# Server configuration

The server loads its configuration once, before it opens Ceph, authentication and the listeners. Libraries: Viper and go-playground/validator.

```sh
shardpost --config /etc/shardpost/server.yaml
shardpost --config /etc/shardpost/server.yaml config check
shardpost --config /etc/shardpost/server.yaml scrub
```

Alternatively, `SHARDPOST_CONFIG` determines the file path. `IMAP_CONFIG`
is kept as a fallback as long as `SHARDPOST_CONFIG` is not set. An
explicit `--config` takes precedence over both variables. An explicitly
empty `SHARDPOST_CONFIG` disables the old fallback. Precedence: built-in defaults, explicit YAML file, explicitly set legacy env variables. There is no implicit file lookup and no automatic reload. Changes in Kubernetes are applied through a controlled deployment rollout. Go runtime settings such as `GOMEMLIMIT` remain environment variables; additional standardized OTEL SDK options are still evaluated by the SDK.

The sections are `server`, `ceph`, `rook`, `tls`, `nauthilus`, `smtp`, `sieve`, `scrub`, `logging`, `telemetry`, `imapsieve`, `extprograms`, `quota`, `admin` and `jmap` (see [JMAP](jmap.md); without `jmap.addr` that front-end is not started). The complete field and env mapping is documented in `internal/config/config.go`, `internal/config/imapsieve.go`, `internal/config/extprograms.go`, `internal/config/quota.go`, `internal/config/admin.go` and `internal/config/jmap.go` (including `jmap.push`); `extprograms`, the learning rules, the quota rules and warnings as well as the admin roles and clients are configurable via YAML only (see `docs/imapsieve.md`, `docs/quota.md` and `docs/admin-api.md`).

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
| `jobs.poll_seconds` | 5 | interval between scans for new jobs |
| `jobs.lease_seconds` | 60 | how long a claim holds without renewal (another server may take over afterwards) |
| `jobs.retention_days` | 14 | retention of finished jobs and plans |
| `jobs.plan_ttl_seconds` | 3600 | how long a plan stays valid |
| `jobs.max_parallel` | 2 | concurrent jobs per server |
| `jobs.settle_seconds` | 10 | how long an exclusive job waits after setting the maintenance lock, so that running deliveries can finish and open IMAP/POP3 sessions notice the lock (they check it at most every 5 s when a command starts and disconnect with BYE) |

YAML only. For operating them see [Admin API: maintenance jobs](admin-api.md#maintenance-jobs-stage-8c).

### Support tools (`admin.search_scan_limit`)

| Key | Default | Meaning |
| --- | --- | --- |
| `admin.search_scan_limit` | 50000 | a search request examines at most this many messages (role `content`); a longer search resumes with its cursor |

YAML only. See [Admin API: support tools](admin-api.md#support-tools-stage-8d).

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
