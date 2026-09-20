> Historical state before the documentation rework of 2026-09-16.
> Not a current operations manual; statements and measurements may be outdated.
> Starting point: [current documentation](../README.md).

# Architecture: mail store directly on RADOS

## Basic idea

At its core, Ceph is an object store (RADOS). CephFS and RGW are layers
on top of it that bring their own metadata paths (MDS and bucket index shards,
respectively). A mail server has a far simpler access pattern than a
POSIX file system: messages are immutable, a mailbox is an
ordered list of UIDs with a few flags. That maps 1:1 onto
RADOS primitives, with no MDS and no HTTP gateway in between.

The approach matches what Dovecot does with `obox`/`rbox` (librmb) and
what RGW does internally for its bucket index.

## Data model

One **RADOS namespace** per user (inside the pool, not one pool per user).
Namespaces are a prefix in the object key, cost nothing and isolate
users cleanly: the IOContext is pinned to the namespace once.

```
Pool imap-index (replicated, size 3)              Pool imap-data (EC 2+1)
namespace <user>                                  namespace <user>
  mailboxes    Registry, omap:                      msg/<guid>/0   Chunk 0, xattr refs=N
                 m:<name>  -> {id, uidvalidity,      msg/<guid>/1   Chunk 1 (4 MiB)
                               subscribed, specialuse}  msg/<guid>/n   ...
                 q:bytes, q:msgs  Quota counters (reserved atomically)
                 d:<id>     -> {started, entries}  Delivery intents
  scrub        omap: o:<guid> -> object version  Orphan candidates (2 runs)
  mbox/<id>    Shard 0 of a mailbox, omap:
                 h:uidnext, h:uidvalidity, h:modseq, h:keywords,
                 h:shardsize, h:logstart
                 c:<modseq> -> {u:[uids], x:bool}   Change log
                 v:<modseq> -> [uids]   Vanished log for QRESYNC
                 m:<uid>    -> {guid,size,chunks,flags,idate,sdate,modseq,env,refs,preview}
  mbox/<id>/n  Shard n: only m:<uid> entries, shard = (uid-1)/shardsize
  fts/<shard>  omap: <field>:<term> -> posting list (doc IDs, delta varint)
  ftsd/<shard> omap: <trigram> -> terms           Dictionary for substrings
  ftsmap       omap: g:<guid> -> id, i:<id> -> guid, n  Doc ID mapping
```

* **Sharding.** A mailbox grows into new objects instead of into an
  ever-growing omap. The shard size is stored in the header so that no process
  with a different configuration reads the index incorrectly. Shard 0 is written
  atomically via CAS, other shards in two phases (first the header with modseq,
  UID and change log, then the entry). A crash in between is a
  UID gap plus an orphaned blob, both cases for the scrubber.
* **Change log.** Every write leaves a `c:<modseq>` with the
  affected UIDs. Before each command, a session reads only the header;
  if HIGHESTMODSEQ has moved, it fetches the log from its own position and the
  entries for the UIDs named there. A full reload stays reserved for the exception
  that `h:logstart` no longer covers the session's position.
* **Stable mailbox IDs.** The name is only a key in the registry,
  the index object hangs off the ID. RENAME, even of a whole subtree,
  is a single CAS write on the registry object and does not touch any
  message. The ID is at the same time the MAILBOXID for OBJECTID.
* **MODSEQ.** Every index write increases `h:modseq` in the same atomic
  write operation and stamps the changed entries. That makes
  CONDSTORE and QRESYNC complete in the data model; the vanished log keeps the
  expunged UIDs per modseq (bounded to the last 10000 entries).
* **Envelope cache.** The meta record contains the IMAP envelope, a
  200 character preview and the save date. List views, SORT and
  THREAD work without touching a blob.
* **Quota.** Counters on the registry object, checked before delivery and
  COPY, corrected after EXPUNGE. They live on a different object than
  the mailbox index and are therefore eventually consistent; a scrubber
  can recompute them from the meta records.

* **Index in omap.** omap is the key/value area that every OSD keeps per object
  in RocksDB. It is sorted, readable page by page (`startAfter`,
  prefix filter), and changes to one object are atomic. The 10 digit,
  zero-padded UID ensures that byte order == UID order.
* **Blobs immutable, in chunks.** A mail is written once in 4 MiB chunks
  (`msg/<guid>/<n>`) with `write_full`, in parallel across
  several OSDs/PGs; reading is parallel as well. That makes an
  erasure-coded pool work without `ec_overwrites`, keeps the latency of large mails
  from scaling linearly, and lowers the storage overhead for the bulk of the data
  from 3x to 1.5x (at 2+1; in production more likely 4+2 or 8+3).
* **Reference counting instead of copying.** Chunk 0 carries in the xattr `refs` the number
  of index entries that point at the blob. COPY increases the counter and
  creates an index entry, MOVE only moves the index entry,
  EXPUNGE lowers the counter and deletes the chunks only at 0. Payload is
  never copied. Deleting is therefore an omap delete plus one object delete
  per chunk, both O(1) and distributed across all OSDs.
* **Two pools, because omap only exists on replicated pools.** RGW separates
  `buckets.index` (replicated) and `buckets.data` (EC) in exactly the same way.

## Consistency without a lock service

All writes to a RADOS object are serialized by the primary OSD and
every object carries a version. The server uses that as a
compare-and-swap:

1. Read the header (`h:uidnext`) and remember the object version.
2. WriteOp with `assert_version(version)` + `omap_set(h:uidnext, m:<uid>)`.
3. If the assertion fails (someone else wrote in the meantime),
   read again and retry.

This lets arbitrarily many stateless pods deliver into the same mailbox
at the same time, without Redis, without a database, without a locking daemon. It is the same
mechanism that RGW uses for its bucket index shards.

Delivery writes the blob first, then the index entry. A crash
in between leaves at most an orphaned blob (collectible by the scrubber),
never a UID without data.

## Full-text search: "Flatcurve on RADOS"

Dovecot's flatcurve plugin keeps one Xapian database per mailbox on a
file system. That cannot be put on RADOS, but what
flatcurve delivers for IMAP can: SEARCH over TEXT, BODY, SUBJECT, FROM, TO
without reading any mail. For that, the POC keeps an **inverted index in
omap**:

* Per user, N shard objects `fts/<shard>` in the index pool. A term
  (`s:invoice`, `f:alice`, `b:sushi`, `h:…` for all headers) lands via
  `fnv1a(term) mod N` in exactly one shard. This keeps an object's omap
  small and spreads the load across PGs/OSDs.
* The value per term is the posting list: the document IDs of all mails with
  this term, sorted and encoded as delta varints (roughly one byte per
  entry instead of a 16 byte GUID; package `internal/postings`). The ID is assigned
  once per blob on the object `ftsmap` during indexing and released again on
  removal. Lists in the old format (raw GUIDs) are
  still read and converted on the first write; the format can be told apart
  by its length (raw lists are multiples of 16 bytes, the
  new encoding never is). Search = `omap_get_vals_by_keys` over the terms,
  intersection of the lists (boolean AND), resolution of the IDs to GUIDs in
  one bundled read, then reconciliation with the mailbox snapshot.
* **Indexing happens per GUID, not per (mailbox, UID).** COPY and MOVE
  therefore never touch the index; a mail is removed from the index exactly
  when its last reference disappears (refs=0).
* Updates are the same compare-and-swap writes as on the mailbox index:
  read terms, extend lists, write with `assert_version`. Arbitrarily
  many pods index at the same time.
* **Stemming.** Every token is additionally indexed as a Snowball stem (English
  and German, without language detection); the query is
  expanded the same way. `SUBJECT invoicing` finds "invoices",
  `BODY rechnungen` finds "Rechnung".
* **Substrings through a trigram dictionary.** Flatcurve answers
  wildcards through Xapian's term database. The counterpart here is N
  objects `ftsd/<shard>` whose omap maps every trigram to the list of
  terms containing it (`voi -> b:invoic, s:invoices, …`).
  A query `SUBJECT voic` reads the lists of `voi` and `oic`
  (one bundled read per affected shard, in parallel), intersects them,
  filters exactly via `strings.Contains` on the field and then fetches only the
  postings of the matching terms. **No mail is read for a search.**
  The dictionary grows with the user's vocabulary, not with the
  number of mails, and shrinks when a term's last reference goes away.
  Queries shorter than three characters fall back to a prefix scan of the sorted
  omap (`FROM li` finds "lic…", not "alice").
* **Parallelism.** All shard writes of one delivery (postings and
  dictionary) run concurrently as independent CAS operations; a
  conflict costs only the retry of that one shard. 40 concurrent
  deliveries into the same mailbox ran through without errors in testing.
* Deliberately no Xapian: no phrases, no ranking, no
  substring matches across token boundaries (`ce at` in "invoice
  attached"). Nested NOT/OR with text criteria fall back to reading the mail.

For production, the following would be added: n-grams instead of a tokenizer for CJK, a shard count
depending on the mailbox size, a per-GUID term list object once
re-tokenizing on delete becomes too expensive, and asynchronous
indexing (queue in omap) instead of synchronously in the delivery path.

## What the review forced

An external code review (Codex, `docs/review-round1.md`) found ten
findings with data loss potential. The answers to them are
now part of the design:

* **Versioned reads on private contexts.** librados provides the
  object version only per context (`get_last_version`). With parallel
  goroutines on one IOContext, a CAS could catch the version of another
  operation. Every versioned read now runs on a
  context from a small pool that it owns exclusively until the version is
  queried (`verpool.go`).
* **Read everything inside the CAS.** STORE reads existence, flags and MODSEQ of the
  entries within the header transaction, EXPUNGE removes only what
  still exists and releases references only for that.
* **Pending changes for shards.** A write to a higher shard
  is two-phase: the header CAS assigns the MODSEQ and announces the UIDs
  as `pending`, the decision (flags, UNCHANGEDSINCE, existence)
  is then made as a read-modify-write under the version of the shard object
  itself, and the commit records what was actually changed or removed.
  This makes parallel STOREs on the same message serialize,
  a late STORE does not re-create an expunged entry, and an
  EXPUNGE releases only references it removed itself. Readers
  stop before a pending entry (at most 30 s); a session
  with a fresh snapshot rewinds its position to before the oldest pending change.
  If the commit fails after the shard write, the entry stays
  valid and the announcement expires.
* **A refcount of zero is final.** The last EXPUNGE writes `refs=0`
  before deleting; a COPY against zero fails with "message is gone".
  Like COPY, MOVE first takes its own reference for the target and releases the
  source's references only once its entries have actually been
  removed; an EXPUNGE of the source between the two
  index steps cannot take the blob away from the target.
* **MODSEQ per message is monotonic.** The MODSEQ is assigned in the header CAS,
  but the shard write can land after that of a later STORE;
  the entry then keeps the higher MODSEQ. Because readers stop before a
  pending change, CONDSTORE sees no gap.
* **Snapshot boundary before the snapshot.** SELECT first reads the header and
  pending changes, then the contents; the session starts before the
  oldest pending change. The other way round, a commit between the two
  reads could hide a message permanently.
* **The scrubber aborts on an incomplete scan.** If an index
  could not be read completely, nothing is deleted and nothing is
  corrected. Refcount corrections are a CAS against the observed version.
  MOVE recovery removes the source only once every entry in the target
  is confirmed.
* **Memory budget.** Message buffers (delivery, APPEND, FETCH with
  a body, POP3 RETR) run against a process-wide budget
  (`MEMORY_BUDGET_BYTES`, default 256 MiB); beyond it, it waits at most 10 s,
  then the command fails temporarily. The wait time is
  bounded because a command holds partial reservations (all literals
  of one command, the steps of an LMTP message) and two such
  waiters would otherwise block each other.
* **Parser limits.** Line length, list nesting, argument count,
  literals per command (256) and command labels for metrics are
  bounded.
* **Quota is reserved, not checked.** The limit check and the increase of
  the counters are one CAS on the registry object; parallel deliveries
  and COPY batches see each other's reservations and cannot jointly exceed
  the limit. If the operation fails afterwards,
  the reservation is returned.
* **Delivery intents.** Sieve with several `fileinto` and MULTIAPPEND
  consist of several index updates. Before the first one, `d:<id>` is created on
  the registry object; every message is recorded with its mailbox ID and
  a GUID chosen in advance **before** it is stored, and at the
  end the intent is removed. Only once that has succeeded does the
  client see an OK; if it fails, everything is rolled back and temporarily rejected
  (otherwise the scrubber could delete confirmed mail). An intent older
  than ten minutes means a crashed process: the scrubber removes
  the messages by mailbox ID and GUID (never by UID, which after
  DELETE/CREATE could denote a different message); the client has
  seen no OK and retries.
* **A MODSEQ collision is re-stamped.** If a STORE writes its
  entry after a parallel STORE with a higher MODSEQ, the entry would carry
  the same MODSEQ as the earlier state; CHANGEDSINCE would miss the
  later change. The entry is therefore written again with a MODSEQ
  assigned afterwards.
* **Quota drift only on the second look.** Reservations of deliveries in flight
  are not yet visible in the index; the scrubber corrects
  the counter only when two runs counted the same value.
* **Unregistered index objects.** A process that dies between
  creating an `mbox/<id>` object and the registry entry
  leaves behind an object that nothing reaches (the ID is random). The
  scrubber removes such objects after the grace period. Found through
  an OOM kill under load: `EnsureMailbox` first created an object per LMTP delivery
  and deleted it again if the name was already taken;
  now the (cached) lookup comes first.
* **Orphans only on the second look.** The scrubber's scan and repair
  have no common locking point. An unreferenced blob is
  only noted with its object version on the first run (`scrub` object)
  and deleted only once a further run finds it unchanged and still
  unreferenced.
* **PendingFloor without a fixed window.** The start of the log scan is found
  by probing individual entries at doubling intervals
  (log2 reads until the first entry older than 30 s); a very active
  mailbox is thereby covered just as well as a quiet one.
* **VANISHED on a log gap.** Without known UIDs, the server reports all
  missing UIDs below UIDNEXT as `VANISHED (EARLIER)`, a
  superset that can contain UIDs the client never saw; for the
  client that has no consequences, and a full UID comparison is avoided.

## Scrubber

Every non-atomic spot in the design (blob before index, two-phase write
on shards, MOVE across two objects, quota on a different object) is
chosen so that a crash leaves only harmless debris. The scrubber
runs per user namespace, is therefore arbitrarily parallelizable, and
repairs exactly these cases with a grace period on the object mtime,
so that deliveries in flight stay untouched. It is a subcommand of the
server binary and runs as a CronJob with the same credentials.

## Notifications without a message bus

RADOS has watch/notify built in. After every index write, the store sends
a notify on the mailbox object, and an IMAP session in IDLE holds a
watch on it. A delivery on pod B reaches the IDLE session on pod A
cluster-wide in about 25 ms, without Kafka, Redis or a director. Notifies are
best effort; a 60 second ticker covers lost ones.

## Why this scales to millions of users

* **Front ends are stateless.** Every pod can serve every user;
  scaling is `replicas: N`. No director, no sticky routing needed
  (it only helps the cache).
* **No central bottleneck.** There is no "mailbox database"; the
  metadata of every mailbox lives on that mailbox's object and is
  distributed via CRUSH across all OSDs. Millions of mailboxes mean millions of
  small index objects, evenly distributed, not one table with
  billions of rows.
* **Namespaces instead of pools.** Ceph tolerates hundreds of pools, not millions;
  namespaces are unlimited.
* **EC for the bulk.** Mail is >95% blob bytes that are never
  changed. That is the ideal case for erasure coding.
* **No MDS layer.** CephFS would route every mailbox access through the MDS,
  whose metadata cache and single-active design (respectively subtree
  partitioning) are the well-known bottleneck with millions of small files.
* **No RGW hop.** S3 would route every fetch through HTTP + gateway and
  keep the index in RGW shards, whose listing semantics do not fit IMAP
  (UID ranges, flags).

## What production would add (no architectural change)

* **Large mailboxes:** one index object per mailbox is enough up to about
  10^5–10^6 entries (omap size). Beyond that, shard the index by
  UID ranges, exactly like RGW's `bucket_index_max_shards`.
* **Cache:** cache header/index snapshots per session in the pod; watch/notify
  on the mailbox object (RADOS has that built in) for IDLE instead of polling.
* **Scrubber/GC** for orphaned blobs (a crash between the blob and the
  index write, a crash between the two index writes of a MOVE), quota in the
  header omap.
* **Encryption** per user at the blob level; msgr2 secure mode for
  the transport (already assumed in the cluster: aes256k keys).
* **A dedicated object class (cls)** for "uidnext++ and set entry" in one
  server-side operation instead of a CAS retry; that is the route RGW takes
  (`cls_rgw`). Only needed once many parallel deliverers per mailbox drive
  the retry rate up.
* **Auth** against a real backend service (statically `test:test` here).

## Protocol layer

`internal/imapproto` is a dedicated IMAP implementation: a tokenizer for
atoms, quoted strings, literals (synchronizing, LITERAL+, LITERAL-,
BINARY `~{n}`) and lists; a dispatcher with a state machine
(not authenticated, authenticated, selected); an encoder for
envelope, body structure and literals. The layer speaks exclusively
`model.User`, a cgo-free interface that `cephstore.User` satisfies.
As a result, the entire protocol logic runs in `go test` against an
in-memory backend, without librados, and the same layer runs in the cluster
against RADOS.

The session only holds a snapshot of the mailbox index
(sequence number = position). Before each command, HIGHESTMODSEQ is read;
only on a change is the index reloaded and the difference reported as EXISTS,
EXPUNGE respectively VANISHED, and FETCH FLAGS. That is the bridge between
the MODSEQ in the data model and CONDSTORE/QRESYNC in the protocol.

## Authentication and director

The front ends only know `auth.Authenticator`: a login attempt goes in,
the canonical account comes out. The Nauthilus implementation preferably speaks
gRPC (`AuthService.Authenticate` / `LookupIdentity`) with basic or
bearer metadata and maps `AUTH_DECISION_FAIL` to a hard
login failure and `TEMPFAIL` plus transport errors to a temporary one. The
master user wrapper allows the nauthilus-director `<account>*director` with
the master password; the target account is nevertheless resolved via `LookupIdentity`,
so that aliases and normalization take effect. LMTP resolves every
RCPT via `LookupIdentity`, because the director passes the envelope address
on unchanged.

## Access rights and shared mailboxes

* ACLs (RFC 4314) live in the mailbox's registry entry at the owner
  (`MailboxRec.ACL`, identifier → rights; the owner implicitly has all of them).
  Every access checks against this entry; a mailbox without rights for
  the account does not exist for that account (NONEXISTENT instead of NOPERM).
* Entitled users find other users' mailboxes through references in their own namespace
  (`sh:<owner>/<name>` on the registry object), written by SETACL
  on the owner's side, re-pointed on RENAME, removed on DELETE and on
  revocation. The reference is a hint for LIST, never an authorization;
  LIST checks every reference against the ACL and cleans up stale ones.
  Access runs through a second user object for the owner's namespace,
  which the session holds until logout.
* Blobs are namespace-bound: COPY/MOVE into another user's mailbox reads
  and stores the message anew (the target's quota); within one
  namespace it remains reference linking.
* Flags in shared mailboxes are shared; a per-user `\Seen` would be
  a separate index entry per account and is not implemented.
* METADATA (RFC 5464) lives on an object `metadata` per namespace,
  keyed by mailbox ID (survives RENAME) and, for `/private/*`, by
  the accessing account. Server entries `/shared/*` live in the reserved
  namespace `~server` and are read-only for accounts.
* Rights apply to flags as well: a replacing STORE checks per message
  which flags are added or dropped; APPEND and COPY leave out flags
  for which the right is missing (RFC 4314 §4). Missing or not
  shared foreign mailboxes uniformly answer NONEXISTENT, so that
  their existence cannot be detected (TRYCREATE only within one's own
  namespace). COPY/MOVE across namespaces run under a
  delivery intent and are fully rolled back on errors.
* NOTIFY (RFC 5465) uses the same RADOS watches as IDLE: one pusher per
  session holds the connection's command lock while writing, which the
  command loop releases only while waiting for input (and in IDLE).
  That way notifications never mix into the response of a
  running command. At most 64 mailboxes are watched.

## Sieve

Scripts live as the omap of the object `sieve` in the user namespace, the
active script in an xattr. Delivery executes the active script and
files the mail into the resulting mailboxes (with flags); every
placement is a normal `Deliver`. A script error at runtime leads to
INBOX, never to loss.

## Operation in the cluster

* The server runs in the namespace `imap` (PSS `restricted`, non-root, read-only
  root, no capabilities). Ports 1143/2424 in the container, 143/24 at the service.
* cephx identity `client.imap` with minimal caps (`mon r`, `osd rwx` on
  exactly the two pools; `x` for the lock object class of the POP3 maildrop). Rook stores the key as a secret in `rook-ceph`; a
  Role there allows the ServiceAccount only `get` on this one
  secret and the mon ConfigMap. The server fetches both at startup through the
  Kubernetes API, nothing is copied manually, and key rotation through Rook
  works (pod restart).
* The image links against librados 20.2.4 (Tentacle), matching the cluster;
  aes256k keys require a Tentacle client with msgr2.
* If Rook rotates the key (`keyGeneration`), the server polls the secret and
  establishes a new connection; running sessions keep the old one until
  they end (reference counting on the connection).
* During a rollout, `/readyz` reports 503, sessions get `BYE`, the pod
  waits `DRAIN_SECONDS` (deployment: `terminationGracePeriodSeconds` 40).
