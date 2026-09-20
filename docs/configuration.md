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

The sections are `server`, `ceph`, `rook`, `tls`, `nauthilus`, `smtp`, `sieve`, `scrub`, `logging`, `telemetry`, `imapsieve`, `extprograms`, `quota` and `admin`. The complete field and env mapping is documented in `internal/config/config.go`, `internal/config/imapsieve.go`, `internal/config/extprograms.go`, `internal/config/quota.go` and `internal/config/admin.go`; `extprograms`, the learning rules, the quota rules and warnings as well as the admin roles and clients are configurable via YAML only (see `docs/imapsieve.md`, `docs/quota.md` and `docs/admin-api.md`).

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
telemetry:
  protocol: http/protobuf
  endpoint: http://collector:4318
  sample_ratio: 0.1
```

Unknown keys, mistyped YAML values, invalid env numbers, invalid boolean values, contradictory TLS pairs and explicit secret files that are missing or read as empty cause a startup error. Env numbers are always interpreted as decimal. A secret file takes precedence over the corresponding direct secret value. Error messages never print values.

`config check` validates the configuration structure and reads the referenced secret files. It is no proof of connectivity to Ceph, Nauthilus, SMTP or the external programs; program files are checked when they are invoked. Certificates are loaded when the server starts afterwards. Without a file path, the existing env-based deployments remain usable.

## Client IP behind the Director

`server.proxy_protocol_trusted_cidrs` controls whether client addresses from
PROXY protocol v1/v2 are adopted ahead of the actual mail protocol/TLS. For
this to work, the Director must also set `haproxy.enabled: true` for the
backends in question. The setting applies jointly to IMAP, POP3, ManageSieve
and LMTP.

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

With an empty prefix no client can reach a shared mailbox, although the
permissions remain in the store and the admin API still shows them.

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

### Public folders

| Key | Default | Meaning |
| --- | --- | --- |
| `server.public_prefix` | `Public/` | client-visible prefix of the public namespace (must end with the separator); empty switches public folders off entirely |
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
