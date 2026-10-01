# RADOS data model and consistency

[Architecture](../../ARCHITECTURE.md) · [Developer guide](../development/README.md)

## Physical model

![Objects and pools](../diagrams/storage.svg)

The canonical account name selects a RADOS namespace inside shared
pools. Namespaces organize data; they are not per-user Ceph credentials
in their own right. Access control has to be correct earlier, in the
protocol/auth/ACL path. The server's Ceph account can reach several
user namespaces.

| Object/structure | Meaning | Primary source |
|---|---|---|
| `mailboxes` | Registry of names and stable mailbox IDs; quota, delivery intents and the intents of the scrub (`bi:`, `bimp:`, `up:`, `ci:`, `dm:`, `ps:`) | [user.go](../../internal/cephstore/user.go), [intents.go](../../internal/cephstore/intents.go) |
| `account` | Account-wide change counter and change log (JMAP state); watched by JMAP push | [account.go](../../internal/cephstore/account.go), [watch.go](../../internal/cephstore/watch.go) |
| `ix/<space>/<nn>` | Front-end indexes derived from the rest (JMAP locator), 32 shards | [index.go](../../internal/cephstore/index.go) |
| `mbox/<id>` | Header, UID counter, MODSEQ, change/vanished log and the first UID shard | [user.go](../../internal/cephstore/user.go) |
| `mbox/<id>/<n>` | Further message entries | [user.go](../../internal/cephstore/user.go) |
| `msg/<guid>/<n>` | Immutable payload chunks; chunk 0 carries `refs` | [blob.go](../../internal/cephstore/blob.go) |
| `fts/<shard>`, `ftst/<shard>`, `ftsmap` | Posting lists, trigram dictionary, GUID/doc ID mapping; `ftsd/<shard>` is the dictionary layout before 0.35.0 | [fts.go](../../internal/cephstore/fts.go) |
| Script/metadata objects | Sieve texts, the active script, private/shared metadata | [scripts.go](../../internal/cephstore/scripts.go), [metadata.go](../../internal/cephstore/metadata.go) |
| `scrub` | Candidates and repair state | [scrub.go](../../internal/cephstore/scrub.go) |
| `pubseen/<id>` | Private `\Seen` marks of the public folder `<id>` (registry key `ps:<id>`) | [acl.go](../../internal/cephstore/acl.go) |
| `~accounts` / `ix.uploads/…` in the index pool | Due queue of the JMAP uploads, by expiry | [uploads.go](../../internal/cephstore/uploads.go) |
| `~learning` / `learning/<job-id>` in the data pool | The independent message plus the job xattr | [learning.go](../../internal/cephstore/learning.go) |
| `~learning` / `ix.learning/…`, `ix.learning/owner/…` in the index pool | Due queue and owner index of the learning queue | [learnindex](../../internal/learnindex/learnindex.go) |
| `~accounts` / `ix.accounts/head`, `ix.accounts/<nn>` in the index pool | The account directory: one entry per account with a registry, 256 shards | [directory.go](../../internal/cephstore/directory.go), [sortedindex](../../internal/sortedindex/sortedindex.go) |

The index pool is replicated because its omap structures require it.
The data pool is EC 2+1 in the existing manifest. That is one concrete
operational configuration, not a general recommendation for every cluster size.
By default, messages consist of 4 MiB chunks; the count is part of
the metadata. `readBlob` uses the configured chunk size to
compute the offsets. It must not be changed casually while data exists.
The FTS shard count likewise affects the object mapping.

The mailbox shard size, by contrast, is stored in the mailbox header and is
fixed at creation time. For a UID, `shard = (uid - 1) / shardSize`.
Stable mailbox IDs decouple RENAME from the payload objects; names are
registry keys, not reliable physical object paths.

## Do not confuse identities

| Term | Scope | Meaning |
|---|---|---|
| Account | Auth/store contract | Canonical namespace owner |
| Mailbox ID | Registry of one account | Stable identity independent of the display name |
| UIDVALIDITY + UID | One mailbox | Identity of a message as seen by the IMAP client |
| GUID | A blob and its index references | Reference to shared message data |
| MODSEQ | Change history of a mailbox | Ordering value for synchronization and conditional changes |
| Account sequence | Change history of a whole account | Ordering value across mailboxes; what a JMAP state token names |
| FTS doc ID | Search index of one account | Compact posting list identity, not an IMAP UID |
| session_id | One network/backend session | Log correlation, not a persistent mail identity |

## The atomicity boundary

RADOS makes operations atomic **within a single object**. The writer reads the
state together with its version and writes with `AssertVersion`; on a conflict
it re-reads and decides again. The CAS helpers and the
[version context pool](../../internal/cephstore/verpool.go) prevent a
concurrent operation from using a version read by someone else.

There is no shared transaction across registry, mailbox header, higher shard,
blob, FTS and external program. Every change therefore has to define
which state remains visible after an abort and which
repair path can resolve it again.

## Writing a message

`DeliverGUID` checks the GUID and the size, resolves the mailbox and reserves
quota. It then writes the blob chunks, FTS and finally the mailbox entry.
A successful index entry must never point at payload data that has not been
written yet. Errors before linking trigger the designated cleanup and
quota refund paths; a process abort can leave residual state behind.

![Commit of a higher shard](../diagrams/shard-commit.svg)

Shard 0 can change the header and the entry in a single CAS. For higher shards,
the header first announces the change as pending. The entry is then written
and the journal is completed. Readers must not move their
MODSEQ watermark past incomplete changes.
`PendingFloor` and the synchronization in the protocol layer honor this.

If the entry is already durable but the concluding pending
commit fails, the caller must not delete the blob the way it would after an
early error: that would leave a visible entry without data.
Exactly this distinction is documented in the `link` path.

## COPY, MOVE and EXPUNGE

COPY creates an additional index reference and increments the blob reference
count. The payload does not have to be written again within this store
path. MOVE is not merely an atomic pointer switch: the code creates
a move intent, temporarily increments references, links the target,
removes the source and releases its reference. An abort leaves the
intent in place for repair. After a successful MOVE, the net
reference count is unchanged.

EXPUNGE removes mailbox entries and releases blob references. The last
reference leads to the deletion of the chunks. This is not a backup or
retention feature. `\Deleted`, a trash mailbox and physical deletion
are different states.

When several local delivery actions are involved, a delivery intent protects
their coherence. Its entries name the stable mailbox ID and the GUID
before the message is written. Orphaned intents are repaired or rolled back
after the designated age threshold. Intents do not guarantee
exactly-once delivery to an external client whose
success response can be lost on the network.

## Intents: debris that a scrub finds without listing

A crash between two writes can leave an object that nothing points at: a
blob written before its entry, a mailbox object created before its name
was registered, the shards of a mailbox whose deletion stopped half way.
Until 0.41 the scrub found such objects by listing the account's
namespace in both pools, which costs a walk over every placement group of
the pool per account. Since 0.42.0 every write that can leave such an
object first records an **intent** in an object the scrub reads anyway,
and removes it once the write is complete
([intents.go](../../internal/cephstore/intents.go)):

| Intent | Where | Set | Removed |
|---|---|---|---|
| `bi:<guid>` | registry | in the reservation CAS of `DeliverGUID` (delivery, APPEND, CATENATE, REPLACE, Sieve `fileinto`, COPY into another account), before the blob is written | once the entry is linked or the blob deleted again; batched, see below |
| `bimp:<batch>` | registry | in the reservation CAS of an import step, with every GUID the step may write (each message's GUID and two fallbacks) | after the header write of the step |
| `up:<guid>` | registry | before a JMAP upload's blob is written, after its entry in the uploads due queue | when the upload expires (`jmap.upload_ttl_seconds`) |
| `ci:<id>` | registry | in a CAS of its own before `mbox/<id>` is created | in the CAS that registers the name (or an import replacement's swap) |
| `dm:<id>` | registry | in the CAS that unregisters a mailbox, with its UIDNEXT and shard size | after the blobs are released and the shards deleted (highest first, shard 0 last) |
| `ps:<id>` | registry | before the first private `\Seen` mark of the public folder `<id>` is written | when the scrub removes the marks of a folder that is gone |
| `r:<uid>` | shard 0 of the mailbox | in the header CAS that removes the entry (EXPUNGE, MOVE, the scrub's own removals), with the blob's GUID, size and chunk count | after the blob reference is released |

A DELETE also **retires** the header of shard 0 after unregistering the
mailbox (`h:uidnext` 0, the old value kept as `h:retired`), so that a
delivery that looked the mailbox up before can no longer allocate a UID
in it; entries such a delivery added between the read of the messages
and the retirement are released as well. A delivery whose entry for a
higher shard was still on its way when the header was retired, or a
release that failed, keeps the tombstone for the scrub.

**What the scrub does with an intent.** An intent older than
`scrub.intent_grace_seconds` (3600) belongs to a write that will not
finish ([scrubintents.go](../../internal/cephstore/scrubintents.go)).
The scrub settles what it names against its index scan of the account
(the omap reads of every registered mailbox it does anyway, no listing):

- a blob that an entry references is fine and the intent goes; a blob
  nothing references and whose chunk 0 is older than
  `scrub.grace_seconds` is an orphan: its reference count is set to 0
  under the object version read (a COPY in between makes this fail, and
  the blob stays), then it is unindexed and its chunks are deleted.
- `r:` and `dm:` are **never** settled by releasing again: the scan
  counts every reference anew (`fixRefs`, with the same mtime guard and
  `AssertVersion`), so a release that did happen before the crash is not
  repeated, and a second decrement can never take the blob away from a
  copy in another mailbox. The mailbox of a `dm:` is not registered, so
  its entries do not count; its range comes from its retired header
  while shard 0 exists, else from the tombstone.
- a `dm:` whose blobs are settled loses its shards (highest first,
  shard 0 last), then the tombstone; a `ci:` whose id is not registered
  loses its object; a `ps:` whose folder is gone loses its marks.
- the METADATA entries of mailboxes that are no longer registered go as
  well (the registry is read after the entries; ids are never used
  again).

**Where the scrub looks** is `scrub.orphan_source`:

| Value | Listing | Acts on | Intents |
|---|---|---|---|
| `listing` | both pools, per account | the listing (two-run rule for blobs) | dropped once what they name is gone or referenced |
| `shadow` (default) | as `listing` | the listing | judged first, then compared with the listing; differences go to the report (`Shadow`), the log and `imapceph_scrub_shadow_differences_total{kind}` |
| `intents` | none | the intents | settled as above |

In shadow mode `orphan_missed`, `unknown_missed` and `marks_missed`
count what the listing found and no intent names: debris from before
0.42.0, or of a server of an older version during a rolling update, or a
write path without an intent (which would be a bug). `orphan_extra`
counts blobs the intents take for orphans and the listing does not; it
must stay 0. The switch to `intents` is safe once every server runs
0.42.0 or later, `orphan_extra` stayed 0, and what `*_missed` still
reports is debris from before (the sweep removes it, see
[operations](../operations/README.md#scrub-without-listings)).

**What it costs on the hot paths** (round trips to the OSDs added per
operation, against 0.41):

| Operation | Set | Remove |
|---|---|---|
| LMTP delivery (one view per delivery) | 0 (in the reservation CAS) | 1 key removal when the view closes |
| LMTP delivery to several mailboxes (Sieve), MULTIAPPEND | 0 | 0 (with the CAS that ends the delivery intent) |
| IMAP APPEND in a session | 0 | 0 (with the next reservation of the session), at the latest one write after 30 s or at logout |
| Delivery into a mailbox the quota rules ignore | 1 CAS | as above |
| EXPUNGE, MOVE (per command, not per message) | 0 (in the header CAS) | 1 key removal on shard 0 |
| CREATE | 1 CAS | 0 (in the CAS that registers the name) |
| DELETE | 0 (in the unregister CAS) | 1 CAS that retires the header, 1 key removal |
| JMAP upload | 1 due queue write + 1 CAS | the expiry |
| First private `\Seen` mark of a public folder | 1 read per session and folder, 1 CAS per account and folder | — |
| Import step | 0 (in the reservation CAS) | 1 key removal |

A key removal is one write op without a read; it bumps the object
version, so a concurrent CAS on the same object retries once. The
skip-if-idle check of the batch scrub reads the registry intents in one
read op (one bounded step per prefix) and each mailbox's `i:` and `r:`
in one more.

**The uploads.** A JMAP upload is a blob that nothing points at, so it
costs an entry in the due queue `ix.uploads` (namespace `~accounts`), the
intent `up:` and the blob, in that order. The upload expiry worker (one
server of the cluster holds its lease) deletes what is due every
minute; the scrub does the same for an expired upload the worker did not
get to, and a purge finds the uploads through their intents. Upload blobs
are never the blob of a message: `Email/import` and `Email/set` read
them and store a new message (`model.Blobs` has no way to link one;
`TestBlobsCannotLink`, `TestUploadBlobsAreNeverLinked`).

**Batched removal.** A view collects the keys of finished intents and
removes them with its next registry write that can carry them (a
reservation, the end of a delivery), after 30 seconds at the latest,
and when it is closed; LMTP, which opens a view per delivery, pays one
write per delivery, an IMAP session that appends many messages none.

## The account change log

A MODSEQ counts inside one mailbox and says nothing about the mailbox
next to it. That is what RFC 7162 specifies and it is all IMAP needs.
JMAP (RFC 8620) asks a different question — what changed anywhere in
this account since state token N — and answers it with a single number
per account and data type. The `account` object holds that number and
the log behind it:

| omap key | Meaning |
|---|---|
| `h:seq` | The account counter; the highest sequence ever allocated |
| `h:logstart` | The oldest sequence the log still holds |
| `p:<seq>` | A change that has been announced and not yet recorded |
| `a:<seq>` | The record of a change: kind, operation, mailbox, messages |

The log is a second record beside the mailbox change log, never a second
number space. An entry says which object changed and carries the account
sequence; the mailbox MODSEQ stays exactly what it is, and the mailbox
log remains authoritative for what happened inside a mailbox.

A mailbox object and the `account` object are two objects, so the
atomicity boundary applies: they cannot be written together. Every
change is therefore three steps, the same two-phase shape a higher-shard
write already uses:

1. the writer announces the change (`p:<seq>`, one CAS on `account`),
2. the mailbox change happens as before,
3. the writer records what it did and drops the announcement.

A reader never reads past the lowest announcement that has no record
yet, so a state token cannot advance over a change that is still being
written. An announcement whose writer died is turned into a *partial*
entry after 60 seconds: the mailbox is named, its messages are not, and
a client resynchronises that one mailbox. The paths that repair or
import (the scrubber, the account import) write partial entries directly,
because they run again if they die and no client is waiting on their
state token.

The direction of the guarantee is the point: the log may report a change
that did not happen, or report one without its details, but it must
never stay silent about a change that did. A client acts on a spurious
report by fetching something unchanged; it acts on a missing report by
never learning about a message.

The log is bounded like the mailbox change log
(`server.account_log_retention`, 10000 entries by default). A client
whose token is older than the oldest entry is told to resynchronise
instead of being handed a delta with a hole in it.

Two limits are deliberate at this stage. The private `\Seen` marks of a
public folder are recorded in the log of the public account, not in the
log of the account the mark belongs to; JMAP sharing (RFC 9670) is where
that is resolved. And METADATA annotations have no entry at all, because
RFC 8621 has no object for them.

## Front-end indexes

A front-end may keep an index of its own in the store: data it derives
from the rest and can always rebuild from it. The JMAP locator (GUID →
mailbox and UID) is the one that exists. Such an index carries no
consistency obligation towards the rest of the store — a lost write costs
a rebuild, never a message — and nothing on the IMAP write path writes to
it. It lives in `ix/<space>/<nn>`, spread over 32 shards by a hash of the
key, so that an account with a million messages keeps each object at
about thirty thousand keys, below the size at which Ceph warns about
large omap objects. The shard count is fixed for good: changing it would
move every key.

The scrubber leaves these objects alone, and an account purge removes
them with the rest of the namespace.

## Store-wide sorted indexes and the account directory

Listing a RADOS namespace visits every placement group of the pool, so
its cost grows with everything in the pool, not with what the namespace
holds. Enumerations that run routinely therefore read sorted indexes
instead; a listing is acceptable only in a repeatable backfill or a
manual tool. The metric `imapceph_rados_listings_total{site}` counts the
listings that remain (see [Observability](../observability.md)).

**Sorted index.** A store-wide map from ids to small values
([sortedindex](../../internal/sortedindex/sortedindex.go)), stored in a
system namespace of the index pool:

| Object | Content |
|---|---|
| `ix.<name>/head` | omap `shards`, `format`, `phase` (`stable`, or `copying:<n>` while a later reshard copies), `mark:<name>` markers |
| `ix.<name>/<nn>` | one omap key per id whose `FNV-1a(id) mod shards` is `nn` |

The names never start with `kv.` (older servers read those as admin
records) and are distinct from the per-account `ix/<space>/<nn>` above.
The first writer creates the head; readers without a head use the
defaults. Each process caches the head for ten seconds. A write refuses
while the phase is not `stable` (resharding is a later stage and this
version does not write the new layout); a newer format is refused
altogether. One entry is read, set or removed with one operation on its
shard object; an update is a compare-and-swap on the shard object's
version, because go-ceph offers no omap comparison; a short random
backoff separates the attempts, and each lost round counts as
`imapceph_rados_cas_retries_total{kind="index"}`. A listing returns the
entries in (shard, id) order, page by page, with the cursor
`<shard>:<id>`; a page reads the shard of the cursor and continues with
the following shards until it is full.

**Account directory.** The sorted index `accounts` in the namespace
`~accounts`, 256 shards. An account is in it exactly when its registry
object (`mailboxes`) exists; this includes `~public` and accounts that
only received a shared-folder pointer, and never the store's own
namespaces (`~admin`, `~learning`, `~server`, `~accounts`). The value is
JSON:

| Field | Meaning |
|---|---|
| `created` | When the registry was last created (the hook writes it on every creation); the registry's last change for accounts a backfill entered. Concurrent creators race, the last one wins: it is only a time. |
| `state` | `active` or `purging` |
| `session` | The purge job's maintenance lock session while `purging` |
| `cluster` | fsid of the Ceph cluster that holds the account |
| `shard` | Reserved for a later stage (placement), empty |
| `scrub` | The record of the last batch scrub of the account: `at`, `state` (`seq` and `reg`, see below), `complete`, `findings`; absent before the first. A new registration of the account drops it. |

The store registers an account in the compare-and-swap that creates its
registry (`casOmapOpAs`, version 0, the account's own index context),
right before the exclusive create: one read and one write (an `active`
entry is written again with the new `created`). That happens once per
account lifetime, so an existing account pays nothing. `shardpost index backfill accounts` enters the
accounts that existed before the directory (see
[Operations](../operations/README.md#account-directory-backfill)).

`Store.User` refuses the empty name and reserved names other than
`~public`, `~server`, `~admin`, `~learning` and `~accounts`: the empty
name would put a registry into the default namespace, where it belongs
to nobody.

**Purge.** The import lock that keeps protocols out of an account lives
in its registry, so it disappears when the purge deletes the registry.
Without the directory, a late session or delivery could then create the account
again. Now the purge job sets the entry to `purging` with its lock
session (`job-<id>`) while it still holds the lock, before it deletes the
namespace. From then on, while the registry is missing, the registry
hook refuses every creation with `ErrAccountPurging`, a kind of
`ErrImportLocked` (LMTP answers 451, IMAP `NO [UNAVAILABLE]`), except an
`ImportLock` of the job's own session, so that a restarted job can take
its lock again. That
restart leaves an empty registry with only the lock key behind, which is
why the last purge phase stops renewing the lock, fences, deletes the
registry once more (absent is fine) and only then removes the entry.
After that the name is free; a later account of the same name is a new
account. While the registry exists, `purging` does not block anything
beyond the import lock itself.

A grant (SETACL, the admin ACL operations, JMAP sharing) to an account
that is `purging` without registry is refused with `ErrAccountPurging`
(an `ErrImportLocked`; the admin API answers `409 account_purging`)
before the ACL is written: the pointer write into the grantee's registry
would fail, but the ACL entry would stay and pass to whoever gets the
name next. A removal of rights goes through and writes no pointer. The
check narrows the window, it does not close it: a grant whose check ran
just before the purge deleted the grantee's registry still writes its
ACL entry, and its pointer write then fails (`ErrPointerSync`). Closing
it would need the ACL write and the grantee's state in one atomic step,
which two objects in two namespaces cannot give.

**Repair.** `shardpostctl account repair` and the backfill apply these
rules:

| Entry | Condition | Action |
|---|---|---|
| `purging` | a purge job of its session still runs | none (`purge_running`) |
| `purging` | no such job, registry missing | entry removed (`removed`) |
| `purging` | no such job, registry exists | back to `active` (`activated`) |
| `active` | older than 10 minutes, registry missing | entry removed (`stale_removed`) |

A stale `active` entry comes from a purge by an older version during the
rollout, from a crash between the registry hook and the registry
creation, or from a creation whose write failed. The grace keeps a
repair away from an account that is being created right now. The
removal is a compare-and-swap on the entry the repair read, including
its `created`, and the hook writes the entry (with a new `created`)
before it creates the registry. So a creation racing the repair always
keeps its entry: if the hook writes first, the repair finds the entry
changed and leaves it; if the repair removes first, the hook finds no
entry and writes one.

While an account is `purging` without registry, no exclusive job can
take its lock, a second purge included. A job that finds the account
left so by an ended purge fails at once with "run shardpostctl account
repair" instead of waiting for the lock.

**Rollback.** Older servers ignore `~accounts` (the scrub's namespace
listing skips reserved names), and an older purge sets no `purging`, so
for its purges the behaviour before the directory applies; it leaves the
entry `active`, which the repair later removes as stale.

**Reshard (later stage).** Resharding the directory will announce itself
in the head (`phase: copying:<n>`). Servers of this version refuse every
directory write in that phase, so creating an account then fails with a
temporary error (LMTP 451). A reshard therefore requires every server to
run a version that writes during the copy phase first.

## Batches over all accounts and the batch scrub

A **batch** is a maintenance job without account (`Account ""`, kind
`batch`) that runs one kind on every account of the directory
([jobs/batch.go](../../internal/jobs/batch.go)). Only a batch may be
account-less, and `Store.User("")` stays refused: the runner opens no
account for it, the batch opens each account in turn. The kinds a
batch can run are `index_check`, `scrub`, `fts_rebuild` and
`attachment_backfill`; they do not depend on a plan of the account (a
batch has one plan for all of them). The role and the exclusivity of a
batch are those of its kind. A batch is planned only while the
directory carries a completed backfill marker, since otherwise accounts
from before the directory would be missed.

The batch walks the directory in its order (shard, account) a page at a
time; a page holds four accounts per worker (at least 16). The accounts
of a page run with a bounded parallelism (`jobs.batch_parallel`, for a
scrub `scrub.parallel`); after each page the directory cursor
`<shard>:<account>` and the counters go into the job's checkpoint, so a
server that takes the job over redoes at most one page (all batch
kinds may run twice on an account). An entry `purging` whose registry
is gone counts as `gone`; with a registry it counts as active, as the
directory semantics say. Before each account the batch fences on its
lease, so a cancel request or a lost lease stops it between two
accounts. An exclusive kind takes the account's maintenance lock (the
batch's lock session `job-<id>` for every account), keeps it renewed
every third of the lease and waits `jobs.settle_seconds` before it
works; an account that an import or another job holds is put aside and
tried again after the walk, up to three times a lease period apart,
and then counted as failed. The result counts the accounts (`done`,
`skipped`, `gone`, `failed`, `findings`) and names the first 50 that
failed or had findings, the record stays far below the 4 MiB of an
admin record however many accounts there are.

**Skip-if-idle.** The nightly scrub is a batch of `scrub` with
`skip_idle`. After it scrubbed an account with every repair class, the
batch writes the account's scrub record into its directory entry: the
account counter (`h:seq` of the account object) and the object version
of the registry, both read **after** the scrub's own writes (the quota
fix, the orphan candidates, the quota count), whether the run was
complete (no import, no error, the index scan complete) and how many
findings it had. A run that found nothing but saw the state move during
the scrub (a concurrent change) records itself as incomplete. A later
batch with `skip_idle` leaves the account alone only when all of this
holds:

1. the account counter and the registry version are the ones recorded;
2. the `scrub` object is empty: no orphan candidates and no quota count
   wait for the second run of the two-run rule (a run that finds the
   quota counters right removes a count an earlier run left);
3. the recorded run was complete;
4. no intent is open: none of `d:`, `bi:`, `bimp:`, `up:`, `ci:`, `dm:`
   in the registry, no MOVE intent `i:` and no removal intent `r:` on a
   mailbox object; this costs one read op of the registry and one per
   mailbox, no listing;
5. the recorded run had no findings;
6. it is younger than `scrub.interval_days` (7): after that the account
   is scrubbed again in any case.

The cheap conditions (3, 5, 6) are checked from the directory entry
before the account is read at all. Accounts that are not skipped are
scrubbed with the configured orphan source (see
[intents](#intents-debris-that-a-scrub-finds-without-listing)): with
`listing` and `shadow` including the listing of their namespace in the
data pool (orphan blobs) and the index pool (unknown objects), with
`intents` without any listing.

**What skipping still misses.** Since 0.42.0 every write that can leave
debris records an intent in the registry or on a mailbox object, so an
account with debris of a crash is not idle (condition 4) and is
scrubbed. What a skipped account can still hold until its next scrub, at
the latest `scrub.interval_days` later: the private `\Seen` marks
(`pubseen/`) of public folders that were deleted in `~public` meanwhile
(their `ps:` key is no open intent), and debris that writes of servers
before 0.42.0 left without an intent. None of it is visible to a client;
it only takes space until then.

`shardpost scrub` (the run in the process itself) takes its accounts
from `scrub.users`, else from the directory once it carries a completed
backfill marker, else from a listing of the index pool.

## Admin store: hint indexes and due queues

The admin store ([adminkv](../../internal/cephstore/adminkv.go)) keeps
one object per record, `kv.<key>` in the namespace `~admin`: jobs
(`job/<id>`), plans (`plan/<id>`), idempotency keys (`idem/<hash>`), the
JMAP push subscriptions of an account (`jmap/push/acct/<account>`) and
smaller state. Records stay objects in every mode. Next to them live
sorted indexes ([kvindex](../../internal/kvindex/kvindex.go)) that spare
the job runner, the expiry of old records and the push worker the
listing of the namespace (which visits every placement group of the
pool).

| Object | Content |
|---|---|
| `ix.job/…` (1 shard) | hint index of `job/`: `{state, account, kind, finished}` |
| `ix.plan/…` (1 shard) | hint index of `plan/`: `{kind, account, expires}` |
| `ix.idem/…` (16 shards) | hint index of `idem/`: `{job_id}` |
| `ix.jmap/push/acct/…` (64 shards) | hint index of `jmap/push/acct/`: `{verified, expires}` (any subscription verified, the latest expiry) |
| `ix.jobs/claimable/…` | due queue of the jobs a runner may have to claim |
| `ix.jobs/expiry/…` | due queue of the jobs, plans and idempotency keys that expire |
| `ix.jobs/expiry/lock` | cls_lock of the expiry worker |
| `ix.kv/lock` | cls_lock of `shardpost index backfill kv` |

The head of `ix.job` carries the backfill markers `backfill.kv` (the
report of the last completed backfill, with its start time) and
`backfill.kv.progress`.

**Hint index.** An entry carries what its readers need of the record,
so a reader that filters (jobs by account and state) reads only the
records that match. The hint rule orders the two writes: on create the
record first, then the entry; on delete the entry first, then the
record. A crash in between leaves a record without entry, which the
backfill adds, or an entry without record, which readers skip and
remove. The entry is written after a write that created the record or
changed its index value (a heartbeat does not), and it is computed from
the record as it is then: the index update reads the record inside its
compare-and-swap on the shard object, so of two writers of one record
the later index write reflects the later record. A failed index write
does not fail the record write; it is logged and counted
(`imapceph_kv_index_errors_total`), and the backfill repairs it.

**Due queue.** A completeness index of what becomes due at a time
([duequeue](../../internal/sortedindex/duequeue.go)): every record that
has to be visited has an entry, so a worker finds what is due by
reading the heads of at most eight shard objects instead of listing.
The omap key is `<due>/<id>/<gen>`: the due time as 20-digit Unix
nanoseconds (keys sort by due time), the id, and the generation of the
record it was written for. The shard is `FNV-1a(id) mod n` of the id
alone, so all entries of one id live in one shard object and moving an
entry is one write op (`RmOmapKeys` of the old key plus `SetOmap` of the
new one). `n` is `admin.kv_queue_shards` (default 4, at most 8) when
the first writer creates the queue; the head keeps it after that. `Due`
reads the first page of every shard, keeps the keys up to now and
merges them, earliest first. Writers create an entry before its record
and remove it after the record. Moves are not compare-and-swap (go-ceph
has no omap comparison); the record stays the fence, and readers
compare an entry with its record: an entry without record or with a
generation newer than the record's is skipped for a grace of ten
minutes (its writer may be between the two writes) and removed after
it; an entry that does not match its record's position is moved there.

**Jobs queue** (`jobs/claimable`). A queued job is due at its creation
time (generation 0), a running one at the end of its lease (generation
`Lease.Gen`). Every write that changes the lease end or the state moves
the entry after the record, from the position computed from the record
as it was read in the compare-and-swap to the one of the record as
written:

| Writer | Record | Queue |
|---|---|---|
| submit | created (`Created` is the entry's time, same nanosecond) | entry at `Created` written before the record; notify |
| claim | `Lease.Gen`+1, `Lease.Until` = now + lease | moved to the new lease end and generation |
| heartbeat | `Lease.Until` extended | moved |
| postpone (account locked) | `Lease.Until` = now + lease | moved |
| release at shutdown | `Lease.Until` = now | moved to now; notify |
| cancel of a queued job | ended at once | removed |
| cancel of a running job | `cancel_requested` | unchanged (`claimable()` does not change) |
| end | final state | removed after the record |

So the entries due now are the jobs `claimable()` selects. A reader
(`admin.kv_index=read`) that finds an entry whose record is not
claimable (a heartbeat's record write succeeded, the process died before
the move) moves it to the record's lease end; an entry of an ended job
is removed; a claim removes the entry it came from together with the
record's own. The compare-and-swap on the record stays the fence.

**Wake instead of poll.** A writer notifies shard 0 of
`jobs/claimable` when an entry becomes due earlier than before: a
submit, and a release at shutdown. In `read` mode every runner watches
that object and then claims from the queue at once; its poll
(`jobs.poll_seconds`) stays as the fallback, and a watch that reports
an error (the OSD dropped it, or the store rotated its connection) is
set up again on the next poll. In `write` mode runners do not watch:
their look for jobs is still a listing of the admin namespace, and a
wake-up per submit on every server would multiply those listings; they
find new jobs by the poll as before. `imapceph_job_wakeups_total{cause}`
counts the looks by cause. Watch errors are coalesced into one pending
error per watch, so the librados thread that reports them never waits
for the runner.

**Expiry** (`jobs/expiry`). The entries carry the retention of
`cleanup()`:

| Record | Entry |
|---|---|
| job | placeholder at `Created + retention` (generation 0) at submit; at the end moved to `Finished + retention` (generation 1) |
| idempotency key | the same positions as its job (the job record names the key, `idem_key`); the placeholder is written before the key itself |
| plan | `Expires + retention` |

One server of the cluster runs the expiry worker: it holds the
cls_lock `ix.jobs/expiry/lock` instead of taking it per step. The lock
lives `jobs.lease_seconds` + 120 s and is renewed with `MAY_RENEW` every
third of that, so a renewal that waits out the OSD op timeout (60 s)
still lands before the lock runs out; releasing it waits for a renewal
in flight before the unlock, so a late renewal cannot take the lock
again. Every
`jobs.expiry_seconds` deletes what is due. For a job it deletes the
idempotency key first (only while the key still names this job; absent
is fine), then the job (a compare-and-swap that checks the job is still
ended and expired), then the entries. A job that has not ended when its
placeholder is due is rescheduled one retention later; an idempotency
key whose job was never stored (a crash between the two creates, which
used to block the key for good) is deleted when its placeholder is due.
A plan is deleted at `Expires + retention`. The lock is advisory: every
deletion is a compare-and-swap on the record. When the store rotates
its cephx key (`Store.Rotate`), the worker's lock is unlocked through
the old connection while its key is still valid and reported lost; the
worker takes it again on the new connection. A lock whose holder died
stays held until its TTL (`jobs.lease_seconds` + 120 s) ran out. A key
that `Due` cannot parse is skipped and counted
(`imapceph_queue_stale_entries_total{action="unparsable"}`).

**Key rotation.** The admin store is opened once and kept for the
process lifetime by the job runner, JMAP and the push worker. It does
not hold one connection for good: it is a view registered with the
store, and `Store.Rotate` (a new cephx key from Rook) reopens it on the
new connection and swaps it in. Requests that already run finish on the
old view, which is closed once they are done (so the old connection
shuts down); the queue watch on the old view gets an error and the
runner watches again on the new one. No restart is needed after a key
rotation. `Store.Rotate` releases each held worker lock only after a
renewal in flight returned, so a rotation can take up to the OSD op
timeout (60 s) per held lock.

In `write` and `off` mode `cleanup()` (a listing of the jobs and plans)
keeps running on every runner poll beside the expiry worker. In `read`
mode it does not run: after the backfill `jobs/expiry` knows every
record, and the expiry worker alone deletes what is due. So in `read`
mode no running path lists the admin namespace: the runner claims from
`jobs/claimable`, job lists (admin API, `shardpostctl job list`) and the
push worker's scans read the hint indexes, and the expiry worker reads
`jobs/expiry`. Only `shardpost index backfill kv` lists
(`imapceph_rados_listings_total{site="admin_kv_backfill"}`); a listing
under `admin_kv` in `read` mode is a prefix that is not an indexed
collection.

**Modes** (`admin.kv_index`):

| Mode | Writes | Reads |
|---|---|---|
| `off` | no index, no queue, no expiry worker | listings (the behaviour before) |
| `write` | hint indexes, both queues, expiry worker, notifies | listings; the runner claims from the listing; `cleanup()` on the poll |
| `read` (default) | as `write` | listings of an indexed collection come from its hint index (key order, merged across shards; entries without record are removed); job lists filter on the index values; the runner claims from `Due`; no `cleanup()` |

`read` is only correct after `shardpost index backfill kv` completed
with a start after the last server without the indexes stopped: records
such a server wrote are missing from the indexes until then. A server
configured for `read` therefore checks the completion marker
`backfill.kv`: while it is missing, the server logs a warning at start
("admin.kv_index=read without a completed …") and behaves as in
`write` mode (it lists, runs `cleanup()` and does not watch the
claimable queue). It looks for the marker again every 30 seconds and
switches to `read` once it exists, without a restart. The server cannot
tell when the last older server stopped; that the backfill started
after it is the operator's part of the rollout (see
[Operations](../operations/README.md#admin-store-backfill)).

**Rollback.** Older servers ignore the `ix.*` objects (they list only
`kv.*`) and keep reading by listing; the records are unchanged, the job
record only gains the field `idem_key`. A job that an older server ends
leaves its queue entries behind; the readers of this version remove
them (claimable: the record has ended; expiry: the record is gone or is
rescheduled to its real expiry).

## Learning queue: due queue and owner index

A learning job ([imapsieve](../imapsieve.md#durable-learning-queue)) is
a record of its own: `learning/<id>` in the namespace `~learning` of
the data pool holds the message, the job is its xattr `job`. The data
pool is erasure coded and has no omap, so the indexes that spare the
worker and the purge the listing of that namespace live in the namespace
`~learning` of the index pool
([learnindex](../../internal/learnindex/learnindex.go)):

| Object (index pool, `~learning`) | Content |
|---|---|
| `ix.learning/head`, `ix.learning/<n>` | due queue `learning`: `<next attempt>/<id>/<gen>` → empty; `imapsieve.learning.queue_shards` shards (default 4) |
| `ix.learning/owner/head`, `ix.learning/owner/<nn>` (16 shards) | hint index of the jobs by owner: `<account>/<id>` → empty |
| `ix.learning/lock` | cls_lock of `shardpost index backfill learning` |
| `mbox/learning-worker` | cls_lock `maildrop` of the worker (the object and name every version locks) |

The head of the due queue carries the backfill markers
`backfill.learning` and `backfill.learning.progress`.

**Due queue.** A completeness index: every job has one entry. A live
job sits at its next attempt, a dead one at the largest due time the
key can hold (`09223372036854775807`, the year 2262), after every live
entry, so it is never due and readers of the due entries never reach
it. (The plan called this the `dead/` prefix; a prefix on the id would
put the entry into another shard, so the move would no longer be one
write.) The generation is the job's `Gen`, which every write through the
queue counts up; servers without the index drop the field when they
rewrite a job.

| Writer | Record | Queue | Owner index |
|---|---|---|---|
| enqueue (event, prepared COPY/MOVE) | created exclusively; an existing job is left alone | entry at its next attempt written before the record (removed again when the record exists already at another position); notify | entry after the record |
| attempt failed (retry, backoff) | `Attempts`, `Next`, `Gen`+1 | moved after the record | unchanged |
| dead (attempts exhausted, permanent error, unconfirmed after a day, oversize) | `Dead`, `Gen`+1 | moved to the dead position | unchanged |
| retry of a dead job (admin API) | `Dead` false, `Next` now, `Retries`+1, `Gen`+1 | moved to now; notify | unchanged |
| delivered, purge | removed | removed after the record | removed before the record |

**Worker.** One server of the cluster holds the cls_lock `maildrop` on
`mbox/learning-worker` in the index pool and keeps it (renewed every
third of its two minutes) instead of taking it per pass. Servers of
older versions take the same lock per pass, so during a rolling update
only one worker sends jobs to the learning programs. When the store
rotates its cephx key the lock is unlocked through the old connection
and reported lost; the worker takes it again on the new connection. A
lock whose holder died stays held until its TTL ran out. In `read` mode
the worker reads at most 100 due entries per pass from the heads of the
queue shards, reads each job's record and compares: an entry without
record is skipped and removed after ten minutes (the entry is written
before the record); an entry the record does not match (an older server
rewrote the job) is moved to the record's position. It watches shard 0
of the queue, which an enqueue and a retry notify, and keeps its poll of
one second as the fallback; a broken watch is set up again at the next
poll. `imap_imapsieve_learning_wakeups_total` counts the passes a
notify started.

**Readers.** In `read` mode the admin API lists jobs from the owner
index (all of them, or the prefix `<account>/`) and reads each record;
an entry whose record is gone is removed. Show and retry read the
record by id in every mode. An account purge deletes the jobs the owner
index names. In `write` mode all three list the namespace as before
(`imapceph_rados_listings_total{site="learning_all"}`,
`{site="learning_purge_owner"}`).

**Modes** (`imapsieve.learning.index`, default `write`):

| Mode | Writes | Reads |
|---|---|---|
| `off` | no index, no queue | listings (the behaviour before) |
| `write` (default) | due queue and owner index | worker, admin list and purge list the namespace |
| `read` | as `write` | worker from `Due`, woken by notify; admin list and purge from the owner index |

`read` needs the marker of a completed `shardpost index backfill
learning` that started after the last server without the index stopped
(jobs such a server enqueued have no entries until then). Without the
marker the server logs a warning at start and behaves as in `write` mode
until the marker exists; it looks for it every 30 seconds. The backfill
lists the namespace once (`{site="learning_backfill"}`) under the
cls_lock `ix.learning/lock`, reads every job again and sets its queue
and owner entry; it can be repeated and resumes after an interruption.
It then removes the queue entries whose job is gone: those at the dead
position, which no reader of due entries reaches, and those due for
longer than ten minutes. A delete that finds the job already gone
removes the id's entries at the dead position as well (one bounded read
of its shard).
See [Operations](../operations/README.md#learning-queue-backfill).

**Rollback.** Older servers ignore the index objects and the field
`Gen`; a job they rewrite or delete leaves its entry behind, which
readers of this version move or remove.

## Notifications and search

Watch/notify is a wake-up signal. The mailbox header, the
change journal and the entries remain authoritative. A full snapshot becomes
necessary when the journal no longer covers the requested time range. Watches
must be stopped before their store view is closed.

The IMAP sessions of a process share their watches through one watch hub
([watchhub](../../internal/watchhub/watchhub.go)): one RADOS watch per
object, on a store view of the hub's own, fanned out to every session
that follows the object. IDLE follows the mailbox object of the selected
mailbox. NOTIFY follows the account object (`account`) and reads what
changed from the account change log: the mailboxes the new entries name
get their STATUS (one header read each), the selected one its poll; when
the log no longer covers the position the session read up to, or an
entry names no mailbox, every notified mailbox is looked at. A selected
mailbox of another account or of the public namespace is followed
through the owner's account object. When an account object cannot be
watched, NOTIFY falls back to the mailbox objects. A watch that reports
an error (the OSD dropped it, or the store rotated its connection) is set
up again by the hub on a fresh view, retried with a growing delay (the
first failure is logged as a warning, the further ones at debug level),
and its followers are woken to look for what they may have missed. NOTIFY
also reads the logs it follows every 60 s without a notify, which bounds
the delay of a change recorded after its notify (an announcement whose
writer died is resolved after 60 s) or missed while a watch is set up
again. A connection holds at most `server.imap_max_watches`
subscriptions, one of them reserved for IDLE; NOTIFY SET is refused with
`NO [LIMIT]` when its fallback watches do not fit.

The full-text index works per GUID. Field terms and DE/EN stemming are stored
in posting lists; a trigram dictionary supports substring searches.

| Object | omap key | Value |
|---|---|---|
| `fts/<nn>`, `server.fts_shards` of them (16), shard = fnv1a(term) | term, `<field>:<token>` (`s:invoice`) | posting list: document IDs, sorted, delta-varint |
| `ftst/<nnn>`, 256 of them, shard = fnv1a(trigram) | `<trigram>:<term>` (`voi:s:invoice`), one key per trigram of the token | empty |
| `ftsmap` | `g:<guid>`, `i:<id>`, `n` | document ID, GUID, next ID |

A term that is new for the account costs one dictionary key per trigram
of its token (at most 38, a token has at most 40 characters). The keys
are set without reading the object and without a version check, because
setting a key twice changes nothing; what a delivery or an import batch
writes to the dictionary therefore depends on its new terms and not on
the size of the vocabulary. The 256 shards are fixed for good, like
those of `ix/`, and keep an object below the key count at which Ceph
warns for a dictionary of tens of millions of keys.

A query token of three or more characters is looked up by scanning the
key range `<trigram>:<field>:` of each of its trigrams, a page of 1024
keys of every range at a time, until the first range ends: every term
that contains the token is listed under each of its trigrams, so one
complete range, filtered for the token, is the whole answer. A shorter
token is looked up as a prefix in the posting shards.

The field `h` (every header name and value) has posting lists but no
dictionary keys. Most of its vocabulary occurs once — message IDs,
timestamps, queue IDs — and a substring index over it would grow with
the number of messages rather than with their words. In a word search
(the JMAP `header` filter, the admin `text` search, IMAP with
`fts.imap_search: word`) a token searched in `h` matches a header word
exactly, by its stem or as a prefix (a prefix lookup reads every
matching term, 64 per round trip and shard), and as a substring where
the word is in the subject or an address (`s`, `f`, `t`), which have
dictionary keys. Subject, addresses and body match by substring.

Up to 0.34.6 the dictionary was `ftsd/<nn>` (as many shards as `fts/`)
with one key per trigram and the newline-separated list of its terms as
the value, capped at 2000 terms. A value was read and rewritten whole
for every term added to it, which made the cost of a new term grow with
the vocabulary ([report](../reports/2026-09-30-shardpost-0.34.5.md)).
Nothing writes this layout any more. It is still read in addition to
the new one, so an account finds its older terms by substring until the
job `fts_dict_convert` has written the dictionary again from the posting
lists and deleted the old objects; `fts_rebuild` does the same from the
messages. A server older than 0.35.0 does not read `ftst/`: after a
downgrade, substring search needs an `fts_rebuild`.

What is indexed is UTF-8 text, prepared by the `fts` package
([body.go](../../internal/fts/body.go), `HeaderValue`, `HeaderText`)
for the index, for IMAP `SEARCH` keys checked against the message and
for the cached preview, so they agree: header names and values
(unfolded, RFC 2047 words in any charset of the WHATWG list, raw 8-bit
text in `fts.fallback_charset`), every `text/*` part converted from its
charset (`text/html` reduced to its visible text, `fts.html`), the file
names of all parts and the header and text of attached messages, at
most 4 MiB of text per message. Details are in the
[configuration](../configuration.md#full-text-index-fts).

A word of more than 40 characters has no term of its own
([query.go](../../internal/fts/query.go) states and proves the rules,
`TestLongWordWindows` checks them). Its first 40 characters are a term
in every field, so every word start stays findable. In the fields with
the dictionary it also gets the 40-character windows that start at
every multiple of 20 and the window that ends with the word, at most
eight: every inner part of up to 21 characters lies in one of them.
Two marker terms stand for what the windows do not cover: `<field>:~~`
(a word of more than 40 characters, for inner parts of 22–40
characters) and `<field>:~+` (a word longer than its eight windows,
180 characters). Neither can be a query token, and neither has
dictionary keys.

The header store `ftsh/<nn>` keeps `HeaderText` per message — the
decoded header lines that IMAP `SEARCH` checks header keys against —
compressed with DEFLATE, at most 64 KiB of text (a longer header is not
kept). It is written with the posting lists (delivery, `APPEND`, JMAP,
import, `fts_rebuild`) and removed with them (expunge, purge, mailbox
deletion, the scrubber's orphans, `DeleteFTS`); keyed by GUID, it is not
touched by `COPY` or `MOVE`. Like the rest of the index it does not
count against the quota.

| Object | omap key | Value |
|---|---|---|
| `ftsh/<nn>`, `server.fts_shards` of them, shard = fnv1a(GUID) | GUID | format byte 1, DEFLATE of the header text |
| `ftsmap` | `v`, `r` | index version marker `2`; `r` (empty) while `fts_rebuild` runs |

IMAP `SEARCH` matches substrings (RFC 9051 6.4.4). The index cannot
answer that alone — stems, words in any order and the missing
dictionary of `h` make it find too much or too little — so by default
(`fts.imap_search: substring`) it supplies candidates and every
candidate is checked. `SearchCandidates`
([ftssearch.go](../../internal/cephstore/ftssearch.go)) returns a
superset of the messages that contain the value, or nothing when it
cannot narrow. Each word of the value is looked up by how it can occur:
a word between two separators of the value as that term; the last word
as the start of a term, by a scan of the posting shards for keys with
that prefix (keys only, a page of 1024 per round trip, complete because
every term has its key in exactly one posting shard); the first word,
which may be the end of a longer word, through the dictionary. The last
two add the messages with the marker the word needs. A word does not
narrow when it cannot be looked up completely: an inner part in `h`, of
fewer than three characters, a lookup of more than 4096 terms, an
account still on the dictionary layout before 0.35.0.

The version marker `v` in `ftsmap` says that every message of the
account was indexed by this version. A fresh index gets it with its
first document ID — only when the id map has no `n` yet, no rebuild is
running and no posting shard exists (an index from before document IDs
has posting lists of GUIDs and no `n`). `fts_rebuild` sets `r` to a token of its run right
after deleting the old index and, when every message is indexed again,
sets `v` and removes `r` in one compare-and-swap — only if `r` still
holds its own token, so a rebuild that another one replaced does not
mark the index that one is still writing. A rebuild that stops midway,
or a delivery into the index it is rebuilding, leaves no trusted
marker. Without `v`, or with `r`, `SearchCandidates` does not
narrow at all — an older index (markup, undecoded words, no file
names) cannot be trusted even for whole words — and every message is
checked: correct, but slow, until the rebuild.

The protocol layer ([searchtext.go](../../internal/imapproto/searchtext.go))
asks for candidates separately for the header and the body of every
text key, wherever it is in the tree, and combines them: `AND`
intersects, `OR` unites, `NOT` does not narrow. It checks the
candidates `fts.verify_parallel` at a time: header keys against the
header store (one omap read per shard for 256 messages; a message
without a stored header has the first 16 KiB to 1 MiB of its blob
read), the body by reading the message under the memory budget, only
where the body candidates include it. The cost of a search is
therefore index lookups, header-store reads for the messages a header
key cannot narrow — at worst every message of the mailbox, as for a
`TEXT` value that may begin inside a header word — and message reads
for the messages whose body may match. A key the index cannot narrow at
all (`BODY ab`) reads every message. With `fts.imap_search: word` the
index answers `SUBJECT`, `BODY`, `TEXT` and `HEADER Subject` alone, as
earlier versions did. JMAP and the admin `text` search always use the
index alone.

## Repair and its limits

The scrubber checks references, quota, orphaned blobs,
missing blobs and aborted intents, among other things. Age thresholds, object
versions and multi-stage candidate handling protect operations in flight. A dry
run is the diagnostic option to use before intended repairs.

Scrub is not a complete backup and no substitute for restore
tests. A missing blob cannot be reconstructed from its index entry.
Backup/restore across both pools as well as a coordinated data migration
have to be qualified as operational procedures of their own.

For store changes, always describe the success, conflict and abort paths:
what is durable? What does a reader see? Who cleans up? A successful
in-memory protocol test does not answer these questions for RADOS.
