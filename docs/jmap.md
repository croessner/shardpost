# JMAP

[Documentation](README.md) · [Architecture](../ARCHITECTURE.md) · [RADOS storage](architecture/storage.md)

JMAP is roadmap stage 6: a third front-end beside IMAP and POP3, on the
same store. It is a protocol layer on top of `model.User`, not a second
data store — everything it answers comes from the same index, the same
blobs and the same account change log that IMAP reads. The staged plan
behind it is `temp/08-jmap-staged-plan.md`.

## What exists

| Stage | Contents | State |
|---|---|---|
| 0 | The account change log: one state per account, and the changes behind it | built |
| 1 | Core (RFC 8620): session resource, API endpoint, authentication, blobs | built |
| 2 | `Mailbox/*` | built |
| 3 | `Email/get`, `Email/changes`, `Thread/*` | built |
| 4 | `Email/query`, `queryChanges`, `Email/set` | planned |
| 5 | Push (EventSource and Web Push), `EmailSubmission/*`, `Identity/*` | planned |
| 6 | `SieveScript/*`, `Quota/*`, `Blob/*`, sharing | planned |

A client that asks for a capability this server does not have is told
`unknownCapability`, and a method that is not implemented answers
`unknownMethod`. Nothing is served half-correct.

The session advertises `urn:ietf:params:jmap:mail` from stage 2 on,
because a method is only reachable through the capability it belongs to
and `Mailbox/*` would otherwise be unusable. The capability is therefore
ahead of what is behind it: a client that asks for `Email/query` or
`Email/set` today is told `unknownMethod`. That is what keeps `jmap.addr` unset until
stage 4 — the capability gate makes the methods testable, the
deployment gate keeps real clients away.

## The account state

IMAP counts MODSEQ per mailbox; JMAP wants one number per account and
data type. The store keeps an account change log for it, described in
[RADOS storage](architecture/storage.md#the-account-change-log). Two
consequences matter outside the store:

- Every write path now writes to the account log as well. That is the
  price of serving two state models from one store, and it is paid on
  every delivery, flag change, copy, move, expunge and mailbox change,
  whether or not the account has a JMAP client.
- The log is bounded (`server.account_log_retention`, 10000 entries).
  A client whose state token is older resynchronises.

## Mailboxes

`Mailbox/get`, `/changes`, `/query`, `/queryChanges` and `/set` (RFC
8621 2). Everything they report comes from what IMAP already keeps:
`role` from the RFC 6154 special-use attribute (and from the name, for
INBOX), `isSubscribed` from the subscription, `myRights` from the fact
that every mailbox in the session belongs to the account itself.
`sortOrder` is the one property with no IMAP counterpart; it is a field
of the registry record that nothing else reads.

**Names and the tree.** IMAP names a mailbox by its path, JMAP by a leaf
plus a pointer to its parent. IMAP also allows a path whose intermediate
levels have no mailbox of their own, and JMAP has no object for such a
level, so those levels stay in the name: `Work/Bills` without a `Work`
is a mailbox named `Work/Bills` at the top level. The invariant either
way is that the path is the parent's path, the delimiter and the name. A
`Mailbox/set` refuses a name that carries the delimiter, so a client
cannot create that shape itself.

**Counts.** `totalEmails`, `unreadEmails`, `totalThreads` and
`unreadThreads` cost a scan of the mailbox index — the same scan an IMAP
`STATUS` costs, and Thunderbird does one of those per mailbox per poll.
They are computed only for the properties a request actually names, so a
client that lists its mailboxes without the counts pays nothing. A draft
does not count as unread, as RFC 8621 2 defines it. `maxObjectsInGet`
bounds how many mailboxes one `Mailbox/get` may render, and
`maxObjectsInSet` how many objects one `/set` may touch.

**Changes.** `Mailbox/changes` reads the account change log; the state
token is the account counter. Three things land in `updated`:

- a mailbox that was written (renamed, subscribed, its rights or its
  sort order changed),
- a mailbox whose messages changed, because the counts are properties of
  it — `updatedProperties` then names the four count properties, and a
  client refetches nothing else,
- a mailbox whose *neighbours* changed. A JMAP name and parent are read
  off the tree around a mailbox, so a level that appears or disappears
  renames everything under it without any of them being written. Those
  children are reported too, and `updatedProperties` is then null.

An entry that records a change without naming its mailbox — which
happens only after a process died mid-write, or during an account
import — makes the call answer `cannotCalculateChanges`, and the client
re-reads the list. The alternative, diffing the client's list against
the current one, is possible with a few dozen mailboxes and is not built
until it is needed.

**What `Mailbox/set` will not do.** The role of a mailbox is fixed when
it is created: the store has no way to change a special-use attribute
afterwards, and inventing one for JMAP alone would put IMAP and JMAP at
odds. An account has at most one mailbox per role, which IMAP does not
enforce and JMAP requires, so a second one is refused here. INBOX can be
neither renamed nor deleted (RFC 9051 6.3.4, 6.3.6), and `myRights` says
so before a client tries. A mailbox with children is `mailboxHasChild`,
and one with messages is `mailboxHasEmail` unless the client passes
`onDestroyRemoveEmails`.

An update is applied in one piece or not at all: everything a client
sent is decoded and checked before the first write, so a rename that
fails cannot leave a sort order behind. A creation may name its parent
by the creation id of another creation in the same call, in whatever
order the JSON object is read; the ids they receive go back in
`createdIds`.

**Sorting** compares bytes, and the session advertises `i;octet` alone
rather than the collations this server does not actually apply. A
comparator that asks for another one is `unsupportedSort`.

**`Mailbox/queryChanges`** answers `cannotCalculateChanges`, and
`Mailbox/query` says so up front with `canCalculateChanges: false`: the
query result is not a list this server keeps, so a client re-runs the
query instead of being handed a delta computed from a guess.

## Messages

`Email/get`, `Email/changes`, `Thread/get` and `Thread/changes` (RFC
8621 3 and 4).

**The id of a message is its GUID** — the identity IMAP reports as
`EMAILID` (RFC 8474). It does not change when a message moves, which is
what lets a client move a message by changing its `mailboxIds`; an id
made of mailbox and UID would turn every move into a deletion and a new
message. It also means that an IMAP `COPY`, which keeps the GUID, puts
one message into a second mailbox rather than making a second message:
`mailboxIds` then holds both, and `maxMailboxesPerEmail` has no limit.
A message copied twice into the *same* mailbox is one message in it —
JMAP cannot say "twice here", and the second copy is not a message of its
own.

**Where a message is.** The store keys its entries by mailbox and UID and
has no way back from a GUID. The JMAP front-end keeps a *locator* for
that: a derived index GUID → places, in 32 shard objects per account
(`ix/jmaploc/<nn>`, see [RADOS storage](architecture/storage.md)). It is
built from the account change log, which already records every placement
with its mailbox, UID and GUID, so **nothing on the IMAP write path
writes to it** — the JMAP front-end folds the log into it before it
reads. The first JMAP request of an account builds it from a scan of all
mailboxes, and so does a request after the log has been trimmed past the
point the locator had reached (a JMAP client that was away for more than
`server.account_log_retention` changes). That scan reads every index
entry once; after it, each request folds only what changed. Every place
the locator names is checked against the mailbox before it is used — by
`Email/get`, `Email/changes`, `Thread/get` and `Thread/changes` alike —
and a place that no longer holds its message is dropped, so a stale
entry costs a lookup and never a wrong answer.

The locator keeps a small record (about a hundred bytes) for every
message it has ever seen, including the ones that are gone:
`Thread/changes` needs the thread of a destroyed message, and the record
is where it is. Thread records only grow, for the same reason a removal
could race an addition. An account therefore accumulates records at the
rate it receives mail, spread over 32 objects; at a million messages over
its lifetime that is about thirty thousand keys per object, well below
Ceph's large-omap threshold. An account far beyond that can have its
locator dropped (`ix/jmaploc/*`) while no JMAP client is connected; the
next request rebuilds it from what exists.

**Keywords** are IMAP flags under their JMAP names (RFC 8621 4.1.1):
`\Seen`, `\Flagged`, `\Answered` and `\Draft` become `$seen`, `$flagged`,
`$answered` and `$draft`; `\Recent` and `\Deleted` are not exposed; every
other flag is an IMAP keyword already and keeps its name in lower case.
A message whose IMAP copies carry different flags has the union of them
— read in one folder is read — and a keyword write in stage 4 sets every
copy alike.

**What is read.** The properties of a list view come from the index
record without touching the message: `subject`, `from`, `to`, `cc`,
`bcc`, `sentAt`, `receivedAt`, `messageId`, `inReplyTo`, `preview`,
`keywords`, `size`, `threadId`, `mailboxIds`. Three header properties
read the header block of the message, because the IMAP envelope in the
index gets them wrong for JMAP: it fills an absent `Sender` and
`Reply-To` from `From`, where JMAP says null, and it merges `References`
with `In-Reply-To` for threading. `header:*` and `headers` read the
header block as well. The body properties — `bodyStructure`,
`textBody`, `htmlBody`, `attachments`, `bodyValues`, `hasAttachment` —
read and parse the whole message, which is what an IMAP `BODYSTRUCTURE`
costs too. A list view that asks for `hasAttachment` therefore reads
every message it shows.

**Bodies.** `textBody`, `htmlBody` and `attachments` come from the
algorithm RFC 8621 4.1.4 gives, ported as written. A body value is
decoded from its transfer encoding and converted from its charset by
this server, which sets `isEncodingProblem` when either step fails, and
`maxBodyValueBytes` cuts on a character boundary. A body part's blob is
its bytes with the transfer encoding undone and the charset left alone —
what a client saves as the file — and its id is the message GUID with the
section number (`<guid>_2-1`). The download endpoint serves it by parsing
the message; a part is not stored on its own.

**Threads** are the messages that share a conversation root: the first
`References` entry, or the message's own `Message-ID`, or the message
itself. That is the same function IMAP uses for `THREADID`
(`mailmeta.ThreadID`), so the two protocols cannot disagree. It depends
only on fields that never change, so a message's `threadId` never
changes either; changing the function would change every thread id and
is therefore a breaking change. A thread's `emailIds` are sorted by
arrival.

**Changes.** `Email/changes` counts placements rather than trusting the
operation of a log entry: a new placement of a message that already
existed — an IMAP `COPY`, or the arrival half of a `MOVE` — is an update
of the message, not a creation. The number of places now (from the
locator, checked) less what the window added gives the number before;
a message is created when it had none, destroyed when it has none left.
A window cut short by `maxChanges` is counted against the places of now,
which can mislabel inside that window — never a message reported
destroyed that exists — and the pages that follow bring the client to
the right end state. `Thread/changes` reports the threads a message
joined or left; a message whose keywords or mailboxes change stays in
its thread.

## Endpoints

| Path | Method | Contents |
|---|---|---|
| `/.well-known/jmap` | GET | The session resource (RFC 8620 2) |
| `/jmap/api/` | POST | Method calls, batched, with back-references |
| `/jmap/upload/{accountId}/` | POST | Blob upload |
| `/jmap/download/{accountId}/{blobId}/{name}` | GET | Blob download, with HTTP ranges |
| `/jmap/healthz` | GET | Backend check: no login, no side effects |

The account id is the canonical account name. It is already stable,
unique within the server and the thing every log line names; an opaque
number would only add a mapping to keep correct. Only the account of the
authenticated user is listed in the session — other accounts (shared
mailboxes, the public namespace) belong to JMAP Sharing, which is stage
6 of the plan.

A blob id is the GUID of the message data, the same identity IMAP
reports as `EMAILID`. An uploaded blob that nothing references is
debris: the scrubber removes it once two consecutive runs have seen it
unreferenced, which is the expiry RFC 8620 6.1 allows. Two consequences
an operator should know: the expiry is the scrub cadence, not a time, so
a deployment that does not schedule `shardpost scrub` never collects
them; and an upload is only refused when the account is already over its
quota, because an unreferenced blob is counted nowhere until something
references it. A tighter bound belongs with `Blob/*` (RFC 9404) in stage
6, where blob lifetime is a protocol matter rather than a guess.

A download is served with `X-Content-Type-Options: nosniff` and a
sandboxing `Content-Security-Policy`, because the media type is whatever
the client asked for and must never be interpreted as a document of this
origin. A request for an account other than the authenticated one is a
404 rather than a 403: the server does not confirm that the other
account exists.

## Authentication

Basic and Bearer, both verified against the same Nauthilus backend the
other front-ends use (`internal/auth`). The server verifies every
request itself even when a Director in front has already done so, so
that it is correct without a Director as well.

A Bearer token needs `nauthilus.bearer_mode: introspect`: the token has
to yield the account, and the `password` mode has no user name to go
with it. Authentication on a plaintext connection is refused unless
`jmap.insecure_auth` is set.

## Configuration

JMAP listens on an address of its own rather than sharing
`server.http_addr`: that one serves metrics and the health probes and is
cluster-internal, while JMAP is customer traffic with its own
certificate and its own network policy. Without `jmap.addr` the
front-end is not started at all.

```yaml
jmap:
  addr: ':8443'
  tls_cert: /tls/jmap/tls.crt
  tls_key: /tls/jmap/tls.key
  public_base_url: https://jmap.example.org
  max_size_upload: 52428800
  max_size_request: 10485760
  max_concurrent_upload: 4
  max_concurrent_requests: 4
  max_calls_in_request: 16
  max_objects_in_get: 500
  max_objects_in_set: 500
```

`tls_cert`/`tls_key` fall back to `tls.cert`/`tls.key` when they are
empty. A limit left at zero takes the built-in default; the session
resource advertises whatever is in force, so a client adapts rather than
guessing.

`public_base_url` is the address clients reach. Behind the Director that
is the Director's address and not this pod's: its default mode
(`backend_advertised`) rewrites nothing, so the session resource has to
advertise URLs that work from outside. Without the setting the URLs are
built from the request, which is right for a direct connection only.

## Limits of this stage

- `Email/query`, `Email/set`, `Email/copy`, `Email/import`,
  `SearchSnippet/get` and the rest of RFC 8621 beyond reading are not
  here, although the mail capability is advertised (see above). Its
  `emailQuerySortOptions` is therefore an empty list.
- `Email/get` needs ids: asking for every message of an account is
  answered with `requestTooLarge`.
- `hasAttachment` reads the whole message (see above). Caching it in the
  index record at delivery would make list views cheaper; it is not done
  yet.
- A message beyond the limits of the MIME walk (more than 1000 parts, or
  nested deeper than 32 levels) is described as one opaque part a client
  can download, and a message whose data cannot be read has no body
  properties; neither holds back the other messages of the call.
- Two small deviations that come from answering from the IMAP envelope:
  a present but empty `Subject` reads as null rather than `""`, and an
  address without a domain (`<postmaster>`) is left out of `from`, `to`
  and the other address properties, because the envelope encodes it the
  way it encodes the start of a group. `header:*:asAddresses` parses the
  header itself and has neither deviation.
- Push is not here: the session advertises an `eventSourceUrl`, and
  stage 5 makes it answer.
- The private `\Seen` marks of a public folder are recorded in the
  account log of the public account, not of the account the mark belongs
  to. JMAP Sharing (RFC 9670) is where that is resolved.
- `Email/queryChanges` is decided, not built: it will answer only for
  the default sort and return `cannotCalculateChanges` otherwise, rather
  than carrying a materialised index per sort key.
