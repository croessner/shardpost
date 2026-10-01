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
| `mailboxes` | Registry of names and stable mailbox IDs; quota and delivery intents | [user.go](../../internal/cephstore/user.go) |
| `account` | Account-wide change counter and change log (JMAP state); watched by JMAP push | [account.go](../../internal/cephstore/account.go), [watch.go](../../internal/cephstore/watch.go) |
| `ix/<space>/<nn>` | Front-end indexes derived from the rest (JMAP locator), 32 shards | [index.go](../../internal/cephstore/index.go) |
| `mbox/<id>` | Header, UID counter, MODSEQ, change/vanished log and the first UID shard | [user.go](../../internal/cephstore/user.go) |
| `mbox/<id>/<n>` | Further message entries | [user.go](../../internal/cephstore/user.go) |
| `msg/<guid>/<n>` | Immutable payload chunks; chunk 0 carries `refs` | [blob.go](../../internal/cephstore/blob.go) |
| `fts/<shard>`, `ftst/<shard>`, `ftsmap` | Posting lists, trigram dictionary, GUID/doc ID mapping; `ftsd/<shard>` is the dictionary layout before 0.35.0 | [fts.go](../../internal/cephstore/fts.go) |
| Script/metadata objects | Sieve texts, the active script, private/shared metadata | [scripts.go](../../internal/cephstore/scripts.go), [metadata.go](../../internal/cephstore/metadata.go) |
| `scrub` | Candidates and repair state | [scrub.go](../../internal/cephstore/scrub.go) |
| `~learning` / `learning/<job-id>` in the data pool | The independent message plus the job xattr | [learning.go](../../internal/cephstore/learning.go) |

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

## Notifications and search

Watch/notify is a wake-up signal. The mailbox header, the
change journal and the entries remain authoritative. A full snapshot becomes
necessary when the journal no longer covers the requested time range. Watches
must be stopped before their store view is closed.

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
has posting lists of GUIDs and no `n`). `fts_rebuild` sets `r` right
after deleting the old index and, when every message is indexed again,
sets `v` and removes `r` in one compare-and-swap; a rebuild that stops
midway, or a delivery into the index it is rebuilding, leaves no
trusted marker. Without `v`, or with `r`, `SearchCandidates` does not
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
