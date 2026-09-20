# Admin API, shardpostctl, and local delivery (LDA)

The core of [stage 8](development/admin-stage.md), pulled forward: a
versioned OpenAPI contract, admin listeners with identity and roles, an
audit log, and local delivery. Jobs, plan/apply, and the admin views
follow in sub-packages 8a through 8c.

## Contract and code generation

- Contract: [`api/admin/openapi.yaml`](../api/admin/openapi.yaml), path `/admin/v1`.
- Generated with `oapi-codegen` v2.8.0 (models, `net/http` server
  interface, client) into `internal/adminapi/api.gen.go`; configuration in
  `api/admin/oapi-codegen.yaml`. Hand-written client helpers for the
  socket and for mTLS live in `internal/adminapi/client.go`.
- `make generate` regenerates the code, `make check-generate` fails if the
  checked-in code does not match the contract. The generator runs in
  module mode (network access); the runtime library
  `github.com/oapi-codegen/runtime` is vendored.

## Listeners and identity

Both listeners are off unless configured, and both are separate from the
mail and metrics ports. Settings that have no effect are rejected:
`socket` and `socket_roles` only together, `addr` only with `tls_cert`,
`tls_key`, `client_ca` and at least one entry in `clients`, TLS settings
not without `addr`.

| Listener | Identity | Roles |
| --- | --- | --- |
| Unix socket (`admin.socket`, mode 0600) | peer UID via `SO_PEERCRED`; only the UID of the server process is allowed | `admin.socket_roles` |
| TCP with mTLS (`admin.addr`) | verified client certificate; subject CN or DNS/URI SAN (case-insensitive) | `admin.clients[].roles` |

The socket is reachable only inside the pod, so it is the same trust
boundary as `extprograms`. Being on the network is never an authorization
by itself. For mTLS, `admin.tls_cert`, `admin.tls_key` and
`admin.client_ca` are re-read when the files change (checked at most every
10 s). A rotation by cert-manager therefore takes effect without a
restart. A broken new version is logged, and until it is fixed the old one
stays in use. Client certificates need `extKeyUsage clientAuth`.

Roles: `diagnose` (version, server status and read-only diagnosis), `deliver` (delivery with quota enforcement),
`deliver:noenforce` (additionally required for `enforce_quota=false`),
`import` (account import), `trace` (protocol trace of an account, see
below), `provision` (create, rename and subscribe mailboxes; change
ACLs), `repair` (maintenance jobs that repair; retry learning jobs),
`delete` (permanently delete a mailbox, messages and an account),
`content` (read message content: search, headers, export, Sieve scripts)
and `support` (flags, copy/move, change and activate Sieve scripts) and
`export` (export whole accounts as an archive; the restore runs through
`import`). `content` is deliberately separate from `diagnose`: diagnosis
sees metadata, but no content.

```yaml
admin:
  socket: /run/shardpost/admin.sock
  socket_roles: [diagnose, deliver, "deliver:noenforce"]
  addr: ":9443"                     # optional
  tls_cert: /admin-tls/tls.crt
  tls_key: /admin-tls/tls.key
  client_ca: /admin-tls/ca.crt
  clients:
    - identity: ops.shardpost.internal
      roles: [diagnose]
    - identity: support.example.org
      roles: [diagnose, trace]
      accounts: ["*@example.org"]      # optional: account scope
```

`clients[].accounts` limits the accounts an mTLS client may address
(`path.Match` patterns against the lowercased account; empty = all). The
scope applies to every account-related operation: diagnosis, trace,
import, delivery (against the resolved account), for `account show` also
to the canonical account of an alias, and for `acl explain` to the
principal being explained. Clients with a scope may address accounts only
in lowercase (RADOS namespaces are case-sensitive). Outside the scope the
API answers `403 forbidden`.
Callers on the Unix socket are not limited.

Environment: `ADMIN_SOCKET`, `ADMIN_ADDR`, `ADMIN_TLS_CERT`, `ADMIN_TLS_KEY`,
`ADMIN_CLIENT_CA`; roles and clients only via YAML.

Every request produces a log line `admin request` (`event=admin_request`)
with principal, transport, method, path, status, error code, duration and
the operation's domain fields (for a delivery: recipient, sender, account,
options, number of stored copies), but without message content or
secrets. Rejected mTLS clients appear with the certificate identity they
presented, rejected socket callers with their UID.
Metric: `shardpost_admin_requests_total{operation,code}`.

## Local delivery

`POST /admin/v1/deliveries` delivers a message (`message/rfc822`) to a
recipient, like `dovecot-lda`. Recipient resolution works as it does for
LMTP (aliases, canonical account, userdb attributes). Sieve runs by
default, including redirect, vacation and notify. LMTP and LDA share the
same delivery path (`internal/delivery`). The server prepends
`Received: from <transport> <principal> by <lmtp_hostname> with LDA` to
the message. Recipient and sender must not contain control characters or
spaces and no further angle brackets. Resolution (10 s) and delivery
(5 min) run decoupled from the HTTP request: if the client aborts, the
delivery still runs to completion.

| Parameter | Meaning |
| --- | --- |
| `recipient` | recipient (address or account), required |
| `from` | envelope sender, empty = null sender |
| `mailbox` | straight into this existing mailbox, without Sieve |
| `sieve=false` | without the user script, into INBOX |
| `enforce_quota=false` | store even above the limit (usage is still counted); role `deliver:noenforce` |

Error codes: `bad_request` 400, `unauthenticated` 401, `forbidden` 403,
`no_such_user`/`no_such_mailbox` 404, `too_large` 413, `rejected` 422
(Sieve reject), `over_quota` 507, `temporary` 503, `internal` 500.

## shardpostctl

In the image at `/usr/local/bin/shardpostctl`. The default is the socket
`/run/shardpost/admin.sock` (`-socket`, `SHARDPOST_ADMIN_SOCKET`). With
`-url https://host:port -cert -key -ca` (or `SHARDPOST_ADMIN_*`) it goes
over mTLS.

```sh
shardpostctl version
shardpostctl deliver -d user@example.org -f postmaster@example.org < mail.eml
shardpostctl deliver -d user@example.org --no-enforce-quota --json < warning.eml
```

Exit codes of `deliver` (sysexits): 0 ok, 64 usage, 67 unknown recipient
or mailbox, 69 rejected or too large, 70 unexpected response, 73 over
quota, 75 temporary (try again), 77 not permitted.

## Read-only diagnosis (stage 8a)

All operations only read (role `diagnose`). A test runs every one of them
against a store that fails on every write operation.

```sh
shardpostctl server status                   # version, pod, start, config fingerprint, store
shardpostctl account show user@example.org   # Nauthilus resolution, store, quota, import lock, trace
shardpostctl mailbox list [-limit 100] [-after NAME] [-all] user@example.org
shardpostctl mailbox show user@example.org INBOX            # name or mailbox ID
shardpostctl acl list user@example.org Shared
shardpostctl acl explain user@example.org Shared kollege@example.org
shardpostctl quota show user@example.org
shardpostctl shared list kollege@example.org
```

Every command supports `--json`; flags may also follow the arguments.
Exit codes as for `deliver`: 67 unknown account/mailbox, 64 usage (an
alias instead of the canonical account counts as usage too), 77 missing
role or scope.

| API | Contents | Cost |
| --- | --- | --- |
| `GET /admin/v1/server` | version, instance (pod), start time, `config_fingerprint`, store ping | one ping |
| `GET /admin/v1/accounts/{account}` | Nauthilus lookup (`canonical`, `alias` with the canonical account, `unknown`, `unavailable`), store present, number of mailboxes, quota, import lock, trace | registry, quota |
| `GET …/mailboxes?after=&limit=&name=` | mailboxes in name order: ID, UIDVALIDITY, UIDNEXT, HIGHESTMODSEQ, shard size, subscription, special-use, number of ACL entries; `next` for the following page | one registry excerpt, one header per mailbox |
| `GET …/mailboxes/{id}` | additionally messages, unseen, marked deleted, size, shards, start of the log (QRESYNC horizon), keywords | reads all index shards of the mailbox |
| `GET …/mailboxes/{id}/acl` | explicit entries and ACL generation (the owner implicitly has all rights) | registry |
| `GET …/mailboxes/{id}/acl/explain?principal=` | the principal's rights (the same function IMAP uses), the pointer in their registry and findings in plain text | two registries |
| `GET …/quota` | usage and limit (0 = unlimited) | quota counter |
| `GET …/shared` | the account's share pointers (`Other Users/…`) | registry |

`account show` also accepts aliases and then names the canonical account;
the store fields describe the (usually empty) namespace under the name
that was given. All other operations require the canonical account,
because the data lives underneath it. Mailboxes are addressed in the URL
by their stable ID. `shardpostctl` resolves names itself (`?name=`, a
single registry key); resolving an ID reads the account's entire
registry. A page ends after `limit` registry entries have been read,
including unreadable ones; a broken entry neither shifts nor ends the
list.

`acl explain` answers "Why doesn't X see the share?" with findings:

- An alias instead of the canonical account; an ACL entry under the alias never takes effect.
- No rights, so the mailbox does not exist for the principal (`NONEXISTENT`).
- Rights without `l`: not listed, but addressable by name.
- Rights only through `anyone`: no pointers are written for that, so LIST
  does not show the mailbox.
- A missing pointer (LIST does not show the mailbox; a SETACL for the
  principal writes it) or a pointer without the `l` right (LIST hides it,
  the pointer stays until the mailbox is deleted or a SETACL withdraws
  it). The generation of the pointer is informational only: SETACLs for
  other identifiers raise the mailbox's generation without touching it.

`config_fingerprint` is a SHA-256 over the effective configuration without
the values of the fields marked `secret:"true"`; a test requires this
marker for every new field whose name looks like a password, secret or
token. Identical fingerprints mean the pods have identical settings.

Job status comes from the maintenance jobs (stage 8c, below).

## Provisioning and permissions (stage 8b)

Role `provision`. The Admin API does not set quota limits: LDAP
(`rnsMSQuota`) is authoritative, taken over at login (decision of
2026-09-19).

```sh
shardpostctl mailbox create [-special-use '\Archive'] user@example.org "Projekte/2026"
shardpostctl mailbox rename [-if-match '"m-…"'] user@example.org "Projekte/2026" "Archiv/2026"
shardpostctl mailbox subscribe|unsubscribe user@example.org "Archiv/2026"
shardpostctl acl set [-mode replace|add|remove] [-if-match '"acl-3"'] user@example.org Shared kollege@example.org lr
shardpostctl acl delete user@example.org Shared kollege@example.org
```

| API | Semantics |
| --- | --- |
| `POST …/mailboxes` `{name, special_use}` | 201 created. If the mailbox already exists with the same special-use: 200 (a retry after a lost response), otherwise 409. Special-use is stored in the standard spelling, as with IMAP CREATE (`\All`, `\Archive`, `\Drafts`, `\Flagged`, `\Important`, `\Junk`, `\Sent`, `\Trash`); other attributes are rejected by the API. |
| `PATCH …/mailboxes/{id}` `{name?, subscribed?}` | Rename including child mailboxes (not INBOX; an occupied target gives 409), set the subscription; the same name is a no-op. |
| `PUT …/mailboxes/{id}/acl/{identifier}` `{rights, mode}` | `replace` sets the rights, `add`/`remove` change them relative to the current ones; the server computes and writes in one versioned step, so concurrent changes are not lost. |
| `DELETE …/mailboxes/{id}/acl/{identifier}` | Remove the entry. |

- **Versions:** `GET …/mailboxes/{id}` and changes return the mailbox ETag
  (`"m-…"`), `GET …/acl` and ACL changes the ACL ETag
  (`"acl-<generation>"`). `If-Match` makes a change conditional; if the
  version does not match, the API answers `412 precondition_failed`.
  The ACL version is checked in the same write step. For the mailbox
  (rename, subscription) it is checked immediately before the change; the
  mailbox ETag also covers the ACL, so an ACL change invalidates it as
  well. A malformed `If-Match` gives 400.
- **Identifiers** are canonical accounts (Nauthilus) or `anyone`: aliases
  are rejected together with the canonical name, unknown accounts with
  404, the owner with 400. The recipient's share pointer follows as it
  does for IMAP SETACL (there is none for `anyone`); a change with no
  effect writes none. If only the pointer write fails, the change still
  stands (the ACL is authoritative; a warning is logged, `acl explain`
  shows the missing pointer, and a `replace` with the same rights writes
  it afterwards).
- **Names** are stored by the server as they arrive (UTF-8). IMAP clients
  with `UTF8=ACCEPT` see the same names; older clients using modified
  UTF-7 send `&AOQ-…` forms and do not see admin-created names with
  umlauts under the same designation.
- **Idempotence** follows from the semantics: absolute values, mailboxes
  addressed by their stable ID. A repeated request after a lost response
  returns the same state. Send relative ACL changes with `If-Match` for
  that reason.
- Running IMAP sessions see the changes like those of another client:
  rights apply from the next command, new or renamed mailboxes at the
  next LIST.
- Exit codes of `shardpostctl`: 65 on a conflict or a stale version
  (409/412), otherwise as for diagnosis.

## Maintenance jobs (stage 8c)

Long-running and dangerous changes run as durable jobs. Every server runs
one runner; jobs live in the shared admin storage (namespace `~admin` in
the index pool, one object per job and per plan) and survive restarts.

```sh
shardpostctl check -wait user@example.org                  # check without repair (role diagnose)
shardpostctl plan scrub -classes refs,orphans user@example.org
shardpostctl plan quota_recalc user@example.org
shardpostctl plan acl_reconcile user@example.org
shardpostctl plan fts_rebuild user@example.org
shardpostctl plan -mailbox "Archiv/2019" mailbox_delete user@example.org
shardpostctl plan purge user@example.org
shardpostctl apply -wait <plan-id>
shardpostctl job list -account user@example.org
shardpostctl job show|wait|cancel <job-id>
```

**Plan → apply.** `plan` computes a dry run and shows the target set and
the effect (findings, counter difference, missing pointers, affected
mailboxes, messages and bytes). `apply` starts the plan as a job, once and
before it expires (`jobs.plan_ttl_seconds`). When a mailbox is deleted and
on purge, the job compares the state (IDs, UIDVALIDITY, UIDNEXT,
HIGHESTMODSEQ, and on purge the Sieve scripts as well) with the plan
before the first write; if it differs, the job ends without a change and
reports "the account changed since the plan". A share reconcile applies
only the repairs that were planned and are still needed.

| Kind | Role | Account lock | Effect |
| --- | --- | --- | --- |
| `index_check` (no plan) | `diagnose` | no | scrubber without repair, findings as the result |
| `scrub` | `repair` | no | scrubber with the chosen classes: `deliveries`, `moves`, `retired`, `uncovered`, `entries`, `refs`, `orphans`, `unknown`, `quota` (all of them if none are given) |
| `quota_recalc` | `repair` | yes | recount the usage from the index and set it |
| `acl_reconcile` | `repair` | no | write missing pointers for recipients, remove own pointers that carry no rights |
| `fts_rebuild` | `repair` | yes | delete the full-text index and rebuild it from all messages |
| `mailbox_delete` | `delete` | yes | delete the mailbox including its child mailboxes (not INBOX) |
| `expunge` | `delete` | no | permanently remove messages of a mailbox: `uids` or `deleted: true` (all with `\Deleted`) |
| `purge` | `delete` | yes | withdraw shares in both directions, delete learning jobs and all objects of the account |

- **Account lock** (maintenance mode, decision of 2026-09-19): the job
  holds the lock the import uses as well. While it is held, IMAP and POP3
  answer "Mailbox maintenance in progress", LMTP answers `451 4.2.1`.
  IMAP/POP3 sessions that are already open check the lock at most every 5
  seconds at the start of a command and then disconnect with
  `BYE [UNAVAILABLE]` or `-ERR [SYS/TEMP]` respectively. After taking the
  lock, the job waits `jobs.settle_seconds` (default 10) before it writes.
  A session that sends no command during that time (an IDLE client, for
  example) writes nothing either. Other account jobs, imports and the
  periodic scrub stay out. If the account is already locked when the job
  starts (an import, another job), the job waits in `running` with the
  phase "waiting for the account lock" and retries once per lease period,
  at most until `jobs.plan_ttl_seconds` after it was created. After that
  it ends `failed`. If a job loses the lock, it ends with "maintenance
  lock lost".
- **Ownership:** a job belongs to the server that took it over (a lease
  with a generation, `jobs.lease_seconds`). Before every write step the job
  checks its lease (fencing). If the server dies, another one takes over
  once the lease has expired and continues from the last checkpoint. On an
  orderly shutdown the server gives up running jobs immediately.
  `fts_rebuild` has no checkpoint and starts over after a takeover.
  Leases compare the clocks of different servers, so those clocks have to
  be kept in sync with NTP. A deviation well below `jobs.lease_seconds` is
  harmless.
- **Cancelling** (`job cancel`): a waiting job ends immediately, a running
  one at the next safe point. What has already been done is not rolled back.
- **States:** `queued`, `running`, `succeeded`, `partially_failed`,
  `failed`, `cancel_requested`, `cancelled`. `apply -wait` and `job wait`
  exit with 0 (success), 1 (partial failure) or 69 (failed, cancelled).
- **Idempotence:** `apply -idempotency-key K`, or the header
  `Idempotency-Key`, returns the same job on a retry (200). With a
  different request the key is rejected (409). Keys are per caller.
- **Scope:** plans that change other accounts (share reconcile, purge)
  need those accounts in the client's scope, both when the plan is created
  and again at `apply`. Clients with a scope see jobs only when an account
  is given. Jobs and plans outside their scope do not exist for them (404).
- Without the job system (`jobs.enabled: false`) the job endpoints answer
  503 `temporary`.
- **purge** removes only the data in the store. The identity (LDAP)
  belongs to its owner. If the account logs in again afterwards, ShardPost
  creates an empty INBOX.
- Finished jobs and plans stay retrievable for `jobs.retention_days`.

## Support tools (stage 8d)

Tools for support work on individual messages, Sieve scripts and the
learning queue.

```sh
shardpostctl messages alice@example.org INBOX                       # metadata (diagnose)
shardpostctl search -subject Rechnung -since 2026-09-01 alice@example.org   # (content)
shardpostctl message show alice@example.org INBOX 42                # envelope + headers (content)
shardpostctl message export alice@example.org INBOX 42 > 42.eml     # (content)
shardpostctl flags -add '\Seen' -remove '$Junk' alice@example.org INBOX 42,43   # (support)
shardpostctl move alice@example.org Junk 42 INBOX                   # copy|move (support)
shardpostctl plan -mailbox Trash -deleted expunge alice@example.org # then apply (delete)
shardpostctl sieve list|show alice@example.org [name]
shardpostctl sieve put -activate alice@example.org main < main.sieve  # (support)
shardpostctl sieve check -message INBOX:1789839043:42 alice@example.org
shardpostctl learning list -state dead
shardpostctl learning retry -confirm-repeat <id>                   # (repair)
```

- **Stable targets:** messages are addressed by mailbox ID, UIDVALIDITY
  and UID. If the UIDVALIDITY does not match (the mailbox was recreated),
  the API answers 412. Without `-uidvalidity`, shardpostctl uses the
  current one. For strict identity, pass the value from an earlier
  listing. UIDs that no longer exist are reported as `missing` in the
  response. Copy/move returns the new UID for each message (as UIDPLUS
  does).
- **Expunge** runs as a planned job. The plan records the UID and GUID of
  every message. On apply, only what still carries the same GUID is
  removed, and with `deleted` only what is still `\Deleted`. A changed
  UIDVALIDITY aborts it.
- **Search** is bounded: at most `admin.search_scan_limit` messages per
  request and 200 hits per page. `next` continues (in mailbox ID order).
  `from`, `to` (Cc as well) and `subject` are substrings in the envelope,
  `text` uses the full-text index. If the index is missing (never built,
  or during `fts_rebuild`), `text` finds nothing.
- **Expunge plans** cover at most 40,000 messages. Handle larger sets in
  several plans (`-uids`).
- **Privacy:** for searches the audit log names only the criteria, never
  their values. For messages and scripts it names the identity and the
  size, never the content.
- **Sieve** is compiled before it is stored, together with the account's
  includes. A faulty script is not stored (400). Versions go through ETags
  (`If-Match`, `If-None-Match: *` to create only). The active script
  cannot be deleted.
- **Checking Sieve without side effects:** `sieve check` compiles the
  script and, on request, runs it against a stored message (role
  `content`) or a text supplied with the request (`diagnose`). Nothing is
  actually carried out: no redirect, no vacation reply, no notification,
  no external program (`execute`/`filter` count as having run with exit 0,
  without running), no duplicate or vacation record, no filing. The
  response lists what would happen and, under `suppressed`, every
  suppressed side effect. With a script and a text supplied in the request
  (role `diagnose`), the store tests (`mailboxexists`, `mboxmetadata`,
  `specialuse_exists`) are active. That way `diagnose` learns no more than
  it would through `mailbox list`. This is a deliberate decision.
- **Learning queue:** `learning list|show` shows jobs with their state
  (`pending`, `dead`, `delivered`), program, attempts and next due time.
  `retry` picks up dead jobs only. Learning programs are not idempotent:
  if the program may already have received the message (attempts > 0),
  `-confirm-repeat` is required. A job whose message was never committed
  (no evidence) cannot be retried.
- Clients with an account scope see learning jobs only when an account is
  given. Jobs of other accounts do not exist for them (404).

## Export and restore (stage 8e)

An account can be backed up as a versioned archive and restored through
the account import, with the same IMAP identities.

```sh
shardpostctl export -o alice.tar alice@example.org          # role export, verifies the archive after writing
shardpostctl archive verify alice.tar                       # offline, without a server
shardpostctl restore alice.tar                              # role import, into the exported account
shardpostctl restore -account restore-test@example.org alice.tar   # isolated restore
```

**Format** `shardpost-account-export`, version 1: a tar stream with

| Entry | Contents |
| --- | --- |
| `shardpost-export.json` | format, version, account, timestamp, server version |
| `mailboxes/<id>/mailbox.json` | ID, name, UIDVALIDITY, UIDNEXT, HIGHESTMODSEQ, subscription, special-use, ACL, annotations (METADATA) |
| `mailboxes/<id>/<uid>.json` | UID, GUID, flags, MODSEQ, INTERNALDATE, SAVEDATE, POP3 UIDL, size, SHA-256 |
| `mailboxes/<id>/<uid>.eml` | the message as stored |
| `sieve/<name>.sieve` | one Sieve script |
| `manifest.json` | complete yes/no, counters, active script, skipped messages, size and SHA-256 of every entry |

The full-text index is not included; it is rebuilt from the messages.

- **Consistency:** the export does not lock the account. Every mailbox is
  read in the state it had at export time. Messages that have been deleted
  in the meantime appear in the manifest under `skipped`, deleted
  mailboxes likewise under `skipped_mailboxes`. If an export fails part
  way through, the archive ends with `complete: false` and is rejected by
  both verification and restore. For an exact point in time, set
  `users hold` in the Director beforehand.
- **Verification:** every message is checked against the size and SHA-256
  in its description, and at the end every entry is checked against the
  manifest. `restore` reads the whole archive once beforehand and does not
  even start restoring a damaged or incomplete archive (exit 65).
- **Restore** uses the account import (a session holding the account
  lock): mailboxes with UIDVALIDITY and ID, messages with UIDs, flags,
  MODSEQ, dates and POP3 UIDL, then UIDNEXT/HIGHESTMODSEQ per mailbox
  (finalize), ACL and annotations, and finally the Sieve scripts. Every
  mailbox is checked against the target's inventory (`verified`). A
  restore can be repeated; messages that are already present are skipped.
  The target has to be empty or in the same state. A mailbox with a
  different UIDVALIDITY that holds messages is a conflict.
- **Isolated restore** (a different target account, for restore evidence
  for example): ACLs are not carried over (they would give real accounts
  rights on the copy) and no Sieve script is activated (no vacation
  replies or forwards out of the copy). `-acl` and `-activate-sieve` turn
  that on explicitly.
- The export keeps moving the admin listener's write deadline forward as
  it progresses and therefore runs through even for large accounts.
  `shardpostctl export` has no overall timeout. `-timeout` bounds only the
  wait for the response to start and each pause without data. An aborted
  export leaves no file behind.
- **Not in the archive:** subscriptions to mailboxes that do not exist,
  server annotations (METADATA without a mailbox), the trace switch and
  the quota limit (which comes from LDAP). The full-text index is rebuilt
  from the messages.
- Exit codes: 0 fine, 1 restore partially failed (run it again), 65
  archive damaged or incomplete, 64 usage, 69 export failed, 73 target
  file exists.

## Public folders

A public folder belongs to no account: an archive, an announcement
folder, a team inbox that outlives whoever created it. Clients see them
under the prefix `Public/` (`server.public_prefix`), the store keeps them
in the reserved namespace `~public`.

```sh
shardpostctl mailbox create ~public "Announcements"
shardpostctl acl set ~public "Announcements" anyone lr
shardpostctl acl set -mode add ~public "Announcements" team@example.org lrswipkxte
shardpostctl acl explain ~public "Announcements" user@example.org
shardpostctl mailbox list -all ~public
```

- **Administration** uses the same operations as an account: the API
  accepts `~public` wherever an account is expected, without a user
  database lookup. `account show ~public` reports the lookup as `public`.
  Delivery is refused: a public folder has no address of its own.
- **Rights:** there is no owner and therefore no implicit right. Only ACL
  entries grant access; `anyone` covers every authenticated account, an
  entry for one account wins over it. Without the `l` right a folder does
  not exist for that account (`NONEXISTENT`, not `NOPERM`), exactly as
  for shared folders. Unlike Dovecot, the rights are data in the store:
  `acl set` takes effect with the next command, without a reload.
- **Subscriptions** belong to the account, not to the folder, and are
  kept by mailbox id: a folder that is renamed stays subscribed.
- **Quota:** the namespace counts its own usage and is bounded by
  `server.public_quota_bytes` and `server.public_quota_msgs` (0 =
  unlimited). A message in a public folder does not count against any
  account, which is what Dovecot does as well.
- **Backup:** `shardpostctl export -o public.tar ~public` and `restore`
  work like they do for an account. `acl_reconcile` is refused for
  `~public`: there are no pointers to reconcile.
- **Switching them off:** an empty `server.public_prefix` disables the
  namespace for clients (no entry in `NAMESPACE`, no extra reads per
  `LIST`); the folders stay in the store and remain administrable. While
  it is off, an account can create a mailbox of its own called
  `Public/...`; once the prefix is in use again, such a mailbox is
  shadowed by the public namespace. The same applies to an account that
  already had a mailbox named `Public` before the feature existed.

## Protocol trace for an account

To debug a problem with a particular client, a recording of the IMAP
protocol can be switched on for an account for a limited time (role
`trace`):

```sh
shardpostctl trace on -for 30m user@example.org   # without -for: trace.default_duration_seconds
shardpostctl trace status user@example.org
shardpostctl trace off user@example.org
```

API: `GET`/`PUT`/`DELETE /admin/v1/accounts/{account}/trace`, `PUT` with
an optional `{"duration_seconds": n}`. The switch lives as the registry
key `trace` in the Ceph account and therefore applies to every server in
the cluster, no matter which pod receives the request. It has to be set on
the canonical account (an alias is rejected together with the canonical
name, as it is for the import) and expires after
`trace.max_duration_seconds` at the latest.

Effect:

- New IMAP sessions of the account log every line in both directions from
  the successful login onwards as `event=protocol_trace` with `dir`
  (`C` client→server, `S` server→client), `line`, `session_id` and
  `user`. Lines over `trace.max_line_bytes` are truncated (`cut_bytes`).
- Literals (message content, APPEND data, fetched sections) appear only as
  `<literal N bytes>`, the text of `PREVIEW` as `"<masked>"`. Client lines
  and literals are reported by the IMAP parser, that is, exactly what the
  server processes; pipelining, rejected literals and read buffers do not
  shift that view. Server lines come from the data stream that was sent.
- Credentials never appear: the recording starts after the login and ends
  at `UNAUTHENTICATE`. A client line with `LOGIN` or `AUTHENTICATE` in the
  authenticated state is replaced by
  `<authentication command, trace stopped>` and stops the trace until the
  next login (any literals or SASL lines that follow do not appear
  either).
- Sessions that are already running are only picked up at their next
  login. Expiry takes effect immediately. Switching the trace off takes
  effect in running sessions at the next command, once
  `trace.recheck_seconds` have passed; a session in IDLE keeps being
  recorded until IDLE ends or the trace expires. If the trace is extended
  before it expires, a session that is already being recorded continues
  past the old end time.
- `protocol_trace_start` and `protocol_trace_stop` (with `reason`) mark the
  beginning and the end; the Admin API's audit log records who switched
  the trace on, when and until when.

The trace contains the account's subject lines, addresses, file names
(ENVELOPE, BODYSTRUCTURE), search terms and mailbox names. That is why it
is a role of its own, time-limited and intended only for debugging.
Reading it out, for example:

```sh
kubectl -n shardpost logs -l app.kubernetes.io/name=shardpost-server -c server --since=30m \
  | jq -r 'select(.event=="protocol_trace" and .user=="user@example.org") | "\(.time) \(.session_id[0:8]) \(.dir): \(.line)"'
```

## Cluster operation (mTLS)

The component `deploy/components/admin-mtls` (active in the
`croessner-nauthilus` overlay) provides the mTLS listener:

- A dedicated CA `shardpost-admin-ca` (cert-manager, bootstrapped via
  `shardpost-selfsigned`). It is deliberately separate from
  `shardpost-ca`, which issues server certificates for IMAP/LMTP; only
  certificates from this CA are accepted as admin clients.
- Server certificate `shardpost-admin-tls` for the service
  `shardpost-admin:8443` (pod port `admin`), mounted as a directory at
  `/secrets/admin-tls` (without `subPath`, so rotation needs no restart).
- Client certificate `shardpost-admin-operator` (CN = identity) with the
  role `diagnose`. Further clients each get their own `Certificate` with
  `usages: [client auth]` and an entry in `admin.clients`.
- NetworkPolicy `shardpost-admin-ingress`: port 8443 only for pods in the
  namespace `shardpost` with the label
  `shardpost.roessner-net.de/admin-client: "true"`.

Calling it from such a pod with the client secret mounted:

```sh
shardpostctl -url https://shardpost-admin.shardpost.svc:8443 \
  -cert /op/tls.crt -key /op/tls.key -ca /op/ca.crt version
```

Accepted on 2026-09-18: operator `version` 200, `deliver` without the role
403, a client of a foreign CA and a client without a certificate rejected
in the handshake, a pod without the label blocked by the NetworkPolicy;
audit lines with `transport=mtls` and the identity.

## Account import with preserved IMAP identities

The core of sub-package 8e ([stage 8](development/admin-stage.md)). An
account is taken over in such a way that IMAP clients only synchronize
incrementally after the cutover: UIDVALIDITY, UIDs with their gaps,
UIDNEXT, flags and keywords, INTERNALDATE/SAVEDATE and MODSEQ per message
are preserved, and HIGHESTMODSEQ becomes at least the source value.
imapsync cannot do this, because IMAP APPEND always assigns new UIDs.

### Semantics

- **Lock:** `POST /admin/v1/accounts/{account}/import` (role `import`)
  takes the account-wide import lock for one session (header
  `X-Import-Session` on all further calls; every call extends it,
  `admin.import.lock_ttl_seconds`). While it is held, IMAP, POP3,
  ManageSieve, LMTP (`451 4.2.1`) and local delivery reject the account
  temporarily. Only the canonical account from the userdb is accepted.
- **Mailboxes:** `PUT .../import/mailboxes` creates the mailbox with the
  UIDVALIDITY of the source (with the Dovecot mailbox GUID as its stable
  ID where available) or takes over an existing one with the same
  UIDVALIDITY. An empty mailbox with a different UIDVALIDITY (an INBOX
  from the first login, say) is replaced, a non-empty one is a conflict
  (`409 conflict`). If the source has renamed a mailbox, the import
  follows it through the stable ID. New mailboxes get
  `admin.import.shard_size` as their shard size.
- **Messages:** `POST .../mailboxes/{id}/messages` with
  `multipart/mixed`; every part is a `message/rfc822` with the identity as
  JSON in the part header `X-Import-Meta` (`uid`, `modseq`, `flags`,
  `internal_date`, `save_date`, `guid`, `pop3_uidl`). UIDs ascending. A
  UID whose message has the same size is skipped (resumption), one with a
  different size is a conflict. Quota is counted, not enforced. The same
  source GUID in several mailboxes shares one blob.
- **Deltas:** `POST .../changes` sets flags with the source MODSEQ and
  removes UIDs the source no longer has. `GET .../messages` returns the
  inventory page by page (`after`, `limit`).
- **Finalization per mailbox:** `POST .../finalize` sets UIDNEXT and
  HIGHESTMODSEQ to at least the source values, the \Recent mark to
  UIDNEXT and the start of the change log past HIGHESTMODSEQ. A client
  that comes back with an older source state via QRESYNC/CONDSTORE then
  gets the complete answer (all newer entries, VANISHED for all gaps), one
  with the current state gets nothing.
- **Sieve and ACL:** `PUT .../import/sieve` replaces the scripts and the
  active script; all scripts are compiled beforehand as they are in
  ManageSieve (`422 rejected` if one does not compile; nothing changes
  then). `PUT .../mailboxes/{id}/acl` replaces the explicit ACL entries.
- **POP3:** with `-pop3-uidl-format` (Dovecot syntax 2.3 `%08Xu%08Xv` or
  2.4 `%{uid | hex(8)}`) the UIDL of the source is stored for every INBOX
  message; POP3 `UIDL` then returns it instead of the GUID. COPY/MOVE do
  not carry it over (it has to stay unique per maildrop).

### Migrator `shardpostctl migrate`

Reads the source over IMAP (master user, `UID FETCH ... MODSEQ X-GUID
SAVEDATE`) and ManageSieve (with a PROXY header where the load balancer
requires one), compares it with the target's inventory and sends only the
differences. Every run is repeatable and checks at the end, per mailbox,
that UIDs, sizes and flags match the source state that was read
(`verified`). Target mailboxes the source no longer has are removed
(`-no-prune` turns that off; INBOX always stays).

In the cluster it runs in the `import` sidecar of the server pod
(component `deploy/components/import-sidecar`) against the admin socket.
The source and the Sieve endpoint are set as `SHARDPOST_MIGRATE_*` in the
component, the master password is in the secret
`shardpost-migrate-source` (key `password`):

```sh
kubectl -n shardpost create secret generic shardpost-migrate-source \
  --from-literal=password="$(kubectl -n director get secret director-backend-master -o jsonpath='{.data.password}' | base64 -d)"
kubectl -n shardpost exec deploy/shardpost-server -c import -- \
  shardpostctl -timeout 10m migrate user@example.org
```

For long runs, detached with a log under `/work`:

```sh
kubectl -n shardpost exec deploy/shardpost-server -c import -- sh -c \
  'nohup shardpostctl -timeout 10m migrate --json user@example.org >/work/user.json 2>/work/user.log &'
```

Exit codes: 0 everything verified, 1 parts failed (run it again), 64
usage, 69 run aborted.

### Migration procedure with the Director

1. First copy while the account is still in service (still on mailstack):
   run `migrate` as often as needed until a run reports almost no changes.
2. Director: `users hold`, `users kick`.
3. Final `migrate` run (delta, Sieve, ACL); all mailboxes `verified`.
4. Pin in the Director/`director.lua` to `shardpost`,
   `users move --to-shard shardpost --strategy kick_existing`,
   `hold clear`.
5. Check with a client: same UIDVALIDITY, no full download.

As long as mailstack stays unchanged after the cutover, the way back is a
re-route; changes that arrived after that would have to be copied back
(with a full resync on the way back in that case).
