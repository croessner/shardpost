> Historical expansion plan. The stages and measurements document earlier
> planning and are not a current release or capacity commitment.
> Current entry points: [Documentation](docs/README.md),
> [Quality status](docs/development/quality.md), [Name migration](docs/decisions/project-name.md).
>
> New planning from 2026-09-16: **[Stage 8: operations management with
> shardpostctl and OpenAPI](docs/development/admin-stage.md)**. This is what is
> currently planned; the historical stages below are kept as history.
> New within it (2026-09-18): identity-preserving account import as a
> pulled-forward core of sub-package 8e, so that migrations do not force
> clients into a full resync.

# Expansion Stages: From POC to a Production Mail Store on RADOS

This document describes what the POC can do today, what it already prepares for
in the data model, and in which stages it grows into a server for an audience of
millions. The order is sorted by benefit per effort.
The basis is the data model in [ARCHITECTURE.md](ARCHITECTURE.md).

## State of the POC (image 0.5.x)

**Custom protocol layer** (`internal/imapproto`): parser, dispatcher and
encoder are our own code; go-imap now only serves as a library for
MIME sections, BodyStructure and Envelope. This makes every extension
first-class that the library did not know on the server side.

**Protocol:** IMAP4rev1 and IMAP4rev2, LOGIN and AUTHENTICATE PLAIN with
SASL-IR, STARTTLS and implicit TLS, LOGINDISABLED without TLS (can be disabled),
ID, ENABLE, UTF8=ACCEPT, NAMESPACE, UNSELECT, IDLE via RADOS watch/notify,
LIST with hierarchy, \Noselect intermediate nodes, CHILDREN, LIST-EXTENDED with
RECURSIVEMATCH/CHILDINFO, LIST-STATUS, SPECIAL-USE, CREATE-SPECIAL-USE,
auto-provisioning of the default mailboxes, SUBSCRIBE/LSUB, CREATE/DELETE/RENAME
including subtree and the INBOX special case, SELECT/EXAMINE (read-only is
enforced), STATUS with SIZE, HIGHESTMODSEQ and MAILBOXID, APPEND with
MULTIAPPEND, LITERAL+ and LITERAL-, APPENDLIMIT, FETCH with all sections,
partials, BINARY, ENVELOPE from the index cache, MODSEQ, EMAILID, THREADID,
PREVIEW, SAVEDATE, **CONDSTORE** (CHANGEDSINCE, UNCHANGEDSINCE, MODIFIED,
MODSEQ in SEARCH), **QRESYNC** (SELECT parameter, VANISHED, VANISHED
EARLIER), SEARCH with ESEARCH, SEARCHRES, all RFC 3501 keys, MODSEQ,
EMAILID, THREADID and full text with stemming and substrings, **SORT** and
**THREAD** (REFERENCES, ORDEREDSUBJECT) from the envelope cache without
blob access, STORE with keywords, EXPUNGE, UID EXPUNGE, COPY, MOVE, UIDPLUS,
**QUOTA** (GETQUOTA, GETQUOTAROOT, SETQUOTA, RES-STORAGE, RES-MESSAGE),
OVERQUOTA codes, idle timeout of 30 minutes.

**Operations (stages 1b to 3, implemented in 0.4.x):** POP3 with a maildrop lock
via `rados_lock_exclusive`, COMPRESS=DEFLATE, charset conversion in
SEARCH, connection limits per IP and user, write timeouts, sharded
mailbox index (shard size per mailbox in the header), change log `c:<modseq>`
with incremental polling, registry cache, cap on frequent trigrams in the
dictionary, scrubber as a CronJob, Prometheus metrics on `/metrics`.

**Stage 5 (0.5.x):** authentication against Nauthilus over gRPC
(`nauthilus.auth.v1.AuthService`, HTTP JSON as an alternative) with
XOAUTH2/OAUTHBEARER (token in the password field per the Director contract, or
local OIDC introspection), master user login for the nauthilus-director,
LMTP recipient resolution via `LookupIdentity`, optional PROXY protocol,
Sieve (fileinto, imap4flags, envelope, variables) in the delivery path plus
ManageSieve on 4190, intent log for MOVE with scrubber reconciliation,
cephx key rotation without restart, graceful drain with BYE, OpenTelemetry
(OTLP, via environment variable), load generator `imapload`.

**Load test (2 pods, 20 users × 3 sessions, 25 deliveries/s, 60 s):**
IMAP commands p50 20 to 100 ms, p99 below 500 ms, LMTP p50 600 ms
(synchronous full-text index, see asynchronous indexing), no errors.

**Review rounds (Codex, `docs/review-round1.md` through `review-round4.md`):**
round 1 found 31 findings, round 2 re-checked the fixes and found 7
follow-up bugs, round 3 found 5 more (budget deadlock, MODSEQ going backwards,
snapshot race in PendingFloor, CAS attempt results in STORE,
MOVE refcount versus EXPUNGE), round 4 reviewed the roadmap implementation and
found 7 (delivery intents: success despite an open intent, rollback by UID,
recording after storing; duplicate doc IDs in the
FTS conversion; VANISHED fallback proportional to UIDNEXT; deletion
on a stat error; token refresh window). Round 5 reviewed ACL, METADATA,
CATENATE, NOTIFY and Sieve sending and found 18 (among them redirect without
a target confirming loss, CATENATE without a budget, replacing STORE and
APPEND/COPY flags without a permission check, the existence of other users'
mailboxes being detectable, reader swap in NOTIFY, IDLE DONE outside the lock,
the vacation period consumed before sending). As of 0.8.1: all handled
(details in the [earlier architecture](docs/history/architecture-before-2026-09-16.md#what-the-review-forced),
section "What the review forced").
Still open and deliberately left that way: pending changes expire after
30 s, VANISHED appears in the log before the shard commit, a missing last
shard is not detectable by the scrubber, MOVE recovery identifies
target entries only by GUID. The points still open after that
(quota reservation, delivery intents, VANISHED without known UIDs,
posting compaction, two-stage orphan deletion, PendingFloor without a
fixed window) were implemented in 0.7.x, see ARCHITECTURE.md. Still open
is the scrub enumeration over the entire data set (stage 2).
Side effect: SETQUOTA is blocked for users
(`QUOTA_SELF_SERVICE=true` enables it).

**Data model:** see ARCHITECTURE.md. Everything the protocol emits
comes from the index (meta records, header omap, vanished log, registry);
a blob is read only for body sections, BODYSTRUCTURE and nested
text search.

**Tests:** the protocol layer is cgo-free and is tested locally against an
in-memory backend (`go test ./internal/imapproto`), without Ceph.
The cluster test `hack/test-proto.sh` runs the same commands against RADOS.

## Stage 1: Remaining Protocol Work

* **CATENATE** (RFC 4469) and **URLAUTH**: assembly from parts in the
  APPEND path.
* **NOTIFY** (RFC 5465): watches on several mailboxes at the same time;
  the watch model already supports that.
* **LIST-MYRIGHTS, ACL, METADATA**: see stage 4.
* **SEARCH=FUZZY**, phrases, CJK n-grams in full text.
* **Pipelining optimization:** commands are processed sequentially;
  a read-ahead that requests literals early saves round trips.

## Stage 1b: POP3 (implemented)

`internal/pop3proto` speaks the same `model.User` interface as IMAP:
USER/PASS, AUTH PLAIN, STLS, CAPA, STAT, LIST, UIDL (the GUID), RETR,
TOP, DELE, RSET, QUIT, UTF8. INBOX only, maildrop lock via a RADOS lock on
shard 0 (a second session gets `-ERR [IN-USE]`), RETR sets `\Seen`,
QUIT expunges. The cephx client needs `rwx` for this (object classes).
Ports 110 with STLS and 995 implicit.

## Stage 2: Scaling the Index (implemented except for asynchronous indexing)

* **Index sharding** is in place: `mbox/<id>` (header + shard 0) and
  `mbox/<id>/<n>`, shard = (UID-1)/shard size. The shard size is fixed in
  the header at CREATE time (`h:shardsize`); the configuration
  applies to new mailboxes only. A write to shard 0 is a single CAS, writes to
  other shards are two-phase (header, then entry); the scrubber cleans up
  gaps. The POC overlay sets 20, production 100000.
* **Change log and session cache** are in place: `c:<modseq>` per write,
  a poll reads only the header, the log and the affected entries; a complete
  reload happens only when `h:logstart` no longer covers the session's position
  (metric `commands_total{command="poll",outcome="incremental|reload"}`).
* **Registry cache** (2 s TTL) and **trigram cap** (2000 terms, after which
  a marker is set and the term is omitted from the query, with a fallback to a
  vocabulary scan) are in place.
* **Posting compaction** is in place: document IDs per user
  (`ftsmap`), posting lists as sorted delta varints
  (`internal/postings`); old lists are converted on the first write.
* **Still open:** asynchronous indexing via an omap queue; delivery
  latency with the synchronous index was a few tens of
  milliseconds in testing, so the need has not arisen yet. The scrub enumeration
  (all objects in a namespace) keeps growing with the data set; it is
  a batch run and can be parallelized per user.

## Stage 3: Consistency and Operations (partially implemented)

* **Scrubber** is in place (`shardpost scrub`, CronJob `shardpost-scrub`):
  orphaned blobs (with a grace period), entries without a blob, refcounts,
  quota, index objects without a registry entry. In testing it repaired a
  planted orphan and an incorrect refcount. `SCRUB_DRY_RUN` and
  `SCRUB_USERS` control it.
* **Metrics** are in place: RADOS latencies per operation, CAS retries per
  object type, command latencies and counters per protocol, connections,
  deliveries, scrub findings. Pod annotations for Prometheus scraping.
* **Limits** are in place: connections per IP and user, idle timeout,
  write timeout as backpressure, literal limits.
* **Intent log for MOVE** is in place: `i:<id>` on the source object before the
  move, removed in the same CAS as the unlink; the scrubber completes
  stalled moves deterministically.
* **Delivery intents** are in place: `d:<id>` on the registry object for
  Sieve multi-filing and MULTIAPPEND; the scrubber rolls aborted
  deliveries back after ten minutes.
* **Atomic quota reservation** is in place: limit and counter in a single CAS,
  released again on failure.
* **Two-stage scrubber** for orphans: only what two runs saw
  unchanged and unreferenced is deleted.
* **Key rotation without restart** is in place: the Rook secret is polled
  (`KEY_POLL_SECONDS`), a new librados connection takes over, and the old one
  is closed as soon as no session uses it any more. In testing it is triggered
  via `keyGeneration` on the CephClient.
* **Graceful drain** is in place: readiness turns red, IDLE sessions
  receive `* BYE [UNAVAILABLE]`, `DRAIN_SECONDS` of wait time.
* **OpenTelemetry** is in place: spans per IMAP/POP3 command and LMTP
  delivery, OTLP gRPC/HTTP via `OTEL_EXPORTER_OTLP_ENDPOINT`. Since
  0.24.0 the store reports below them (`store.*`, `rados.omap_cas`): the
  protocol layer hands the command context to the store view
  (`model.RequestScoped`), so a trace shows blob access, index reads and
  full-text writes as children of the command.
* **Not in the POC:** a custom object class (cls; C++ on every OSD, not a
  candidate for a production cluster), multi-cluster (no second cluster).

## Stage 4: Product Features (partially implemented)

* **Authentication: Nauthilus** is in place (`internal/auth`), with gRPC
  preferred. Same contract as for the nauthilus-director: Basic or
  Bearer backchannel, `AUTH_DECISION_OK|FAIL|TEMPFAIL`, `Auth-Wait` is
  honored (HTTP), `auth_login_attempt` per client address, account from
  `attributes[account_field][0]`, lowercased. Temporary failures are reported
  outward as `[UNAVAILABLE]`, `-ERR [SYS/TEMP]` or `451` respectively,
  never as a failed login. The POC deployment still runs with the
  static backend because the backchannel credentials and a test account
  of the production Nauthilus instance are not in this namespace;
  switch over with `AUTH_BACKEND=nauthilus` plus `NAUTHILUS_*`.
* **XOAUTH2 / OAUTHBEARER** are in place (IMAP AUTHENTICATE, POP3 AUTH),
  including the RFC 7628 error dialog. Token validation either through Nauthilus
  (token in the password field, `method=xoauth2|oauthbearer`) or locally via
  `/oidc/introspect` followed by `LookupIdentity`.
* **Director integration:** master user (`<account>*director` with a
  master password, `MASTER_USER*`), accepting LMTP RCPT unchanged
  and resolving it via `LookupIdentity` (`method=recipient_lookup`), optional
  service auth via `AUTH PLAIN` (`LMTP_AUTH_*`), PROXY protocol v1/v2
  on all listeners (`PROXY_PROTOCOL_TRUSTED_CIDRS`), one account for
  deep health checks.
* **Sieve / ManageSieve** is in place (`internal/sieve`, foxcpp/go-sieve):
  scripts as the omap of the per-user object `sieve`, the active script in the
  xattr; fileinto, keep, discard, reject (550), imap4flags, envelope,
  variables; redirect is logged and treated as keep (no MTA
  in the POC). ManageSieve: AUTHENTICATE PLAIN, STARTTLS, PUT/GET/LIST/
  SETACTIVE/DELETE/RENAME/CHECKSCRIPT/HAVESPACE.
* **Load generator** `imapload` (LMTP plus IMAP sessions, percentiles) as
  job `hack/loadtest-job.yaml`.
* **Implemented (0.8.0):** shared folders and ACL (RFC 4314), METADATA
  (RFC 5464), NOTIFY (RFC 5465, without MailboxName/SubscriptionChange/
  AnnotationChange), CATENATE (RFC 4469), Sieve redirect and vacation with
  sending through an SMTP relay. Still open: URLAUTH, per-user flags
  in shared mailboxes and LMTP mode for the relay.
* **Implemented (0.8.1 to 0.8.6):** WITHIN, REPLACE, LIST-MYRIGHTS,
  UNAUTHENTICATE; Sieve date/currentdate, duplicate, editheader, mailbox;
  conformance runs with imaptest (71 of 72 groups) and the
  Pigeonhole suite, see `hack/conformance.md`.
* **Implemented (0.9.0):** Sieve ihave, include, index, regex, spamtest/
  virustest, mboxmetadata/servermetadata, special-use, enotify (mailto),
  foreverypart, mime, extracttext; Pigeonhole suite 117 of 148 files.
  Open: source route in the ENVELOPE, extlists, replace/enclose;
  imapsieve and extprograms as stage 5, JMAP as stage 6.

## Stage 5: Sieve in the IMAP Path – implemented and rolled out

The server configuration uses Viper and `validator/v10`. YAML files
and existing environment overrides are validated before any services are
opened. See [Configuration](docs/configuration.md).

IMAPSieve processes APPEND, COPY, MOVE and actual flag changes
(including FETCH/Seen). Scripts are assigned via METADATA and executed
under the permissions of the triggering user. ManageSieve and
delivery scripts are validated separately. Details and limits are in
[IMAPSieve/Rspamd](docs/imapsieve.md).

Learning uses explicit special-use rules for COPY/MOVE and a
durable Ceph queue with commit records, a cluster-wide worker lease,
backoff and retained failed jobs. APPEND and imports do not train.
From 0.11.0 on, the worker starts only programs from the configured
allowlist. The replaceable Python helper handles the Rspamd integration;
the Go learning path knows no classifier.

Extprograms (`vnd.dovecot.pipe`, `filter`, `execute`) are implemented,
with a limited number of invocations, immutable program names, fixed arguments
and explicit approval of additional script arguments. Failing pipes
preserve the message; filter and header changes stay in script order.
The details of the review fixes, the tests and the rollout are in the
[acceptance report 0.11.0](docs/reports/2026-09-16-extprograms.md).

The earlier acceptance of 0.10.1 remains documented in the
[report from September 15](docs/reports/2026-09-15-imapsieve.md).
Remaining general conformance gaps are not a full release of all
Sieve extensions. extlists, replace/enclose and `vnd.dovecot.report`
stay outside this stage.

### Re-verification 0.11.2

The identified Sieve and test-adapter bugs are fixed: CPU and MIME limits,
parser validation, vacation and SMTP details, byte regex and multiscript/flags
isolation. Linux Sieve: 139 passed, 0 failed, 9 explicitly
delimited exceptions; the full Linux race tests of the server passed.
The additional HTTP 204 bug found in production in the learning helper is
fixed in 0.11.2. [Fixes and release evidence](docs/reports/2026-09-16-sieve-conformance-fixes.md).

## Stage 6: JMAP in Full – in progress

**State 2026-09-25:** the staged build-out follows
`temp/08-jmap-staged-plan.md`. Stage 0 of that plan (the account change
log, the account-wide state JMAP needs and IMAP does not have), stage 1
(Core, RFC 8620: session resource, API endpoint with batching and
back-references, authentication, blob upload and download), stage 2
(`Mailbox/*`), stage 3 (`Email/get`, `Email/changes`, `Thread/*`) and
stage 4 (`Email/query`, `Email/queryChanges`, `Email/set`,
`Email/import`, `Email/parse`, `Email/copy`) are implemented; see
[JMAP](docs/jmap.md) for what exists and what does not. Push,
submission and identities are the next stage. What follows is the full
target of the stage, unchanged.

Goal for this server: a complete JMAP server alongside IMAP, not
a proxy in front of it. Complete means: JMAP Core (RFC 8620), all of JMAP Mail
(RFC 8621), plus the extensions that the clients (Mozilla
Thunderbird, Ltt.rs, Meli, Cyrus-/Stalwart-compatible webmailers)
actually use. The data path stays the same: JMAP is a
second protocol layer on top of `model.User`, not a second data store.

**Core (RFC 8620):**
* Session resource `/.well-known/jmap` with accounts, capabilities,
  `apiUrl`, `downloadUrl`, `uploadUrl`, `eventSourceUrl`; state per
  data type from the existing ModSeqs (mailbox registry, shard header,
  the mailbox's `h:modseq`).
* Authentication through Nauthilus: bearer token (OAuth 2.0, introspection
  over the gRPC backchannel as in the Director contract) and Basic for
  scripts; master user login for the Director.
* API endpoint with method calls, batching, back-references
  (`#ref`/`resultOf`), `using` validation, error model per RFC 8620 3.6;
  limits (`maxSizeRequest`, `maxCallsInRequest`, `maxObjectsInGet`,
  `maxObjectsInSet`, `maxConcurrentRequests`) from the configuration.
* Blob upload and download (including `Blob/copy`) directly onto the
  RADOS blobs: an upload creates `msg/<guid>`, which is referenced by
  `Email/import` or `EmailSubmission/set` and otherwise removed by the scrubber
  once it expires.
* Push: `EventSource` (server-sent events) over the existing
  RADOS watches, `PushSubscription` with Web Push (RFC 8030, VAPID,
  encryption per RFC 8291), `StateChange` per data type.
* `Core/echo`, compression, HTTP/2, clean `Location` redirects behind
  the Director (JMAP runs over HTTPS; the Director routes by host/path
  or token claim to the shard).

**Mail (RFC 8621), everything:**
* `Mailbox/get`, `/changes`, `/query`, `/queryChanges`, `/set` with
  roles (`role` from special-use), rights (`myRights` from RFC 4314) and
  sorting; `sortOrder` and `isSubscribed` persisted as a
  mailbox attribute.
* `Email/get` with all properties (header parsing in every form:
  `header:X:asText/asAddresses/asGroupedAddresses/asMessageIds/asDate/
  asURLs/asRaw`, `bodyStructure`, `bodyValues` with `maxBodyValueBytes`,
  `textBody`/`htmlBody`/`attachments`, `preview`), `Email/changes`,
  `Email/query` with the complete filter (text, addresses, date,
  size, keywords, `inMailbox`/`inMailboxOtherThan`, `allInThreadHaveKeyword`
  and related) and all sort keys, `collapseThreads`,
  `Email/queryChanges`, `Email/set` (keywords, mailboxes, creation from a
  structure description, deletion), `Email/copy`, `Email/import`,
  `Email/parse`; keywords mapped bijectively onto IMAP flags (`$seen`,
  `$flagged`, `$answered`, `$draft`, `$forwarded`, `$phishing`, `$junk`,
  `$notjunk`).
* `Thread/get` and `Thread/changes` from the existing
  REFERENCES threading; `threadId` stable per conversation
  (persisted in the meta entry, not recomputed on every call).
* `SearchSnippet/get` from the FTS index (match highlighting).
* `Identity/get` and `/set` (sender identities, pre-populated from LDAP via
  Nauthilus, additions persisted).
* `EmailSubmission/get`, `/changes`, `/query`, `/set` with
  `onSuccessUpdateEmail` and `onSuccessDestroyEmail`: sending through the
  relay (`SMTP_RELAY_*`), `envelope`, `undoStatus` with a `sendAt` queue
  (persistent, in the user namespace), DSN evaluation (RFC 3464) for
  `deliveryStatus`.
* `VacationResponse/get` and `/set` as a view onto the vacation script
  (a server-managed Sieve script `jmap-vacation`, so that IMAP
  and JMAP clients see the same out-of-office setting).

**Extensions that belong with it:**
* JMAP for Sieve Scripts (RFC 9661): `SieveScript/get`, `/set`,
  `/validate`, `/query` on the same script storage as ManageSieve.
* JMAP Quotas (RFC 9425): `Quota/get`, `/changes`, `/query` from the
  user's quota.
* JMAP Blob Management (RFC 9404): `Blob/get`, `Blob/upload`,
  `Blob/lookup`, `Blob/copy` with byte ranges.
* JMAP Sharing (RFC 9670) and JMAP Mail Sharing (draft): `Principal/get`,
  `ShareNotification`, shares from the ACLs, `Other Users` as further
  accounts in the session.
* JMAP for Contacts and Calendars (RFC 9610, draft-ietf-jmap-calendars)
  only once there is data for it (not mail, a separate item).
* WebSocket transport (RFC 8887) for clients that do not want an EventSource.

**What the core lacks for this and what comes first:**
* Stable `threadId` and `emailId` in the meta entry (today the
  thread ID is derived from the References on each call; JMAP needs it
  as a change stream).
* A change log per data type that answers `/changes` and `/queryChanges`
  across session boundaries: the existing
  shard change entries (`c:` with ModSeq and UIDs) suffice for emails
  and mailboxes; for identities and submissions small dedicated
  logs are added; `cannotCalculateChanges` when the log has been truncated.
* A configurable header parser library for `header:*:as*`
  (the existing address parser is the basis).
* Body parsing with charset conversion and HTML sanitizing for
  `bodyValues` (converters as for `body :text` in Sieve).

**Verification:** JMAP conformance with the `jmap-samples`/`jmap-client`
test corpus, the Stalwart JMAP test set and a dedicated run with
Thunderbird against the Director, analogous to `hack/conformance.md`.

**What JMAP does not change:** delivery (LMTP), the Sieve runtime, the storage
model. It is a third frontend layer alongside IMAP and POP3, and it
should be built in its own package `internal/jmap` with its own test coverage.

**Director contract (requirement from `nauthilus-director/docs/FUTURE_JMAP.md`):**
The Director cannot do JMAP today. Its design note specifies that
it will later carry JMAP exclusively as a JMAP-to-JMAP proxy: its own
HTTPS listener, authentication through Nauthilus, routing over the same
routing facts and shard tags as IMAP, reverse proxy to a backend
that speaks JMAP; no translation to IMAP, no mailbox semantics in the
Director, no account aggregation across shards. This imposes firm
requirements on this server as a backend, which are implemented as part of
stage 6:
* **Session resource with public URLs:** in its default mode
  `backend_advertised` the Director does not want to rewrite anything; the
  server must generate `apiUrl`, `uploadUrl`, `downloadUrl`,
  `eventSourceUrl` and the WebSocket URL from a configured `JMAP_PUBLIC_BASE_URL`
  (the Director address), not from the pod name.
* **Per-request authentication:** the Director passes the
  HTTP authentication through and authenticates against Nauthilus itself;
  the server nevertheless verifies it itself too (Bearer over the gRPC
  backchannel, Basic over `Authenticate`), so that it is correct without a
  Director as well.
  Open and to be clarified with the Director owner: whether the Director
  additionally signals an already verified identity to the backend (a master
  user analogy to IMAP, only over mTLS), or whether verification happens twice.
  Both variants are meant to work; the second is the default.
* **Backend TLS with server name:** the Director always verifies backend TLS;
  the certificate from `shardpost-ca` must carry the service name
  configured in the Director as `tls.server_name` (currently
  `shardpost.shardpost.svc.cluster.local`).
* **Health endpoint without login and without side effects** for the
  Director's backend check (`/jmap/healthz`, analogous to `/readyz`).
* **Streaming through the proxy:** EventSource and WebSocket (RFC 8887)
  must work behind an HTTP reverse proxy (no buffering,
  heartbeats, clean close codes on drain and kick); blob upload and
  download within the Director's request limits
  (`max_body_bytes`, default 10 MiB; larger attachments chunked via
  RFC 9404 `Blob/upload`).
* **Affinity:** long-lived connections (EventSource, WebSocket) create
  active leases in the Director; the server must survive connection drops on
  drain and move without data loss (state is in the
  store anyway, not in the connection).
* **Routing facts:** account pinning as today via `director.lua`
  (`mailShard=shardpost`), protocol `jmap` as its own backend pool.

As long as the Director cannot do JMAP, stage 6 is tested directly against the
server (port forward or an internal ingress with the POC certificate)
and only placed behind the Director in stage 7.

## Stage 7: JMAP in the Director – planned, owner nauthilus-director

A downstream stage in the Director repository, assigned to its owner (Codex),
following the phases from `docs/FUTURE_JMAP.md`: (1) contract and fixtures,
(2) JMAP HTTPS listener with Nauthilus auth, routing facts, backend selection
and reverse proxy, (3) session resource in mode `backend_advertised`,
(4) upload, download, EventSource with limits and redaction,
(5) WebSocket (RFC 8887) with session leases, (6) operational functions
(route lookup, drain, move, kick for `protocol=jmap`), (7) end-to-end tests
against this server as a backend fixture. The open questions in the note
(accepted auth schemes, cookies, URL rewriting yes/no, lease timing,
close codes, shared accounts across shards) are answered jointly beforehand;
this server provides the backend fixture and the
answers from stage 6. Prerequisite: stage 6 is in operation.

## What Deliberately Should Not Come

* **A file system or an SQL database for metadata.** Either would be
  a central bottleneck again. Everything listed here as "missing" can
  be expressed as another omap prefix or another object in the user namespace.
  That is the core of the approach and the reason it scales
  horizontally.
* **A message bus for notifications.** RADOS watch/notify handles
  that cluster-wide, see the IDLE measurement in testing (about 25 ms pod to
  pod).
