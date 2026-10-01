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
ACLs), `repair` (maintenance jobs that repair; retry learning jobs;
flush the auth cache, see below),
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
    - identity: fleet.shardpost.internal
      roles: [diagnose, repair]
      all_accounts: true               # optional: scope of all accounts
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

`clients[].all_accounts` grants the **scope of all accounts**: listing the
account directory (`GET /admin/v1/accounts`) and planning, applying and
cancelling batches over every account (`POST /admin/v1/plans`, see
[Batches over all accounts](#batches-over-all-accounts)). Each operation
still needs its role. A client without the flag cannot do either, even
without an account scope; the flag and `accounts` exclude each other.
Socket callers always have it.

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
the message, and above it `Return-Path: <from>` and
`Delivered-To: <recipient>` (removing a Return-Path the message brought
along; `server.lmtp_return_path`, `server.lmtp_delivered_to`, see
[Sieve during delivery](imapsieve.md#delivery-headers)). A Sieve
`ereject` answers `422` (`rejected`); a `reject` sends a rejection
message to the sender and answers `200` with nothing stored and
`disposition: rejected`, unless the sender is the null sender (then the
message is discarded, `disposition: discarded`), the mail is automatic or
list mail, no relay is configured, the relay refuses the rejection
message or `sieve_reject_mode=protocol` is set (then `422` as well; see
[Sieve during delivery](imapsieve.md#reject-and-ereject)). Recipient and sender must not contain control characters or
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

The result names the account, the stored copies (`stored`: mailbox,
UID, GUID, size) and the `disposition`: `stored`, `discarded` (Sieve
discarded the message, or rejected mail from the null sender) or
`rejected` (Sieve rejected it and a rejection message went to the
sender); in the last two cases `stored` is empty.

Error codes: `bad_request` 400, `unauthenticated` 401, `forbidden` 403,
`no_such_user`/`no_such_mailbox` 404, `too_large` 413, `rejected` 422
(Sieve ereject, or a reject that sends no rejection message), `over_quota` 507, `limit_exceeded` 409 (an account limit, see
[configuration](configuration.md#account-limits-limits); every API that
changes an account answers it the same way), `temporary` 503, `internal` 500.

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
| `GET …/shared` | the account's share pointers (the shared namespace) | registry |

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
| `POST …/mailboxes` `{name, special_use}` | 201 created. If the mailbox already exists with the same special-use: 200 (a retry after a lost response), otherwise 409. Beyond `limits.mailboxes` or `limits.mailbox_names`: 409 `limit_exceeded`. Special-use is stored in the standard spelling, as with IMAP CREATE (`\All`, `\Archive`, `\Drafts`, `\Flagged`, `\Important`, `\Junk`, `\Sent`, `\Trash`); other attributes are rejected by the API. |
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
  404, the owner with 400. An identifier that is not UTF-8 or holds a
  `/` or a NUL is refused with 400, by the store as well: the
  recipient's share pointer is keyed `<owner>/<mailbox>`. IMAP `SETACL`
  and `DELETEACL` answer `BAD` for such an identifier. The recipient's share pointer follows as it
  does for IMAP SETACL (there is none for `anyone`); a change with no
  effect writes none. If only the pointer write fails, the change still
  stands (the ACL is authoritative; a warning is logged, `acl explain`
  shows the missing pointer, and a `replace` with the same rights writes
  it afterwards).
- **Share notifications**: a change that alters the rights of an
  account leaves a notification in that account, as an IMAP `SETACL`
  does (RFC 9670 3; see
  [JMAP: Share notifications](jmap.md#share-notifications)). Its
  `changedBy` is named `Administrator`, with no address and no
  principal: the admin client is nobody the account could look up. A
  change of `anyone` leaves none. The notification is best effort and
  never fails the request.
- **Names** are UTF-8, here and in the store. IMAP clients that enabled
  `UTF8=ACCEPT` or `IMAP4rev2` see the same names; older clients see
  and send them in modified UTF-7 (`Entwürfe` is `Entw&APw-rfe` for
  them), which the IMAP server converts in both directions. Both kinds
  of client address the same mailbox. Mailboxes that such a client
  created under 0.35.2 or earlier are stored under their modified UTF-7
  name; see
  [Upgrade: modified UTF-7 names](#upgrade-modified-utf-7-mailbox-names).
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
shardpostctl plan fts_dict_convert user@example.org        # dictionary layout of 0.35.0
shardpostctl plan mailbox_name_convert user@example.org    # modified UTF-7 names of 0.35.2 and earlier
shardpostctl plan attachment_backfill user@example.org     # JMAP hasAttachment of older messages
shardpostctl plan -mailbox "Archiv/2019" mailbox_delete user@example.org
shardpostctl plan purge user@example.org
shardpostctl apply -wait <plan-id>
shardpostctl job list -account user@example.org
shardpostctl job show|wait|cancel <job-id>
shardpostctl plan -all index_check                         # a batch over every account
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
| `scrub` | `repair` | no | scrubber with the chosen classes: `deliveries`, `moves`, `retired`, `uncovered`, `entries`, `refs`, `orphans`, `unknown`, `quota` (all of them if none are given); with `scrub.orphan_source: intents`, `orphans` covers the blobs of open intents and `unknown` the objects of abandoned creations, unfinished deletions, the private `\Seen` marks of public folders that are gone and the METADATA of deleted mailboxes (see [storage](architecture/storage.md#intents-debris-that-a-scrub-finds-without-listing)); the report carries `OrphanSource`, `IntentsOpen`, `IntentsDropped`, `DeletionsFinished`, `CreationsUndone`, `StaleMetadata` and, in shadow mode, `Shadow` |
| `quota_recalc` | `repair` | yes | recount the usage from the index and set it |
| `acl_reconcile` | `repair` | no | write missing pointers for recipients, remove own pointers that carry no rights |
| `fts_rebuild` | `repair` | yes | delete the full-text index (with the header store) and rebuild it from all messages; when it completes it sets the index version marker that IMAP substring search needs to narrow fully, see [configuration](configuration.md#full-text-index-fts) |
| `fts_dict_convert` | `repair` | no | write the substring dictionary of the full-text index again from the posting lists and delete the objects of the layout before 0.35.0; reads no message, may be repeated, stops when an import or another job locks the account |
| `mailbox_name_convert` | `repair` | yes | rename mailboxes stored under a modified UTF-7 name to their UTF-8 name (children move with the parent) and rewrite subscriptions without a mailbox the same way; a rename whose target exists is a conflict (with its children) and is skipped; names that are not valid UTF-8 are reported (see [Upgrade: modified UTF-7 names](#upgrade-modified-utf-7-mailbox-names)) |
| `attachment_backfill` | `repair` | no | read every message whose index record does not carry `hasAttachment` (stored before the record had the field) and remember the answer in the JMAP locator (`a:<guid>`, see [JMAP: hasAttachment](jmap.md#hasattachment)); changes no message and no index record, may be repeated and then reads only what is left, stops when an import or another job locks the account; a mailbox that cannot be listed and a message that cannot be read are skipped and counted (`skipped_mailboxes`, `failed`, the first errors in `errors`); the plan counts such messages and the ones already remembered |
| `mailbox_delete` | `delete` | yes | delete the mailbox including its child mailboxes (not INBOX) |
| `expunge` | `delete` | no | permanently remove messages of a mailbox: `uids` or `deleted: true` (all with `\Deleted`) |
| `purge` | `delete` | yes | withdraw shares in both directions, delete learning jobs and all objects of the account (listed with `purge.listing: true`, the default; named by the data model with `false`, see [storage](architecture/storage.md#purge-without-listing)), remove the directory entry, delete the account's JMAP push and submission records |

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
  `fts_rebuild` and `fts_dict_convert` have no checkpoint and start over
  after a takeover; `attachment_backfill` has none either, but keeps what
  it wrote and reads only what is left.
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
  different request the key is rejected (409). Keys are per caller. A
  key expires together with its job, `jobs.retention_days` after the
  job ended; after that the same key starts a new job. Earlier versions
  kept keys forever; keys from before the update expire once
  `shardpost index backfill kv` ran (see
  [Operations](operations/README.md#admin-store-backfill)). With
  `admin.kv_index: off` keys do not expire.
- **Scope:** plans that change other accounts (share reconcile, purge)
  need those accounts in the client's scope, both when the plan is created
  and again at `apply`. Clients with a scope see jobs only when an account
  is given. Jobs and plans outside their scope do not exist for them (404).
- Without the job system (`jobs.enabled: false`) the job endpoints answer
  503 `temporary`.
- **purge** removes only the data in the store. The identity (LDAP)
  belongs to its owner. If the account logs in again afterwards, ShardPost
  creates an empty INBOX.
- **Purge phases** (checkpointed, a takeover continues with the next
  one): 1 withdraw the account's grants to others, 2 withdraw the grants
  others gave it (public folders included), 3 delete its learning jobs,
  4 mark the account `purging` in the account directory (still under the
  account lock) and delete every object of its namespaces, the registry
  last, 5 remove the directory entry. From phase 4 on, while the registry
  is gone, nothing can create the account again: deliveries are deferred
  (`451 4.2.1`), sessions get `NO [UNAVAILABLE]`, and a grant to the
  account is refused (`409 account_purging` here, `NO [UNAVAILABLE]` for
  SETACL) before the ACL is written. Only the job's own lock session gets
  through, so that a takeover can take the account lock again; that
  leaves an empty registry, which phase 5 deletes once more before it
  removes the entry. After phase 5 the name is free. See
  [storage](architecture/storage.md#store-wide-sorted-indexes-and-the-account-directory).
- **A purge that ended after phase 4** without finishing (failed,
  cancelled) leaves the account `purging` and without registry. Nothing
  can use the account then, and no exclusive job can take its lock — a
  second purge included: such a job fails at once with "the account was
  left purging by an ended purge job; run shardpostctl account repair".
  Run `shardpostctl account repair` first (below), then plan again.
- Finished jobs and plans stay retrievable for `jobs.retention_days`
  (a job from its end, a plan from its expiry), together with the
  idempotency key of a job. With `admin.kv_index: read` (the default) a
  new job is picked up at once by a server with free capacity (the
  runners are woken); otherwise, and as the fallback, within
  `jobs.poll_seconds`. In `read` mode job lists read the hint index of
  the jobs instead of listing the admin namespace.

### Batches over all accounts

```sh
shardpostctl plan -all index_check                 # role diagnose
shardpostctl plan -all -skip-idle scrub            # role repair; what the nightly scrub runs
shardpostctl plan -all -classes refs scrub
shardpostctl plan -all fts_rebuild                 # locks each account in turn
shardpostctl plan -all attachment_backfill
shardpostctl apply -wait <plan-id>
```

`POST /admin/v1/plans` plans a **batch**: one kind run on every account of
the account directory. Body: `kind` (`index_check`, `scrub`,
`fts_rebuild`, `attachment_backfill`), `params` (scrub: `classes`) and
`skip_idle` (scrub only). The plan and its job have the kind `batch` and
no account; `summary` names the kind, its role, whether it locks the
accounts, the parallelism and the directory (shards, backfill marker).
It is applied like any plan with `POST /admin/v1/jobs`.

- **Role and scope:** a batch needs the role of its kind and the scope of
  all accounts (socket callers; mTLS clients with `all_accounts`), for
  planning, applying and cancelling. Reading it needs `diagnose`, like
  any job; clients with an account scope do not see it.
- **Precondition:** the account directory must carry a completed
  backfill marker (`shardpost index backfill accounts`), otherwise the
  plan is refused with `409 conflict`: accounts from before the directory
  would be missed.
- **Walk:** the job reads the directory a page at a time in its order
  (shard, account) and works on `jobs.batch_parallel` accounts at once
  (`scrub.parallel` for a scrub). After each page it stores the cursor
  `<shard>:<account>` and the counters as its checkpoint; a server that
  takes it over redoes at most one page. An entry `purging` without
  registry counts as `gone`; with a registry it is handled like an active
  one. The progress shows the directory shard and the accounts looked at
  (the total is not known up front).
- **Account lock:** a kind that locks the account (`fts_rebuild`) takes
  the lock per account, keeps it renewed while it works and waits
  `jobs.settle_seconds` before it writes, so a batch of it takes at least
  accounts × `jobs.settle_seconds` / `jobs.batch_parallel`. While it holds
  an account, that account is in maintenance as with a single job: its
  open IMAP and POP3 sessions get `BYE [UNAVAILABLE]` / `-ERR [SYS/TEMP]`
  at their next command, new logins and LMTP deliveries are deferred. A
  batch of `fts_rebuild` therefore disconnects every account once, one
  after another; run it in a quiet window. An account an
  import or another job holds is put aside and tried again after the walk,
  up to three times a lease period apart (at most 1000 accounts are put
  aside, the rest fail at once); still locked, it counts as failed.
- **Cancel:** `job cancel` stops the batch between two accounts.
- **Result:** `accounts` (looked at), `done`, `skipped` (idle, scrub with
  `skip_idle`), `gone`, `failed`, `findings` (accounts whose report had
  findings: `index_check`, `scrub`), the first 50 failed accounts with
  their error (`errors`, `errors_dropped` for the rest) and the first 50
  accounts with findings (`finding_accounts`). A batch with a failed
  account ends `partially_failed`; the per-account reports are not kept
  (run the kind for one account to see one).
- **Skip-if-idle** (`skip_idle`, scrub only): every batch scrub of all
  repair classes writes the account's scrub record into its directory
  entry (shown by `account directory` and `account list`). A batch with
  `skip_idle` leaves an account alone when its last batch scrub was
  complete and found nothing, is younger than `scrub.interval_days`, the
  account's change counter and registry version are still the recorded
  ones, the scrub object holds no orphan candidates and no quota count,
  and no delivery or MOVE intent is open. A skipped account misses the
  per-account listings, so unreferenced JMAP upload blobs and the `\Seen`
  marks of deleted public folders wait for its next full scrub, at most
  `scrub.interval_days` later (until a later stage records intents for
  them). See
  [storage](architecture/storage.md#batches-over-all-accounts-and-the-batch-scrub).
- **Export** is no batch kind: an export streams one archive per request
  to the caller, and a batch job has no place to put one archive per
  account. Export accounts one by one (`shardpostctl export`, listing them
  with `account list`).

`shardpost scrub --batch` (the nightly CronJob) submits the batch scrub
with `skip_idle` through the admin store directly, unless a batch scrub
has not ended yet (see [Operations](operations/README.md#nightly-scrub)).

### The account directory

```sh
shardpostctl account directory user@example.org   # role diagnose
shardpostctl account list -limit 500              # role diagnose, scope of all accounts
shardpostctl account list -all -state purging
shardpostctl account repair user@example.org      # role repair
shardpostctl account repair --json user@example.org
```

`GET /admin/v1/accounts/{account}/directory` (role `diagnose`) shows the
account's directory entry (state, creation time, the purge's lock
session, the last batch scrub), whether its registry exists, and the
directory itself: shard count, phase and the completion marker of the
last backfill.

`GET /admin/v1/accounts` (role `diagnose`, scope of all accounts) lists
the directory a page at a time: `cursor` (`<shard>:<account>`, from
`next`), `limit` (1 to 1000, default 100) and `state` (`active`,
`purging`). The order is the directory's (shard, then account), not
alphabetical; it reads the directory, never a pool listing. With `state`
a page may hold fewer than `limit` items while `next` is present (a page
reads at most ten times `limit` entries). `shardpostctl account list`
pages with `-after`, fetches every page with `-all` and prints JSON lines
with `-all --json`.

`POST /admin/v1/accounts/{account}/directory/repair` (role `repair`)
applies the repair rules to one account:

- `purging`, and no purge job of the recorded lock session still runs
  (`queued`, `running` or `cancel_requested`): the entry is removed when
  the registry is gone (`removed`) and set back to `active` when it
  exists (`activated`); while the job runs, nothing changes
  (`purge_running`).
- `active`, older than ten minutes and without registry: the entry is
  stale and removed (`stale_removed`). It comes from a purge by an older
  version, a crash between the registry hook and the registry creation,
  or a creation whose write failed.

Anything else is `none`. The answer shows the entry before and after.
Neither command resolves the account through Nauthilus: a purged account
may no longer exist there. The same rules run for every entry in
`shardpost index backfill accounts` (see
[Operations](operations/README.md#account-directory-backfill)).

### Upgrade: modified UTF-7 mailbox names

Up to 0.35.2 the IMAP server stored mailbox names as the client sent
them. A client that had not enabled `UTF8=ACCEPT` sends non-ASCII names
in modified UTF-7, so a mailbox it created as "Entwürfe" is stored as
`Entw&APw-rfe`. Since then the server converts names on the wire and
keeps UTF-8 in the store. Such a mailbox keeps its old name, which every
client now sees literally (an older client as `Entw&-APw-rfe`), and an
older client that opens "Entwürfe" no longer finds it.

After the upgrade, run `mailbox_name_convert` for the accounts that may
have such mailboxes (all of them, if unsure; the plan of an account
without any is empty):

```sh
shardpostctl plan mailbox_name_convert user@example.org
shardpostctl apply -wait <plan-id>
```

The plan lists every rename (`rename`, with the mailbox ID), every
subscription without a mailbox that is rewritten (`subscription`),
every conflict (`conflict`: the UTF-8 name is taken, or a parent is in
conflict; nothing is renamed for it, resolve it by hand and plan again)
and every stored name that is not valid UTF-8 (`invalid_utf8`). Only a
name level that is exactly the modified UTF-7 form of another string is
converted; `R&D` or `A&B` stay, `A&-B` becomes `A&B`. Check the plan:
a name a UTF-8 client really chose in that form would be converted too.
The job locks the account (open sessions are disconnected and
reconnect), applies only the planned changes that are still needed and
can be run again; when a rename fails, the mailboxes below it stay where
they are and the job ends `partially_failed`. Clients see the renamed
mailboxes as new folders once and synchronise them again.

**The typical conflict.** Delivery and Sieve name mailboxes in UTF-8.
When a Sieve rule (`fileinto :create "Entwürfe"`), an autocreate entry
or a JMAP client created `Entwürfe` and an IMAP client without UTF-8
created `Entw&APw-rfe`, the account has two mailboxes for one name, and
the plan reports a `conflict`. Resolve it by hand: move the messages of
one into the other (with an IMAP client that shows both, or
`shardpostctl move <account> "Entw&APw-rfe" <uids> "Entwürfe"`), delete
the emptied mailbox
(`mailbox_delete`), and plan again; or rename the old one to a third
name (`shardpostctl mailbox rename <account> "Entw&APw-rfe" "Entwürfe (old)"`)
if both are to stay.

**What the job does not rewrite.** Only mailbox names and subscriptions
change. Other places that name a mailbox keep the old name: Sieve
scripts (`fileinto "Entw&APw-rfe"` then files into, or with `:create`
creates, a mailbox of that literal name again), IMAPSieve and learning
rules in the configuration, client-side filter rules and folder settings
(sent and drafts folders). Check the account's Sieve scripts
(`shardpostctl sieve list|show <account>`, or ManageSieve) for names with
`&` after the conversion and change them to the UTF-8 names; clients ask
for their folder settings again or are pointed at the new names.

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
  (`If-Match`, `If-None-Match: *` to create only). A script name is at
  most 512 bytes of UTF-8, as over ManageSieve and JMAP. The active script
  cannot be deleted. `jmap-vacation` is the server's copy of the JMAP
  VacationResponse, for display (see [JMAP](jmap.md#vacation)): it can
  be listed, shown and checked, but storing, deleting or activating it
  is refused (409). An import of scripts that names it is refused (409)
  and one that does not leaves it in place; export, restore and
  `migrate` leave it out.
- **Checking Sieve without side effects:** `sieve check` compiles the
  script and, on request, runs it against a stored message (role
  `content`) or a text supplied with the request (`diagnose`). Nothing is
  actually carried out: no redirect, no vacation reply, no notification,
  no external program (`execute`/`filter` count as having run with exit 0,
  without running), no duplicate or vacation record, no filing. The
  response lists what would happen and, under `suppressed`, every
  suppressed side effect. The message carries the `Return-Path` and
  `Delivered-To` fields a delivery adds, so the script sees what it sees
  in a delivery; without `envelope_from` that is the null sender,
  `Return-Path: <>`, and without `envelope_to` the account. A vacation reply reports its effective period, after the
  vacation period settings, as `days` and `seconds` (`seconds: 0` is a
  reply to every message). With a script and a text supplied in the request
  (role `diagnose`), the store tests (`mailboxexists`, `mboxmetadata`,
  `specialuse_exists`) are active. That way `diagnose` learns no more than
  it would through `mailbox list`. This is a deliberate decision.
  With global scripts configured (`sieve.before_dir`, `sieve.after_dir`)
  the check runs the sequence a delivery runs — before, the checked
  script, after — and reports the scripts that ran as `scripts` and
  failed global ones as `failed_scripts`; an account without an active
  script is checked with an empty one.
- **Learning queue:** `learning list|show` shows jobs with their state
  (`pending`, `dead`, `delivered`), program, attempts and next due time.
  `retry` picks up dead jobs only. Learning programs are not idempotent:
  if the program may already have received the message (attempts > 0),
  `-confirm-repeat` is required. A job whose message was never committed
  (no evidence) cannot be retried.
- Clients with an account scope see learning jobs only when an account is
  given. Jobs of other accounts do not exist for them (404).
- With `imapsieve.learning.index: read` the learning list reads the
  owner index of the queue (all jobs, or those of the account given) and
  then each job; otherwise it lists the queue namespace. `show` and
  `retry` read the job by its id in every mode. A retried job is due at
  once, and the worker is woken by a notify in `read` mode.

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

Quote the namespace name: a shell expands `~public` as a home directory
(zsh errors out, bash leaves it alone).

```sh
shardpostctl mailbox create '~public' "Announcements"
shardpostctl acl set '~public' "Announcements" anyone lr
shardpostctl acl set -mode add '~public' "Announcements" team@example.org lrswipkxte
shardpostctl acl explain '~public' "Announcements" user@example.org
shardpostctl mailbox list -all '~public'
```

- **Administration** uses the same operations as an account: the API
  accepts `~public` wherever an account is expected, without a user
  database lookup. `account show ~public` reports the lookup as `public`.
  Delivery is refused: a public folder has no address of its own.
- **Migration:** a public namespace of another server is imported with
  `shardpostctl migrate -source-namespace`, see
  [Importing a public namespace](#importing-a-public-namespace).
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
- **Reading state:** `\Seen` is private per account and kept in that
  account's own namespace; every other flag and every keyword is shared.
  "Answered" and "flagged" are statements about the folder, "read" is a
  statement about a person. `STATUS ... (UNSEEN)` therefore differs
  between accounts. This matches Dovecot, whose private index also holds
  `\Seen` alone.
- **Delivery** goes through Sieve: a normal account receives the message
  and its script files it into the folder
  (`fileinto "Public/Support/Inbox"`). That needs the `p` (post) right on
  the folder for that account; without it the message stays in INBOX, as
  a failed `fileinto` does. The message is stored in the public
  namespace, so it counts against its quota, not the account's. A
  delivery never creates a public folder.
- **Backup:** `shardpostctl export -o public.tar '~public'` and `restore`
  work like they do for an account. `acl_reconcile` is refused for
  `~public`: there are no pointers to reconcile.
- **Switching them off:** an empty `server.public_prefix` disables the
  namespace for clients (no entry in `NAMESPACE`, no extra reads per
  `LIST`, no public account in a JMAP session); the folders stay in the store and remain administrable. While
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

## Auth cache flush

Every server keeps the logins and lookups Nauthilus answered for
`auth.cache.ttl` and `lookup_ttl` (see
[configuration](configuration.md#auth-cache-auth)). A change in LDAP that
has to take effect before — a locked account, a removed alias, a new
quota — is pushed through with a flush (role `repair`):

```sh
shardpostctl auth flush user@example.org   # the entries of one account
shardpostctl auth flush                    # every entry
shardpostctl auth flush --json alias@example.org
```

API: `POST /admin/v1/auth/flush` with an optional
`{"account": "<name>"}`; without it, empty or `*` everything is
flushed. The answer is `{"account": "<name in lower case or *>",
"cached": true}`; `cached` is false on a server without an auth cache
(the static backend, or `auth.cache.ttl: 0`), where nothing is flushed.

The name is matched without regard to case against the login names, the
lookup names and the canonical accounts of the entries, so an alias, an
address or the account all reach the entries that carry them. The
server that answers flushes its own cache before it answers and records
the flush in the flush log of the cluster (the omap object `auth-flush`
in the admin namespace: a generation counter and one entry per flush,
kept for an hour). The other servers apply it when the notification on
that object wakes them and at the latest on their next poll, 60 seconds
later; the answer does not wait for them. A server that lost its watch,
or finds that entries it has not read are gone, flushes everything.
The flush is in the audit line (`flush_account`). When the flush log
cannot be written the answer is 503 (`temporary`): the answering server
is flushed, the others are not.

A flush does not reach the Director's routing cache (up to 30 s) nor the
cache of Nauthilus itself. See
[Operations: Auth cache](operations/README.md#auth-cache) for what each
change costs until it takes effect.

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
  `PUT .../import/mailboxes/{id}/metadata` refuses entries under
  `/private/vendor/shardpost/jmap/`, which the server keeps (400).
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

### Importing a public namespace

A public namespace of the source (Dovecot `namespace { type = public }`)
is not part of any account, so an account import skips it. It is
imported on its own, into the public folders:

```sh
shardpostctl -timeout 10m migrate \
  -source-user lists@example.org \
  -source-namespace Mailinglists/ -target-prefix Mailinglists/ \
  -readers lists@example.org,user@example.org '~public'
```

- **What is read:** the namespace with the prefix `-source-namespace`, as
  `-source-user` sees it (master login as for an account). The folders
  land below `-target-prefix`; a namespace that is itself a mailbox
  (`Zabbix` of `Zabbix/`) becomes the folder the prefix names. Identities
  (UIDVALIDITY, UIDs, MODSEQ, flags, dates) are kept as for an account.
- **Session:** the import session is opened on `~public` and locks the
  public namespace for its duration: IMAP answers `NO [UNAVAILABLE]` for
  `LIST` and for public folders, a session that has one selected is
  closed at its next command (checked every few seconds), a delivery that
  files into one is deferred. Plan the first copy of a large namespace
  accordingly; a delta is short, but reads the flags of every folder once
  per reader.
- **Only into folders that are not in use yet:** a later run makes the
  target what the source is. A reader's `\Seen` becomes exactly the
  source's — what the account read here since the last run is unread
  again — and the reader marks of an earlier run have moved
  HIGHESTMODSEQ past the source, so a CONDSTORE/QRESYNC client that
  synchronized such a folder here between two runs can miss flag changes
  the second run brings with source MODSEQs below that. Run the last
  delta before clients are routed here, and grant the rights afterwards
  (or keep clients away until then).
- **Readers** log in at the source under their account name; an account
  that has another login name there cannot be a reader. A folder a reader
  can list but not open is skipped with a warning.
- **`\Seen`:** the messages carry the `\Seen` of the source user; that is
  the shared default every account starts from. For each account of
  `-readers` the migrator logs in at the source as that account, reads
  its `\Seen` and sends it with
  `PUT .../import/mailboxes/{id}/readers/{reader}`: the server writes
  private marks where the reader's state differs from what the reader
  sees now, under a new MODSEQ of the mailbox (as an IMAP STORE does),
  and sets the reader's subscription. The call is repeatable; a run that
  finds nothing to change writes nothing. The marks are written in
  batches of 1000, each under its own MODSEQ. A folder the reader does not
  see at the source is skipped.
- **Not copied:** rights (Dovecot takes them from the configuration and
  from groups; grant them with `acl set '~public' ...`), Sieve scripts,
  special-use attributes.
- **Prune** only removes folders below a non-empty `-target-prefix`; the
  folder the prefix itself names stays.
- **Scope:** an admin client with a scope needs `~public` and every
  reader in it.
- **Delivery** into the imported folders is a matter of Sieve; what a
  global `sieve_after` script did at the source is done here by a script
  in `sieve.after_dir` (see [configuration](configuration.md)).

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
