# JMAP

[Documentation](README.md) · [Architecture](../ARCHITECTURE.md) · [RADOS storage](architecture/storage.md)

JMAP is roadmap stage 6: a third front-end beside IMAP and POP3, on the
same store. It is a protocol layer on top of `model.User`, not a second
data store — everything it answers comes from the same index, the same
blobs and the same account change log that IMAP reads. The staged plan
behind it is `temp/08-jmap-staged-plan.md`. [JMAP by
example](jmap-examples.md) shows a recorded send and receive as JSON.

## What exists

| Stage | Contents | State |
|---|---|---|
| 0 | The account change log: one state per account, and the changes behind it | built |
| 1 | Core (RFC 8620): session resource, API endpoint, authentication, blobs | built |
| 2 | `Mailbox/*` | built |
| 3 | `Email/get`, `Email/changes`, `Thread/*` | built |
| 4 | `Email/query`, `Email/queryChanges`, `Email/set`, `Email/import`, `Email/parse`, `Email/copy` | built |
| 5 | Push (EventSource and Web Push), `Identity/*`, `EmailSubmission/*` | built; delayed sending prepared, not offered |
| 6 | `Quota/*`, `VacationResponse/*` | built |
| 6 | `Blob/*`, `SieveScript/*` | built |
| 6c-1 | Sharing of mail: shared mailboxes and public folders as accounts, `Email/copy` between them | built |
| 6c-2 | `Principal/*`, `shareWith`, `ShareNotification` (RFC 9670) | built |
| 6c-3 | Push for shared accounts and share notifications; shared states that move with visible changes only | built |

A client that asks for a capability this server does not have is told
`unknownCapability`, and a method that is not implemented answers
`unknownMethod`. Nothing is served half-correct.

The session advertises `urn:ietf:params:jmap:mail` from stage 2 on,
because a method is only reachable through the capability it belongs to.
With stage 4 the capability is complete for reading and writing mail;
`SearchSnippet/get` is the one method of RFC 8621 5 that is still
missing (see the limits below). Whether `jmap.addr` is set, and real
clients reach the front-end, is an operator decision from here on, not
a gap in the code.

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
that every mailbox of the personal account belongs to the account itself
— in a shared account from the ACL (see [Sharing](#sharing)).
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

**Sorting** by `name` uses `i;ascii-casemap` unless a comparator asks
for `i;octet`; the session advertises these two, the collations this
server actually applies. A comparator that asks for another one is
`unsupportedSort`.

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
— read in one folder is read — and a keyword write through `Email/set` sets
every copy alike.

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

## Queries

`Email/query` and `Email/queryChanges` (RFC 8621 4.4 and 4.5).

A query is over messages, not over placements: a message in two
mailboxes is one result, and its keywords and mailboxes are those of all
its copies, as `Email/get` reports them. **Where the candidates come
from decides the cost.** A filter limited to one mailbox at the top
(`inMailbox`, alone or in a top-level `AND`) reads that mailbox's index
and looks up only the messages the locator knows to have copies
elsewhere. A filter whose top level holds a `text`, `subject` or `body`
condition takes its candidates from the full-text index, which is kept
per account and per GUID, and finds them through the locator. Anything
else — no filter, `inMailboxOtherThan`, a keyword or date condition on
its own, an `OR` at the top — reads the index of every mailbox of the
account. That is the price of having no materialised per-account
message list, and it is the same read `Email/changes` avoids by using
the log.

The conditions:

| Condition | How it is answered |
|---|---|
| `inMailbox`, `inMailboxOtherThan` | The mailboxes of the message's copies |
| `before`, `after`, `minSize`, `maxSize` | The index record (`receivedAt`, `size`) |
| `hasKeyword`, `notKeyword` | The keywords across the copies |
| `allInThreadHaveKeyword`, `someInThreadHaveKeyword`, `noneInThreadHaveKeyword` | The keywords of every message of the thread, through the locator, once per thread and query |
| `text`, `subject`, `body` | The full-text index, with the word semantics IMAP `SEARCH` has here; body text converted to UTF-8 and HTML reduced to its visible text (see [configuration](configuration.md#full-text-index-fts)) |
| `from`, `to`, `cc`, `bcc` | The full-text index narrows by the words of the value (`to`, `cc` and `bcc` share an index field); the envelope decides: every word has to occur, without regard to case, in `name <address>` of one address |
| `header` | The full-text index narrows a value; the header block of each candidate decides, its values decoded as the index decodes them (encoded words, raw 8-bit text in `fts.fallback_charset`). A name alone, or a value without an indexable word, reads the header of every candidate |
| `hasAttachment` | Refused with `unsupportedFilter`: it would mean parsing every candidate |

An unknown condition or operator, and a node that is both an operator
and a condition, is `unsupportedFilter`, never silently ignored — an
ignored condition would match everything. A `text`, `subject` or `body`
value without a word the index can look for (`"!!!"`) matches every
message, as an empty IMAP `SEARCH TEXT` does.

The sort properties are the ones `emailQuerySortOptions` lists:
`receivedAt`, `size`, `from`, `to`, `subject`, `sentAt`, `hasKeyword`,
`allInThreadHaveKeyword` and `someInThreadHaveKeyword`. Text keys are
compared with `i;ascii-casemap` unless a comparator asks for `i;octet`;
the session lists both in `collationAlgorithms`, and `Mailbox/query`
sorts names the same way. `subject` sorts by the base subject IMAP
`SORT` uses (`mailmeta.BaseSubject`), `from` and `to` by the name of the
first address, or its email when it has none. Equal
keys fall back to arrival, newest first, and then to the id, so a query
answers in the same order every time. `collapseThreads` keeps the first
message of each thread in that order.

**`Email/queryChanges` answers for the default order only** — arrival,
newest first, without `collapseThreads` and without a thread keyword
condition — as decided when the plan was agreed. In that order a
message's position depends only on its arrival, which never changes,
and on which other messages match, which changes only for the messages
the log names. The query is run again as it stands; every message the
log records a change of since the client's state is reported in
`removed`, and those that match now are in `added` at their position.
RFC 8620 5.6 allows `removed` to name ids that were not in the old
result, and this is where it does. Any other order, a collapsed one, a
thread condition (whose match can change with another message of the
thread), a state the log no longer covers, a change recorded without its
messages, or a change that is announced and not yet recorded is
`cannotCalculateChanges`, and the client runs the query again.
`Email/query` says as much in `canCalculateChanges`. `upToId` is
accepted and not used: the delta is computed over the whole result.
Carrying a
materialised index per sort key would lift the restriction, at a write
on every delivery; it is not done.

## Writing messages

`Email/set`, `Email/import`, `Email/parse` and `Email/copy` (RFC 8621
4.6 to 4.9).

**An update changes where a message is and which keywords it has**;
every other property of an Email is immutable and an update of it is
`invalidProperties`. Both are carried out with the IMAP operations the
store already has, so an IMAP client sees a JMAP change as it would see
another IMAP client's:

- A keyword change is a `STORE` on every copy. The flags that are not
  keywords (`\Deleted`, `\Recent`) stay, and a keyword a copy keeps
  keeps its spelling.
- A change that adds exactly one mailbox and removes others is a `MOVE`
  (from the first removed mailbox in id order). Any other change of
  `mailboxIds` copies first and expunges afterwards, so a failure
  half-way leaves the message in too many mailboxes, never in none.
- When copies go and the update does not set keywords, the copies that
  stay are given the keywords the message had: under the union rule,
  removing the one copy that was `\Seen` would otherwise make the
  message unread.
- A destruction expunges every copy.

Both the whole property and paths into it (`keywords/$seen`,
`mailboxIds/<id>`) are accepted; giving both for one property is
`invalidPatch`. A mailbox id may be the creation id of a mailbox made
earlier in the same request.

**A creation is written by the server.** The client describes the
message with the header properties (`from`, `subject`, …, and
`header:*` in any form the field allows) and either a `bodyStructure`
or `textBody`, `htmlBody` and `attachments`, with the content in
`bodyValues` or in blobs — uploads, stored messages, or body parts of
them. From the three lists the server builds the usual tree: text and
HTML as `multipart/alternative`, inline parts with a `cid` related to
the HTML, attachments mixed in after. It adds `Date`, `Message-ID` (on
the sender's domain) and `MIME-Version`, encodes non-ASCII header text
in RFC 2047 words and non-ASCII file names by RFC 2231, and picks the
transfer encoding itself: text that is ASCII in lines of legal length
as it is, other text quoted-printable, everything else base64. The
message is stored in the first of its mailboxes and copied to the
others, with the keywords as flags and `receivedAt` as the arrival
(default: now); when a copy fails, what was stored is expunged again, so
a creation is all or nothing. The constraints of RFC 8621 4.6 are
enforced rather than repaired — a field given twice (also once on the
Email and once on the root body part), a `Content-*` field on the Email,
a `Content-Transfer-Encoding` or `MIME-Version` anywhere, a `charset`
next to a `partId`, a `header:*` name that is not a field name, a word
too long to fold — and a missing blob is `blobNotFound` with every
missing id. Content is counted as the parts are read: a creation whose
bodies add up to more than `maxSizeAttachmentsPerEmail` (which is
`max_size_upload`) is `tooLarge` before anything is put together, and a
blob referenced several times is read once. There is no limit on the
number of keywords or mailboxes, so `tooManyKeywords` and
`tooManyMailboxes` do not occur.

**`Email/import`** stores an uploaded message byte for byte, in the
mailboxes and with the keywords given — line endings and anything else
are not normalised, as they are not for an LMTP delivery; `receivedAt`
defaults to the date of the topmost `Received` field, else to now. A
blob without a header is `invalidEmail`. Duplicates are allowed:
importing the same blob twice makes two messages with ids of their own.

**`Email/parse`** renders a blob as the Email it would be, without
storing it: `id`, `mailboxIds`, `keywords` and `receivedAt` are null,
and `threadId` is the thread it would join when it has a `Message-ID` or
`References`, null otherwise. A body part of type `message/rfc822` can
be parsed in turn by its blob id, and the parts of that message get blob
ids with one more level (`<guid>_2_1-2`) that the download endpoint
serves.

**`Email/copy`** copies between accounts: the personal one and the
shared ones (see [Sharing](#sharing)). An account the user cannot reach
is `fromAccountNotFound` on the source side and `accountNotFound` on the
destination side, the same account on both sides `invalidArguments`
(RFC 8620 5.4). Within an account, a copy is an update of `mailboxIds`.

**The new state** of a write advances over the log entries that name
only the call's own messages. Such an entry can still be another
session's — an IMAP `STORE` on the message the call just moved — so the
messages are read back afterwards, and whatever differs from what the
call meant is reported in `created` or `updated` (RFC 8620 5.3 lets the
server report properties it changed beyond the request): keywords and
`mailboxIds` as they are now. A destroyed message that is still there
keeps the old state. A client that adopts the new state therefore ends
up at least as far as the state says, never behind it.

## Sharing

Stage 6c-1. The mailboxes other accounts share with the user, and the
public folders, are accounts of their own in the session (RFC 8620
1.6.2), beside the personal one. They are the same mailboxes IMAP shows
under `Shared/<owner>/` and `Public/`, with the same rights: RFC 4314
ACLs, set over IMAP `SETACL`, `shardpostctl acl` or JMAP `shareWith`
(see [Sharing a mailbox](#sharing-a-mailbox-sharewith)).

The store view of another namespace checks no permissions — IMAP
enforces them in its protocol layer, and JMAP does the same in its own
(`internal/jmap/account.go`). An account of another namespace is built
from the mailboxes the user may see in it, with the rights it holds on
each, and everything the account answers comes from that set: its
mailbox tree has the visible mailboxes only, its messages are found only
where the user may read them, and every write checks the right it needs
before anything is written. Whatever is not allowed is refused rather
than approximated.

**Accounts.** Besides the personal account the session lists:

- one account per owner who shares at least one mailbox the user can see
  (right `l`). Its id and name are the owner's account name.
- the public folders, when the user can see at least one: the id is
  `~public`, the name the public prefix without its delimiter
  (`Public`).

Both are `isPersonal: false`, and `isReadOnly: true` when the user holds
none of the rights `i`, `w`, `s`, `t`, `e`, `x` on any of their
mailboxes. `k` does not count: no mailbox is created in a shared account
(see below), so `k` alone lets the user do nothing there. Their `accountCapabilities` are core and mail, with
`mayCreateTopLevelMailbox: false`, mail:share and, for an owner's
account, principals:owner (see [Principals](#principals)): submission, vacation, Sieve, quota
and the blob methods belong to the owner's own account, and a call of
one of them with the id of a shared account is `accountNotFound`, like a
call with any id the user cannot reach. `primaryAccounts` stay the
personal account.

**Discovery** works as IMAP `LIST` does: a grant writes a pointer into
the grantee's registry, and the user's pointers name the owners. Each
pointer is checked against the owner's ACL; a pointer whose mailbox is
gone is pruned, and one into the public namespace is dropped (public
folders are found through their namespace). A mailbox shared with
`anyone` alone has no pointer and is therefore not found — IMAP `LIST`
does not show it either, and where IMAP can still open it by its name,
JMAP has no name to address it by. The public namespace is read
directly and filtered by `l`. `server.shared_prefix` and
`server.public_prefix` switch the two kinds of account like the IMAP
namespaces: an empty prefix, no accounts.

**Cost.** The state of the session resource goes into every API
response, and building the shared accounts means opening each owner's
namespace and reading one registry entry per pointer. So the state does
not hash the shared accounts themselves but a fingerprint of what they
are made of, read from the user's own registry and the public one: the
pointers with their ACL generations — a grant or a revocation of the
user's rights rewrites the pointer with the mailbox's new generation, a
deletion or a rename removes or moves it — and the public folders the
user sees, with its rights. An API response reads those two — the
pointers only as far as `jmap.max_session_pointers` of them in key
order, one bounded read however many pointers other accounts have
written for the user, with a marker when there are more — and opens no
owner's namespace. Revoked pointers do not count against that bound:
the store keeps the tombstone of a revocation for an hour, against a
grant from an older ACL generation that lands late, and after that the
listings that meet it remove it, at most 256 per listing, so an account
that grants and revokes many mailboxes cannot fill another user's
window for good; the owners are opened when the session resource is
fetched, and when a method call names one of their accounts. A change
of the `anyone` entry of a shared mailbox alone does not touch the
pointers, so it reaches a client when it next fetches the session
resource, not through the state of an API response; so does a grant
whose pointer lies beyond the first `jmap.max_session_pointers`.

Three bounds keep the rest cheap:

- `jmap.max_shared_mailboxes` (default 1000) bounds the mailboxes one
  shared account examines: the user's pointers to that owner, the first
  ones by name, or the first visible public folders by name. What lies
  beyond is left out of the account — the same mailboxes every time —
  and logged once per owner.
- `jmap.max_session_pointers` (default eight times
  `max_shared_mailboxes`) bounds the pointers the session resource and
  the session state examine in all. The session resource finds the
  owners one step each, however many pointers one of them has, and an
  owner costs only its own pointers, at most `max_shared_mailboxes` of
  them. An owner that no longer fits into what is left is skipped (the
  look at it costs one), not the end of the list: an account that shares
  a thousand mailboxes with the user cannot push the others out of its
  session. A skipped owner is logged once and stays reachable by its id.
- `jmap.max_shared_accounts` (default 64) bounds how many owners the
  session resource lists; the public account is one more. An owner
  beyond it stays reachable by its id as well.

The owners are listed in the key order of the user's pointers, which is
the order of their names except where a name holds a character that
sorts before `/`.

**Existence is not revealed.** An id that names no account, an owner who
shares nothing with the user, a reserved namespace and an owner whose
mailboxes the user may not see are all `accountNotFound`. A mailbox
without `l` is not found, by id either, as IMAP answers `NONEXISTENT`;
a message that lies only in mailboxes the user may not read is not
found; a download of anything else is a 404. One exception is inherited
from IMAP: a rename onto the name of a mailbox the user cannot see is
refused because the name is taken, as IMAP `RENAME` answers
`ALREADYEXISTS`. The emptiness of a mailbox without `r` is not revealed:
see the deletion below.

**Rights.** `myRights` of a shared mailbox:

| Property | Needs |
|---|---|
| `mayReadItems` | `l` and `r` |
| `mayAddItems` | `i` |
| `mayRemoveItems` | `t` and `e` |
| `maySetSeen` | `s` |
| `maySetKeywords` | `w` |
| `mayCreateChild` | never (see below) |
| `mayRename`, `mayDelete` | `x`, and never for INBOX |
| `maySubmit` | never |

**Mailboxes.** The tree is built from the visible mailboxes. A visible
mailbox below a level the user cannot see keeps that level in its name
and has no parent, as a mailbox below a level without a mailbox does in
the personal account; IMAP `LIST` shows the same level as a `\Noselect`
parent, so its name is no secret either. The counts need `r` and are 0
without it; in a public folder `unreadEmails` and `unreadThreads` follow
the reader's own `\Seen`. `isSubscribed` is the user's subscription —
its pointer, or its list of public subscriptions — never the owner's
flag, and `Mailbox/set` writes it there; it needs nothing beyond seeing
the mailbox. `sortOrder` is the owner's, and changing it needs `x`,
since every other user sees it too. A rename or a move needs `x` on the
mailbox and `k` on the nearest existing mailbox above its new place, as
IMAP `RENAME` does; the top level of another namespace has no mailbox to
hold `k` on, so nothing moves there, and a move to another account is
not possible at all. A deletion needs `x` and removes the messages as
IMAP `DELETE` does; a mailbox with visible children is
`mailboxHasChild`, while children the user cannot see stay where they
are, as after an IMAP `DELETE`. Whether a mailbox still holds messages
(`mailboxHasEmail`) is only checked where the user may read it (`r`);
without `r` the deletion is `forbidden` unless the client passes
`onDestroyRemoveEmails` — which is what IMAP `DELETE` does anyway — so
the answer never tells whether the mailbox was empty.

A mailbox is not created in another namespace: IMAP `CREATE` allows it
with `k` on the parent, but the new mailbox carries no ACL — the store
does not inherit one — so its creator could not see it afterwards, and a
JMAP client would hold a mailbox that answers `notFound` from then on. A
creation is therefore `forbidden`, and `mayCreateChild` is false; the
owner creates the mailbox and shares it.

**Messages.** An Email of a shared account is a message in a mailbox the
user may read (`l` and `r`), and its `mailboxIds` are those mailboxes;
a copy elsewhere is not part of it. `Email/get`, `/query`, `/changes`,
`/queryChanges` and `Thread/*` see only those mailboxes. The full-text
index is the namespace's, so it finds messages the user may not read,
but every hit is resolved through the locator, which finds a message
only where the user may read it, so such a hit never becomes a result.

Writes check every right they need before the first write, as IMAP
does:

| Change | Needs |
|---|---|
| adding a message to a mailbox (creation, import, copy, move) | `i` there |
| removing it from a mailbox (move, `mailboxIds`, destruction) | `t` and `e` there |
| `$seen` changing on a copy, or carried by a new one | `s` there |
| any other keyword changing on a copy, or carried by a new one | `w` there |

A missing right refuses the whole creation or update with `forbidden`
and writes nothing; a keyword is never dropped silently. A new copy
takes only the flags the mailbox allows, as IMAP `COPY` does (a
`\Deleted` without `t`, for instance). A destruction removes the message
from every mailbox the user can read, and needs `t` and `e` in each; a
copy in a mailbox the user cannot read stays.

**Public folders** keep `\Seen` per reader (see
[Public folders](development/public-folders.md)). `$seen` in the public
account is the reader's own mark where it has one, the shared flag
otherwise; setting it writes the mark the way an IMAP `STORE` does —
the folder's next MODSEQ is allocated, the mark written under it, and
the folder's change log and the public account log name the message as
changed. The shared `\Seen` is left alone, and every other keyword is
shared. A message created, imported or copied into a public folder
carries `$seen` as the shared default, as an IMAP `APPEND` or `COPY`
does, which needs `s`. The marks of messages that are gone are cleaned
up by the next IMAP `SELECT` of the folder, not by JMAP.

**State and changes.** A shared account's state is a position in the
owner's account log (or the public namespace's), followed by a
fingerprint of what the user sees: for `Mailbox` the visible mailboxes
with the user's rights and subscriptions, for `Email` and `Thread` the
readable mailboxes. The position is the *visible watermark*: the newest
log entry the user can see — about a visible mailbox, or about the
messages of a readable one — not the head of the log. An entry that
names no mailbox (the RADOS store writes one for a change it could not
record in full) counts as invisible: it may lie in a mailbox the user
cannot see. What the owner does in mailboxes the user cannot see
therefore leaves the state where it is.

The position is the owner's counter, and two of them would tell how
many changes lie between two visible ones, so the token does not carry
it in the clear: it is enciphered with a key of the namespace (32
random bytes, created on first use and kept as a server METADATA entry
of the namespace under the accessor `~jmap`, which no account can be,
so neither the owner nor a reader can fetch it) into 16 hex digits after an `x`, a permutation of
the counter the user can neither read nor order. Tokens written before
this was introduced (a plain number before the dash) no longer parse,
and a client holding one resynchronises that account once.

A `/changes` reads from the watermark and leaves out the invisible
entries, however many there are: it reads on past them — at most 16
pages of 1000 entries — until it has `maxChanges` visible entries or
reaches the watermark, so neither the page nor the new state shows how
many were skipped. `hasMoreChanges` is true only when visible changes
were left for the next call; `newState` is the last visible entry it
reports, and the watermark once there are no more — the token a `/get`
answers. A run of more than 16000 invisible entries with no visible one
before it is `cannotCalculateChanges`, and the client resynchronises.
`Email/queryChanges` reads up to the watermark of its query state.

Finding the watermark means reading back to the last visible entry, so
a cursor records how far the log has been read: the watermark, the entry
read up to, and when it was written. There is one per visible set — the
visible mailboxes, and which of them may be read — not per user: users
who see the same share it, and so the same states. It is a METADATA
entry (`/private/vendor/shardpost/jmap/sharedcursor/<set>`) in the
*owner's* namespace under the server's own accessor `~jmap`, which no
account can be, so neither the user nor the owner reads it over IMAP.
Every request and every push follower advances it from where it stands
and writes it back when the watermark moved, when it read 256 entries
or more, or when it started again, so the log is read once rather than
once per request. A namespace keeps at most 256 cursors; the one
written longest ago goes, and its users start again.

A cursor starts again when there is none for the set yet (the first
request, a grant or a revocation — the state changes then anyway),
when the log no longer reaches back to it
(`server.account_log_retention`, 10000 entries), and when more than
16000 entries lie between it and the head. It then reads the last 1000
entries of the log and takes the newest visible one as the watermark,
so the state stays where it was when that is where it stood. Only when
those 1000 entries are all invisible does the watermark become the
head, and the state moves without a visible change — the one case in
which a client learns that the owner did something, not what nor how
much. Two requests that start it again at once agree on the one that
was stored first.

The log records what happens to the owner's mailboxes, not what the user may
see of them — a grant, a revocation, a pointer that appears or goes and
the user's own subscriptions change the answer without a log entry the
user's view could be read from. So a `/changes` against a token whose
fingerprint is not the current one answers `cannotCalculateChanges`, and
the client resynchronises that account rather than being handed a wrong
delta. Otherwise the delta is the owner's log with the entries about
invisible mailboxes, and about the messages of unreadable ones, left
out. A `/set` returns a token with the fingerprint of afterwards, so a
client's own subscription change or deletion does not cost it a
resynchronisation.

**Blobs.** The blob store of a shared account is the owner's, and it
holds more than the user may read. A blob of a shared account is a
message the user can read there, or a part of one: the download
endpoint, `Email/import`, `Email/parse` and the `blobId` of a body part
in a creation accept nothing else and answer as for a blob that does
not exist. An upload into a shared account is refused (404), and
`Blob/*` is not offered there. A message with an attachment from the
user's own upload is therefore written in the personal account and
copied over with `Email/copy`.

**`Email/copy`** works between any two accounts the user can reach:
personal to shared, shared to personal, shared to shared. The source
message has to be readable where it is, and is read through the
source's view; the destination checks what any new message there needs.
A namespace keeps its own blobs, so the copy is stored again and gets a
new id, as an IMAP `COPY` into another namespace does. Its `keywords`
and `receivedAt` default to the original's as the user sees them — its
own `$seen` in a public folder. The destination needs the rights for
those keywords like for any others: a read message copied into a
mailbox without `s` is `forbidden` unless the creation passes `keywords`
without `$seen`. IMAP `COPY` drops the `\Seen` silently instead; JMAP
does not drop what a client did not ask to drop. `onSuccessDestroyOriginal` runs the
implicit `Email/set` on the source, which needs `t` and `e` there; its
response follows the copy's under the same call id.

**Push** reports the shared accounts as well (stage 6c-3, see
[Push](#push)). `Quota/*` is not offered for the public account.

**Deviation from RFC 9670 1.4.** The RFC lists a shared account in the
session only when the user subscribes to at least one of its mailboxes.
This server lists every account that shares a mailbox the user can see,
as stage 6c-1 did and as IMAP `LIST` shows them; filtering by
subscription would take shared accounts away from clients that already
use them. `isSubscribed` of each mailbox says what the user subscribed
to.

### Principals

Stage 6c-2 (RFC 9670 2). The session offers
`urn:ietf:params:jmap:principals` (an empty object), and the personal
account is the account of the principals — `primaryAccounts` names it —
with `currentUserPrincipalId` in its capability. Every account that
belongs to somebody carries `urn:ietf:params:jmap:principals:owner`
with `accountIdForPrincipal` (the personal account) and `principalId`
(its owner): the personal account names the user, a shared account its
owner. The public account belongs to nobody and has none.

A principal is an account of this server, of type `individual`. Its id
is the canonical account name — the same string that is its account id,
its ACL identifier and, where it is an address, its `email` — so a
principal id tells a caller nothing it does not already know. `name` is
the account name as well: display names are the directory's, and a
principal looks the same however it was found. `description` and
`timeZone` are null, `capabilities` is empty, and `accounts` holds the
account the user can reach for it: its own personal account, or the
shared account of an owner who shares with it; null for anybody else.

**No directory listing.** The authentication backend resolves one
address at a time and lists nobody, and Principal/* does not work
around that. The principals a user sees are:

- itself;
- the owners who share mailboxes with it — its pointers, as far as the
  session looks for shared accounts (`jmap.max_shared_accounts`);
- the identifiers in the ACLs of its own mailboxes (not `anyone`, not a
  negative or reserved identifier);
- an account named exactly: an `email` condition of `Principal/query`,
  an id of `Principal/get`, or a key of `shareWith` that is none of the
  above is looked up in the backend (Nauthilus identity lookup, as for
  `Identity`). It is a principal when the backend resolves the address
  to that very account and the account lives on this shard (the JMAP
  shard gate, `jmap.shard_attribute` and `jmap.shard`). An unknown
  address, an alias and an account of another shard all give no result,
  after the same lookup.

The other conditions of `Principal/query` — `name`, `text`, a part of
an address in `email`, `accountIds`, `type`, `timeZone` — filter this
set and look nothing up; `email` and `name` are substring matches
without regard to ASCII case. A query sorts by `name` or `email`
(both the account name); without a sort the user comes first, then the
others by id.

Bounds: `jmap.max_principal_lookups` (default 5) lookups per API
request, whatever calls make them — the same address twice counts once;
a query or a get beyond it is `requestTooLarge`, a `shareWith` beyond it
`rateLimit`. A backend that cannot answer is `serverUnavailable` (for a
`shareWith`, `serverFail`).
`jmap.principal_miss_per_minute` (default 30) bounds, per account and
across requests, the lookups that find nothing — an unknown address, an
alias, an account of another shard — so that a user cannot use them to
test which addresses exist: every account has a bucket of that many
misses that refills at that rate per minute, and a lookup is made only
while the bucket holds one. A lookup that finds a principal takes
nothing out of it, but an empty bucket holds back every lookup, since
the answer is not known before it is made. Beyond it no lookup is made;
`Principal/query` and `Principal/get` answer `serverUnavailable` — RFC
8620 has no method-level `rateLimit`, and this is the error a client
retries later — and `shareWith` the set error `rateLimit`. The buckets
are kept in the memory of each front-end, at most 10000 accounts (full
buckets are dropped first, then the least recently used): a deployment
with several front-ends grants the rate per front-end, and a restart
refills them. Every lookup goes to Nauthilus with the client's address,
so that its own rate limits and brute-force detection apply, and is
logged at Info (`jmap: principal lookup`, with the account, the first
16 hex digits of the SHA-256 of the address and the result: `found`,
`unknown`, `alias`, `other shard`, `failed` or `refused: miss budget`);
the address itself is logged only at Debug. The hash is unsalted: it
tells repeated lookups of one address apart from many and can be
compared with an address one suspects, but it is not anonymous.
`jmap.max_principals` (default 1000) bounds the principals seen without
a lookup; the rest, in id order, is left out and logged once, and stays
reachable by an exact lookup. `Principal/get` without ids returns that
set, and `requestTooLarge` when it is larger than `maxObjectsInGet`.

**State and changes.** The principals are computed, not logged. Their
state is a hash of the set a user sees and of the sharing fingerprint of
the session (the `accounts` follow the shared accounts).
`Principal/changes` from the current state reports nothing, from any
other `cannotCalculateChanges`, which RFC 9670 2.2 foresees for a server
backed by a directory; `Principal/queryChanges` is always
`cannotCalculateChanges`. `Principal/set` refuses every change with
`forbidden`: principals are the directory's.

### Sharing a mailbox (shareWith)

With `urn:ietf:params:jmap:principals` or
`urn:ietf:params:jmap:mail:share` (JMAP Mail Sharing,
draft-ietf-jmap-mail-sharing) in `using`, a Mailbox has `shareWith` and
its `myRights` has `mayShare`; without them it looks as before. Every
account offers `urn:ietf:params:jmap:mail:share` (an empty object).

`mayShare` is the IMAP right `a`: always true in the personal account,
the ACL's in a shared one. `shareWith` is the mailbox's ACL seen as JMAP
rights: one entry per principal in it, never the owner (whose rights are
implicit), `anyone` or a negative identifier. It is shown only to a user
who may change it, as IMAP `GETACL` needs `a`; anybody else reads null.

| MailboxRights property | IMAP rights |
|---|---|
| `mayReadItems` | `l` and `r` |
| `mayAddItems` | `i` |
| `mayRemoveItems` | `t` and `e` |
| `maySetSeen` | `s` |
| `maySetKeywords` | `w` |
| `mayCreateChild` | `k` |
| `mayRename`, `mayDelete` | `x` (one right: they are always equal) |
| `mayShare` | `a` |
| `maySubmit` | none: always false, cannot be granted |

A property is true when the entry holds all of its IMAP rights. The two
models do not map one to one — IMAP has `p`, and `l` or `t` without its
partner, which JMAP cannot show — so a write changes only the IMAP
rights of the properties whose value it changes: a property that
arrives as the entry shows it leaves its rights alone, and a right JMAP
cannot express is never dropped by an update. Only removing the
principal (null, or leaving it out of a whole new `shareWith`) removes
its entry, `p` included. `shareWith` here is the raw mapping: on INBOX
the grantee's own `myRights` still say it can neither rename nor delete
it, and in a shared account `mayCreateChild` is false whatever `k`
says (a mailbox is not created in another namespace).

`Mailbox/set` takes the patch forms of RFC 8620 5.3:

- `shareWith`: a whole new map, or null. Every principal not in it
  loses its entry; `anyone` and negative entries stay.
- `shareWith/<principal>`: that principal's rights, a MailboxRights
  object whose missing properties are false, or null to remove it.
- `shareWith/<principal>/<property>`: one right, true or false.
  `mayRename` and `mayDelete` patch each other.

A whole map with a patch of it, or a principal with a patch of one of
its rights, is `invalidPatch`. A key that is the owner, not a principal
(see [Principals](#principals)), or an unknown or foreign account is
`invalidProperties`; so are `maySubmit: true`, `mayRename` and
`mayDelete` apart, an unknown property and a value that is not a
boolean. A change of `shareWith` needs `mayShare` (`forbidden`
otherwise); in a shared account a user with `a` may share further,
change its own entry, and never name the owner. A creation may carry
`shareWith` as well. Everything is checked before anything is written.
When the ACL of a new mailbox cannot be written after all, the mailbox
is deleted again and the creation reported as `serverFail`, so that a
creation is all or nothing; only if that deletion fails too is it
reported created, with the `shareWith` actually in effect (both are
logged). Notifications already written for a grant that was rolled back
stay.

The write is the one IMAP `SETACL` and the admin API make: one
versioned change per identifier, computed from the entry as it is at
that moment, with the grantee's pointer and the ACL generation
following — a grant shows the owner's account in the grantee's session,
a revocation removes it. A pointer that cannot be written is logged and
the change stands, as for IMAP. Several principals of one update are
several such changes, not one atomic step. The update answers with
`shareWith` as it is afterwards, since one right can move another.

### Share notifications

RFC 9670 3. When the rights an account holds on a mailbox change — a
grant, a change, a revocation, through JMAP `shareWith`, IMAP `SETACL`
or `DELETEACL`, or the admin API — the account gets a notification.
The rights are the ones that count for it, its own entry with
`anyone`'s; a change that leaves them as they were, a change of
`anyone` itself (RFC 9670 3 lets a server leave out groups), and a
change an account makes to its own entry leave none. A second change
of the same mailbox replaces the first, keeping the rights from before
the first; a change back to where it started leaves nothing (RFC 9670
6.3). Deleting or renaming a mailbox, and the maintenance jobs that
remove the grants of a deleted account, write none.

`ShareNotification` objects are in the personal account:
`objectType` is `Mailbox`, `objectAccountId` the owner's account (or
`~public`), `objectId` the mailbox id, `name` the mailbox's full name
in the owner's namespace at the time — after a revocation it is all the
user has to recognise it by — and `oldRights` and `newRights` are
`myRights` as a shared mailbox shows them, with `mayShare`, or null for
no rights. `changedBy` is the account that made the change, with its
principal id; a change through the admin API is `Administrator`, with no
address and no principal.

They are kept in a server-managed METADATA entry of the account's own
namespace, written by whoever makes the change, bounded by
`server.share_notifications_max` (50, at most 200: the oldest goes) and
`server.share_notifications_retention_seconds` (30 days), and by the
64 KiB of one entry. Writing one is best effort and never fails the ACL
change. It is written into the grantee's namespace in the store of the
server that made the change: an account of another shard, which the
change cannot reach there, gets no notification it can read.

`ShareNotification/get`, `/changes`, `/query` (filters `after`,
`before`, `objectType`, `objectAccountId`; sort by `created`, oldest
first by default) and `/set`, which destroys and refuses everything
else with `forbidden`. The state counts every creation and destruction;
`/changes` reports both and remembers destructions four times
`share_notifications_max` far back — an older state is
`cannotCalculateChanges`. `/queryChanges` is `cannotCalculateChanges`.

Notifications are pushed as the type `ShareNotification` of the
personal account (see [Push](#push)); principals are not: their state is
a hash of every principal the user can see, which costs as much to
compute as `Principal/get`. A client that shows principals fetches them
with the session, or asks `Principal/get` again.

## Push

Push (RFC 8620 7) tells a client that something changed, never what:
a `StateChange` naming, per account, the data types whose state moved.
Both ways of telling it compute that from the account change logs: the
personal account's, and those of the shared accounts (see
[Shared accounts](#shared-accounts-in-push) below).

**Which types.** Every type's state is the account counter, so the
value is the same for all of them; the types that appear say what kind
of change it was. A mailbox change is `Mailbox`; a change to a message is
`Email`, `Thread` and `Mailbox` (the counts of its mailbox moved); a new
message is also `EmailDelivery`. The log does not know how a message
arrived, so an IMAP `APPEND` — a sent copy, a draft — is a delivery as
well. A Partial entry, or a log that no longer reaches back, names every
type.

**The event source** (`/jmap/eventsource/`, RFC 8620 7.3) is a
`text/event-stream` held open by the front-end. It learns of a change
from a watch on the account object — the store notifies it after every
record of the account change log, cluster-wide, the way IMAP IDLE learns
of a delivery on another pod — and checks the state on its own every
`jmap.push.event_poll_seconds` (30) in case a notification was lost.
Each `state` event carries, as its event id, the state of everything
the user sees (RFC 8620 7.3): the personal account's state, and — when
the user has shared accounts or share notifications — a digest of their
states after a dot (`4711.3f9a0c21d4e8`). A client that reconnects with
`Last-Event-ID` is sent what it missed of the personal account at once,
and the current states of all shared accounts and of the notifications
when the digest is no longer the current one; it compares them with its
own and asks for the changes where they differ.
`closeafter=state` ends the stream after one event; `ping` is clamped to
15–900 seconds. An account may hold `maxConcurrentRequests` event
sources open, counted apart from API requests, and shutting the server
down ends them.

**Push subscriptions** (RFC 8620 7.2) are for clients that cannot keep a
connection open. A front-end takes them and keeps them in the shared
admin store — all subscriptions of an account in one record, and new
ones in a queue of verifications to send; a separate process, the
**push worker** (`shardpost push`, one replica, component
`deploy/components/jmap-push`), serves them. It sends the
`PushVerification` of a new subscription, and once the client has sent
the code back it watches that account and posts a `StateChange` when it
changes, gathering changes for a second so a burst becomes one push.
The worker that works holds a lease in the admin store, renewed on a
clock of its own, and another waits for it to lapse; a store it cannot
reach for a moment does not end its work while its lease is valid. The
state last pushed is kept per account, so a worker that takes over, or
restarts, pushes what happened in between rather than starting from
now. The account watches are kept up: a watch the store reports broken
(an OSD restart drops RADOS watches) is set up again, and the poll
covers the gap.

Every request to a push service is encrypted for the device when the
subscription has keys (RFC 8291, aes128gcm) and signed with the
server's VAPID key (RFC 8292); the front-ends advertise its public half
as `urn:ietf:params:jmap:webpush-vapid` (RFC 9749), which is what a
browser client hands its push service. The key is
`jmap.push.vapid_key_file`, and it is also the switch: without it,
`PushSubscription/*` is not offered. A subscription made under another
key is destroyed when the key changes.

What the worker does with an answer: 404 and 410 destroy the
subscription; 429, 5xx and an unreachable push service hold its pushes
back — at least one scan, or as long as `Retry-After` says — and the
types it missed are kept and sent in one push afterwards; any other
refusal is logged and not repeated. A verification that is refused is
not sent again (RFC 9749 6); one that did not get through is tried up
to three times. Push URLs are capabilities and do not appear in the
logs.

A push URL is a client's to choose, and the worker posts to it, so it is
treated as an attack vector: it has to be `https` on port 443, a
front-end refuses one that names an internal address or another port,
and the worker refuses to connect to any loopback, private, link-local
or shared address after name resolution, or to any port but 443, and
follows no redirects; such a push is not retried. An in-house push
service needs `jmap.push.allow_internal_endpoints` on both sides, which
lifts the port rule as well.

The verification queue is emptied only of what the worker has sent,
after it sent it, in one update that keeps what the front-ends queued
in between: a worker that stops on the way leaves the queue whole, and
its successor sends those verifications — some a second time, with the
same code, which does no harm.

A subscription belongs to the account, not to the credentials that made
it (RFC 8620 7.2 ties it to them): changing the password does not end
it. It expires after at most `jmap.push.max_expiry_seconds` (7 days;
never less than the 48 hours the RFC asks for), and a client extends it
by updating `expires`. With `jmap.push.check_accounts` the worker looks
the accounts with subscriptions up in the authentication backend (each
at most hourly, twenty per scan) and destroys the subscriptions of an
account the backend has denied three times in a row. It refuses to
start with it unless `server.auth_backend` is `nauthilus` — the worker
needs the same backend settings as the server — and when most of the
accounts asked in one scan are denied it suspects the backend, logs an
error and drops nothing. An account may hold
`jmap.push.max_subscriptions` (16) subscriptions of at most 32 types
each.

The worker lists the account records once per scan and reads the
verification queue every two seconds; each watched account costs it an
open store view, a RADOS watch and a poll. That is sized for thousands
of accounts with push subscriptions, not for millions.

### Shared accounts in push

Stage 6c-3. A `StateChange` names the shared accounts of the user (see
[Sharing](#sharing)) as well, keyed by their account ids, with the same
types as the personal account (`Mailbox`, `Email`, `Thread`,
`EmailDelivery`) and the states a `/get` in that account answers — the
visible watermark and the fingerprint, from the same cursor. A change in
a mailbox the user cannot see (no `l`, or for its messages no `r`)
wakes the follower, which reads the log, finds nothing it may report,
and sends nothing: the state has not moved. The types are those the
visible entries changed, as for the personal account, and those whose
fingerprint changed: a grant or a revocation that changes the visible
set, or a subscription the user changed from another client, is a
`Mailbox` change (and `Email` and `Thread` when the readable set
changed).

**Which accounts.** A follower — an event source, or the push worker
for one account with subscriptions — reads the shared accounts as the
session resource lists them, with the same bounds
(`jmap.max_shared_accounts`, `jmap.max_session_pointers`,
`jmap.max_shared_mailboxes`), and reads them again when what they are
made of changes: the fingerprint of the session state (the user's
pointers and the public folders it sees) and the user's subscriptions,
checked on every poll. An account that appears is named with all its
types; the client does not know its id, and fetches the session
resource (RFC 8620 2). An account the user lost — its last visible
mailbox revoked, deleted, or hidden by a change of `anyone` — is named
once with the state `revoked` for every type: no `/get` answers that
state, so a client asks for the changes, is answered `accountNotFound`,
and fetches the session resource. RFC 8620 has no push of the session
state itself (a `state` event carries a `StateChange` only), so this is
how a client that only listens learns that its accounts changed; the
next API response's `sessionState` says it as well. A grant takes up to
one poll to be seen: it rewrites the user's pointer, which no watch
reports.

**Share notifications** are the type `ShareNotification` of the
personal account; their state is read on every poll (the notices live
in METADATA, which no watch reports), so a grant's notification comes
with the same poll that finds the grant. The state is read as stored;
notices that expired are pruned by the next `ShareNotification/*` call,
whose state may then be newer than the pushed one.

**Watches.** A follower watches the account object of each shared
account's namespace, as the personal one, through a hub that holds one
watch per namespace for every follower of the process — a thousand
users following the public folders cost one watch. A follower holds at
most `jmap.push.max_shared_watches` (8) watches; the accounts beyond
them, and any whose watch cannot be set up, are read on its poll
(`jmap.push.event_poll_seconds`, 30, for an event source;
`jmap.push.poll_seconds`, 60, for the worker). The worker watches at
most `jmap.push.worker_max_watches` (1000) namespaces for shared
accounts in all; beyond that, accounts are polled. A watch the store
reports broken is set up again, and the poll covers the gap, as for the
personal account. The watches are given back when the event source
closes or the worker stops following the account.

**Cost.** Opening an event source, or starting to follow an account in
the worker, builds the shared accounts as fetching the session resource
does. Afterwards a follower reads, per poll, the user's pointers, its
public subscriptions and its notices, and per shared account due the
cursor and the log entries since; it loads an account's mailboxes again
only when the log shows a change of a mailbox itself. The worker keeps
the shared states it last pushed with the personal one
(`jmap/push/seen/<account>` in the admin store), so a worker that takes
over pushes what changed in between; a record written before this stage
holds the personal state alone, and the shared accounts start from
"now".

## Identities

`Identity/get`, `Identity/changes` and `Identity/set` (RFC 8621 6)
belong to the submission capability and are offered together with it,
when `jmap.submission.relay_addr` is set.

The addresses an account may send from come from the authentication
backend's identity lookup — Nauthilus `LookupIdentity`, without a
password — so it works whatever the backend behind Nauthilus is. The
configured attributes of the answer carry the addresses (values without
a local part, such as a catch-all `@example.org`, are not addresses and
are skipped); each address is one identity that cannot be deleted, named after the backend's
display name (`display_name_field`). A client may change the name,
`replyTo`, `bcc` and the signatures, and may add further identities for
the same addresses (`forbiddenFrom` for any other); what it changed is
kept in the account's own metadata (the server entry
`/private/vendor/shardpost/jmap/identities`). An identity whose address
disappears from the backend disappears with it. The state is a hash of
the list, because part of it lives in the user database;
`Identity/changes` from an older state is `cannotCalculateChanges`, and
the client fetches the list again.

## Sending

`EmailSubmission/get`, `/changes`, `/query`, `/queryChanges` and `/set`
(RFC 8621 7). JMAP has no other way for a client to send mail, which is
why it is here at all.

**ShardPost sends nothing itself; it is a proxy to the relay.** Every
request is authenticated first, as every JMAP request is; only a
message of the authenticated account can be sent, only with one of its
identities. The message is then handed to the relay configured in
`jmap.submission` — a Postfix submission service — and the user is named
to it with Postfix's `XCLIENT LOGIN=<account> ADDR=<client address>`:
the relay treats the session as if the user had logged in with SASL, so
its own rules decide what the user may send — `smtpd_sender_login_maps`
with `reject_sender_login_mismatch`, rate limits, policy services. The
relay has to allow XCLIENT from the ShardPost hosts
(`smtpd_authorized_xclient_hosts`); a relay that does not offer it to
them is not used, and a submission fails rather than going out
unattributed. With `jmap.submission.xclient: false` the relay is logged
into with the configured credentials instead, and then ShardPost's own
check below is the only one — the configuration refuses that
combination without it.

**What ShardPost checks before the relay is asked:**

- The identity and the email exist in the account (`invalidProperties`
  otherwise).
- The message has at most one From and one Sender field, one address
  in Sender, and a Sender when From has several (`invalidEmail`): a
  second From is how a sender nobody checked would reach the reader.
- With `jmap.submission.check_sender` (on by default) the envelope
  sender and every From and Sender address are among the account's
  addresses from the identity lookup (`forbiddenMailFrom`,
  `forbiddenFrom`). The relay checks the envelope only; the header is
  ShardPost's to check. Switch it off only where the relay allows more
  — delegations — and checks the header itself.
- The recipients: at least one (`noRecipients`), at most
  `max_recipients` (100, `tooManyRecipients`), each an address
  (`invalidRecipients`). The message is at most `max_size`
  (`max_size_upload`, `tooLarge`).
- No SMTP parameters: `submissionExtensions` is empty, so an envelope
  that carries any is `invalidProperties`.

Without an envelope the server derives one as RFC 8621 7 says: the
Sender, else the From, else the identity's address, and the recipients
of To, Cc and Bcc, each once; a recipient field that does not parse is
`invalidEmail` rather than silently dropped. The Bcc field is removed from the message
before it goes out. A refusal of the relay comes back as the matching
error: a refused sender (`Sender address rejected`) as
`forbiddenMailFrom`, refused recipients as `invalidRecipients` with the
list, a message too large as `tooLarge`, anything else as
`forbiddenToSend` with the relay's reply; a temporary refusal or an
unreachable relay is `serverFail`, and the client may try again. A
relay that does not offer XCLIENT with LOGIN to this host, or refuses
it, is a configuration matter: `forbiddenToSend` for the client and an
error in the log. Each step of the conversation has its own deadline
(`timeout_seconds`), the upload time for the size of the message, and
the relay's answer to the message up to ten minutes — a relay runs its
filters before it answers, and a message it took must not be reported
as failed, or the client sends it again.

`onSuccessUpdateEmail` and `onSuccessDestroyEmail` are carried out in an
implicit `Email/set` whose response follows the `EmailSubmission/set`
response, under the same call id — the usual way a client moves the
sent draft to *Sent*. A client can create the draft and send it in one
request, naming it by its creation id.

**Delayed sending is prepared, not offered.** `maxDelayedSend` is 0, a
`HOLDFOR`/`HOLDUNTIL` parameter is refused, and every submission is
`final` once the relay has it, so a cancellation is `cannotUnsend`. The
record already carries `sendAt` and `undoStatus`, so a queue can be
added without changing what is stored.

**Records.** The submissions of an account are one record in the admin
store, with a state and a change log of the last 256 changes behind
`/changes`; `/queryChanges` is `cannotCalculateChanges`. An account keeps
them for `retention_seconds` (7 days), at most `max_kept` (500) and at
most a megabyte of records, oldest dropped first; RFC 8621 7 lets a
server destroy a submission at any time after it was sent.
`deliveryStatus` says `queued` for each recipient, with the relay's
answer to the message — its queue id included — as `smtpReply`; DSNs
are not evaluated, so it never becomes `yes` or `no`, and `dsnBlobIds`
and `mdnBlobIds` stay empty.

`EmailSubmission/query` filters by `identityIds`, `emailIds`,
`threadIds`, `undoStatus`, `before` and `after` (on `sendAt`), with the
operators `AND`, `OR` and `NOT`, and sorts by `emailId`, `threadId` and
`sentAt`. Any other property, an unknown operator or a date that is not
a `UTCDate` is `unsupportedFilter`, a property of the wrong type
`invalidArguments`, and another sort property or collation
`unsupportedSort`: a condition left out would widen the result.

## Quotas

`Quota/get`, `/changes`, `/query` and `/queryChanges` (RFC 9425) belong
to `urn:ietf:params:jmap:quota`, which is always offered: every account
has a quota, even if it is unlimited. The capability carries no
properties, in the session or in the account.

The objects are what IMAP `GETQUOTA` reports, from the same counters:
one per limited resource of the account. `octets` is the storage (usage
and limit in bytes), `count` the number of messages; both have the scope
`account`, the names `Mail storage` and `Mail messages`, and the types
`["Email"]` — RFC 9425 asks for the names of the JMAP data types
registry, whose examples say `Mail`; a query for the type `Mail` finds
them as well. A resource whose limit is zero is unlimited and has no
object, as it has no resource in the IMAP answer. `warnLimit` is the
lowest `quota.warnings` threshold with the direction `over` for that
resource (see [Quota](quota.md)), as long as it lies below the limit;
there is no `softLimit` and no `description`.

**State.** The account change log does not record quota changes — the
usage moves with every delivery and the limit comes from the user
database — so the state is not the account counter. It is the values
themselves, written into the token: a version prefix and, per object,
`used`, `hardLimit` and `warnLimit`. `Quota/changes` compares them with
the values of now: an object that appeared is created, one whose limit
was removed is destroyed, one with other values is updated, and
`updatedProperties` is `["used"]` when nothing else of the updated
objects changed. More changes than `maxChanges` are handed out in steps,
each with a state of its own. A token of another shape is
`cannotCalculateChanges`.

`Quota/query` filters by `name` (a substring, without regard to case),
`scope`, `resourceType` and `type`, with the operators `AND`, `OR` and
`NOT`, and sorts by `name` and `used`; anything else is
`unsupportedFilter` or `unsupportedSort`. `Quota/queryChanges` is
`cannotCalculateChanges`, as for mailboxes.

## Vacation

`VacationResponse/get` and `/set` (RFC 8621 8) belong to
`urn:ietf:params:jmap:vacationresponse`, which is offered when delivery
can send the reply: when `smtp.addr` names a relay. Without one the
capability is not in the session and the methods answer `unknownMethod`.

An account has one VacationResponse, `singleton`; it can be read and
updated, not created or destroyed (`singleton`). Its default is off,
with no dates and no texts. A change is checked first: `isEnabled` is
`true` or `false` (null is `invalidProperties`), the dates are UTCDates
(written back in UTC) and `toDate` does not lie before `fromDate`, the
subject is one line of at most 998 bytes, each body at most 16 KiB
without control characters.

**The setting is the source.** It is kept as JSON in the account's
metadata, in the server entry `/private/vendor/shardpost/jmap/vacation`,
and that entry is what delivery answers from. Only the JMAP front-end
writes it (see [server-managed entries](#server-managed-metadata)). Its
state is a hash of it, and there is no `/changes` for this type. The
state check and the write are one step: the entry is replaced only while
it still holds what the state was computed from. Of two sets that name
the same `ifInState`, one wins and the other is `stateMismatch`; a set
without `ifInState` starts over on the new setting.

**The script.** Delivery turns the setting into a Sieve script and runs
it. The script sends the reply with `vacation`, once per sender in seven
days. `fromDate` and `toDate` become `currentdate` tests (RFC 5260) in
UTC: the reply goes to messages that arrive on or after `fromDate` and
before `toDate`. A text body alone is a plain reply; with an HTML body
the reply is a MIME entity, the text and the HTML as alternatives, or
the HTML alone. Without either body the reply carries a short default
text, as RFC 8621 asks; without a subject it takes the server's default
subject. A subject is cut to 256 characters in the reply, with an
ellipsis, as every Sieve vacation subject is. Every text a client sends
is written as a quoted Sieve string and cannot become part of the
script, and a set whose script would not compile is refused
(`serverFail`) before anything is stored.

The once-per-sender record is kept under a handle derived from the
setting — dates, subject and bodies. A changed text therefore reaches a
sender that was answered before; turning the vacation off and on again
with the same setting does not, within the seven days.

**A copy for display.** Every change also stores the script under the
name `jmap-vacation` in the account's script store while the vacation
is enabled, and removes it when it is not, so that a ManageSieve client
such as Roundcube can show the setting. Delivery never reads that copy, and a user script cannot `include` it (an include of the reserved name finds no script).
ManageSieve, JMAP `SieveScript/*` and the admin API show it but refuse
to store, rename, delete or activate it; an import of scripts that names
it is refused, and an export, a restore or a migration leaves it out.

**After the active script.** The JMAP vacation is never activated and
never replaces the user's active script. Delivery runs the active script
as always; when that produced no vacation reply — none in the script,
its conditions did not match, or the script failed — did not reject the
message and keeps it in a mailbox, delivery evaluates the JMAP vacation
and takes its vacation alone. A vacation the user keeps in the active
script, such as Roundcube's, therefore wins; the two never answer the
same message twice. An account without an active script still gets the
JMAP vacation. The reply goes out the way every Sieve vacation does:
through the relay, with the RFC 5230 checks (no reply to automatic mail,
lists or the null sender) and the per-sender record. A setting that
cannot be read or does not run is logged and leaves the delivery alone.

**Who is answered.** The RFC 5230 check answers a message only when one
of the account's addresses is among its recipients (`To`, `Cc` and the
like), and JMAP has no way to name further addresses (`:addresses`):
the one address counted is the one the message was delivered to. A
message to an alias the delivery rewrote, with that alias alone in the
header, is not answered. The Sieve setting
`sieve_vacation_check_recipient` (`sieve.settings`, `no` turns the check
off) is the knob for a deployment with such aliases.

## Blobs

A blob is a sequence of octets the account can read: a stored message
(its id is the GUID of the message data, the same identity IMAP reports
as `EMAILID`), a body part of one (`<guid>_2-1`, see Messages), an
upload, or the content of a Sieve script (`S` and 40 hex digits of its
SHA-256, see [Sieve scripts](#sieve-scripts)). The upload endpoint stores the request body; the download
endpoint serves any of them, with HTTP ranges. Blobs live in the
account's own namespace of the store, so a blob id of another account is
not found rather than refused, whoever knows it.

**Lifetime.** An uploaded blob that nothing references is debris: the
scrubber removes it once two consecutive runs have seen it
unreferenced, which is the expiry RFC 8620 6.1 allows. Two consequences
an operator should know: the expiry is the scrub cadence, not a time, so
a deployment that does not schedule `shardpost scrub` never collects
them; and an upload is only refused when the account is already over its
quota, because an unreferenced blob is counted nowhere until something
references it.

**The blob methods** (RFC 9404, `urn:ietf:params:jmap:blob`) are always
offered. The capability is empty in the session; the account's says
`maxSizeBlobSet`, `maxDataSources`, the types `Blob/lookup` answers for
(`Email`, `Mailbox`, `Thread`, and `SieveScript`) and the digests
`Blob/get` computes (`sha-256`, `sha`).

- **`Blob/upload`** builds a blob from pieces: `data:asText` (UTF-8),
  `data:asBase64` (with or without padding) and ranges of other blobs
  (`blobId` with `offset` and `length`), concatenated in order. The blob
  is stored the way an upload is — same store, same lifetime, the same
  quota check (`overQuota` for an account that is already full) — and its
  id goes into `createdIds`, so a later call, or a later creation of the
  same call, names it as `#creationId`; so do `Email/import`,
  `Email/parse` and the body parts of `Email/set`. A creation is refused
  as a whole: a source that is not exactly one of the three kinds, text
  that is not UTF-8 (the server checks what the client sent, not what a
  JSON decoder made of it), base64 with a foreign character, a range that
  does not lie within its blob, and more than `maxDataSources` sources are
  `invalidProperties` with the source's path (`data/2`); a blob that is
  not there is `blobNotFound` with every missing id; a blob beyond
  `maxSizeBlobSet` is `tooLarge`, found out before the source that would
  overflow it is read. The creations of one call build at most
  `maxSizeBlobSet` bytes together, since every source is copied into
  memory before the blob is stored: a creation beyond what the call has
  left is `tooLarge` as well, and the ones before it stand. A call takes
  a slot of the same per-account counter as the upload endpoint
  (`maxConcurrentUpload`); without a free one it is `serverUnavailable`,
  and the client tries again. The messages that body parts are taken
  from, the part contents and the script list are kept for the call so
  that each is read once, up to the larger of `maxSizeBlobSet` and
  `jmap.max_size_blob_get` in bytes; beyond that the oldest goes and is
  read again when it is named again, and something larger than the bound
  on its own is not kept at all. The
  media type is echoed as given, and null when none was (RFC 9404 4.1);
  the server keeps bytes, not types. Creations that name each other in a
  cycle find no blob.
- **`Blob/get`** returns `data`, `data:asText`, `data:asBase64`, `size`,
  `digest:sha` and `digest:sha-256` (default: `data` and `size`) of every
  kind of blob, optionally of a range (`offset`, `length`). `size` is the
  size of the whole blob. Octets that are not UTF-8 are `data:asBase64`
  for `data` and null for `data:asText`, with `isEncodingProblem`; a range
  that starts or runs past the end is cut and says `isTruncated`. `ids`
  are required, at most `maxObjectsInGet`. The data one call returns is at
  most `jmap.max_size_blob_get` (10 MiB) in total, counted before the
  encoding; a call that would return more is `requestTooLarge` as a whole,
  and the client asks for a range or downloads the blob. A digest reads
  the octets it covers and counts against the same bound, once per blob
  with its data; without the data it is computed a megabyte at a time.
  Messages, parts and the script list are kept for the call within the
  same bound as for `Blob/upload`; `size` alone costs no data budget, but
  it does not make the call hold more than that bound either.
- **`Blob/lookup`** finds the objects a blob belongs to. A message's blob
  and its body parts' blobs name the message, so this is a lookup in the
  locator, not a search: the result is the message, its thread and the
  mailboxes it is in. A body part is matched by its message without the
  message being parsed. A script's content names the scripts that have
  it. Any other blob — an upload nothing uses yet, a
  blob that does not exist — gets empty lists, as RFC 9404 asks, so that
  nothing is learnt about what is not the account's. A type other than
  these, or one whose capability (`urn:ietf:params:jmap:mail`,
  `urn:ietf:params:jmap:sieve`) the request does not use, is
  `unknownDataType`.

## Sieve scripts

`SieveScript/get`, `/set`, `/query`, `/queryChanges` and `/validate`
(RFC 9661) belong to `urn:ietf:params:jmap:sieve`, which is always
offered. The scripts are the ones ManageSieve manages, in the same store:
a script stored over one protocol is there over the other, and the active
script is the one delivery runs.

**The capability.** The session names the implementation (`ShardPost`
and the version). The account's lists what ManageSieve says: the
extensions of its `SIEVE` capability, including `imapsieve` and the
`vnd.dovecot.*` programs when they are switched on; `notificationMethods`
`["mailto"]` for `enotify`; `externalLists` null (no `extlists`);
`maxSizeScript` 256 KiB; `maxNumberScripts` null, since ManageSieve
counts no scripts either; `maxNumberRedirects` 4, which is what the
interpreter and delivery allow and what ManageSieve now advertises as
`MAXREDIRECTS` as well (it said 5 before); and `maxSizeScriptName` 512,
the octets RFC 9661 asks for — 128 characters of up to four bytes. The
same 512 bytes bound a name over ManageSieve, the admin API, in an
account archive and in the store.

**Ids.** ManageSieve knows a script by its name; JMAP wants an id that
stays when the name changes. A script's id is derived from its name
(`n` and 16 hex digits of its SHA-256) unless the account's metadata
says otherwise: the server entry
`/private/vendor/shardpost/jmap/sievescripts` (see [server-managed
entries](#server-managed-metadata)) keeps, by name, only the ids that
differ from the derived ones. A script stored over ManageSieve, the
admin API or an import therefore has its id at once, and listing the
scripts writes nothing. The entry is written with a compare-and-swap,
by `SieveScript/set` alone:

- A script created over JMAP gets a random id, registered for its name
  before the script is written, so a listing in between finds it waiting.
- A rename over JMAP registers the id for both names before it renames,
  and keeps it for the new name afterwards; the old name gets a fresh id
  to wait with.
- A destruction over JMAP gives the name a fresh id to wait with as
  well. No id a script had is handed to the next script under its name,
  whichever protocol stores that one.
- A creation or rename that fails puts back what the names had.

What stays as it was: a rename over ManageSieve is a new script to JMAP
— the new name has its derived id — and a script deleted over ManageSieve
and stored again under the same name has the same id, since ManageSieve
changes nothing in the entry. An export and a restore do not carry the
entry either: the scripts of a restored account have their derived ids.
The ids of existing entries, from the time every script had a random id,
are kept.

The entry grows only with scripts created, renamed or destroyed over
JMAP. It is bounded like the identities' (48 KiB, hundreds of entries);
a write that would outgrow it drops the entries of names that are not
scripts first — which lets such a name hand out an old id again — and
then those of the scripts last by name, whose ids fall back to the
derived ones. That is a last resort, logged once per process.

**The content** is a blob whose id is derived from the text: `S` and 40
hex digits of its SHA-256. A new content is therefore a new `blobId`
under the same id, and the blob id resolves — through the download
endpoint, `Blob/get`, `Blob/lookup` and as the source of another script
or blob — as long as a script of the account has that content. Two
scripts with the same text share a blob id.

**`SieveScript/set`** creates a script from a blob (an upload, a
`Blob/upload` creation, any blob the account can read), renames it,
replaces its content, and destroys it, the way ManageSieve's
`PUTSCRIPT`, `RENAMESCRIPT` and `DELETESCRIPT` do:

- A new content is checked as `PUTSCRIPT` checks it: at most
  `maxSizeScript` (`tooLarge`), UTF-8 text of at least one character, and
  it has to compile with the same settings and the same `include`
  resolution (the account's scripts, the global directory);
  `invalidSieve` otherwise, with the compiler's message, which names the
  line and column. A blob that is not there is `blobNotFound`.
- A name is 1 to 512 bytes of UTF-8 without the characters RFC 9661
  excludes (U+0000–U+001F, U+007F–U+009F, U+2028, U+2029;
  `invalidProperties`), unique in the account (`alreadyExists` with the
  `existingId` of the script that has it). A creation without a name, or
  with null, is named `script-<id>`. `isActive` is the server's: a
  creation or update that sets it to anything but its value is
  `invalidProperties`.
- An update changes everything or nothing (RFC 8620 5.3). After the
  checks it renames first — the step that can still fail, when another
  session took the name or renamed the script meanwhile
  (`alreadyExists`, `notFound`) — and then writes the new content under
  the new name; a content write that fails renames the script back. If
  that is impossible too — another session took the old name meanwhile —
  the error is `serverFail` saying that the script was renamed and its
  content is unchanged, so that a client does not blindly retry. The
  response names the new `blobId`, since it is not the one the client
  sent.
- The active script is not destroyed (`sieveIsActive`); it is
  deactivated first, in a call of its own, as RFC 9661 asks.
- `onSuccessDeactivateScript` and then `onSuccessActivateScript` are
  carried out only when every creation, update and destruction of the
  call succeeded. The activation names an id or a creation id of the
  call; one that names no script is ignored, as the RFC asks. A script is
  compiled for delivery before it is activated — without the IMAPSieve
  context, as ManageSieve's `SETACTIVE` does — and one that fails (a
  script that requires `imapsieve`) is not activated either: there is no
  error the RFC would report it with, so the client sees it stay
  inactive. What changed is reported with `isActive`, in `created` for a
  script the call created and in `updated` otherwise.

**State.** Script writes are not in the account change log: the state is
a hash of every script's id, name, content and activity, so it moves with
a change over ManageSieve too. `ifInState` is compared with it before
anything is written; it is not held while the call writes, since the
script store has no compare-and-swap. RFC 9661 defines no
`SieveScript/changes`, and there is none; `SieveScript/queryChanges` is
`cannotCalculateChanges`, and `SieveScript/query` says as much with
`canCalculateChanges: false`.

**`SieveScript/query`** filters by `name` (the name contains the text,
without regard to ASCII case) and `isActive`, with `AND`, `OR` and
`NOT`, and sorts by `name` (with `i;ascii-casemap` or `i;octet`) and
`isActive` (false first); anything else is `unsupportedFilter` or
`unsupportedSort`. **`SieveScript/validate`** is `CHECKSCRIPT`: the blob
is checked as a new content of `/set` is, nothing is stored, and the
answer is `error: null` or the set error — `invalidSieve`, `tooLarge`, or
`blobNotFound` for a blob that is not there.

**The vacation copy.** The script `jmap-vacation`, which shows the JMAP
VacationResponse to ManageSieve clients, is listed like any other and is
never active. An update or destruction of it is `forbidden`, and so is a
creation or rename to its name; an `onSuccessActivateScript` that names
it is refused as a whole (`forbidden`, nothing written): delivery does
not read the copy, and making it the active script would replace the
user's filter with it. The vacation changes through
`VacationResponse/set` (RFC 9661 4).

## Server-managed metadata

The entries under `/private/vendor/shardpost/jmap/` — the identities,
the vacation response, the ids of the Sieve scripts and the share
notifications — are the server's. They can be read over
IMAP (`GETMETADATA`), but `SETMETADATA` refuses to set or remove them
(`NO [NOPERM]`), in any spelling of the name, and an account import
refuses them in a mailbox's annotations (400). They change over JMAP
only, where they are checked, and the vacation is what delivery answers
from.

## Endpoints

| Path | Method | Contents |
|---|---|---|
| `/.well-known/jmap` | GET | The session resource (RFC 8620 2) |
| `/jmap/api/` | POST | Method calls, batched, with back-references |
| `/jmap/upload/{accountId}/` | POST | Blob upload |
| `/jmap/download/{accountId}/{blobId}/{name}` | GET | Blob download, with HTTP ranges |
| `/jmap/eventsource/?types=…&closeafter=…&ping=…` | GET | Event source (see Push) |
| `/jmap/healthz` | GET | Backend check: no login, no side effects |

The account id is the canonical account name. It is already stable,
unique within the server and the thing every log line names; an opaque
number would only add a mapping to keep correct. The accounts other
namespaces share with the user are listed beside it, with the owner's
account name or `~public` as their id (see [Sharing](#sharing)).

What a blob id names, and how long an upload lives, is described under
[Blobs](#blobs).

A download is served with `X-Content-Type-Options: nosniff` and a
sandboxing `Content-Security-Policy`, because the media type is whatever
the client asked for and must never be interpreted as a document of this
origin. A request for an account the user cannot reach is a 404 rather
than a 403: the server does not confirm that the other account exists.
In a shared account a download serves only the messages the user may
read and their parts, and an upload is refused (see
[Sharing](#sharing)).

## Authentication

Basic and Bearer, both verified against the same Nauthilus backend the
other front-ends use (`internal/auth`). The server verifies every
request itself even when a Director in front has already done so, so
that it is correct without a Director as well.

The backend sees `protocol=jmap` for a login and for the identity
lookup (`method=recipient_lookup`). A backend that decides per protocol
whether an account is enabled — mailbox access for IMAP, sending for
SMTP — has to know `jmap` and treat it as mailbox access; an unknown
protocol may pass without that check. Sending is checked again by the
relay, for the account XCLIENT names (see Sending).

A Bearer token comes without a user name, so the token itself has to
say whose it is: ShardPost asks the OIDC introspection endpoint
(RFC 7662, `nauthilus.introspect_url`) and takes the account from the
claims (`nauthilus.account_claims`), then looks it up in Nauthilus
without a password. It does that in either `nauthilus.bearer_mode`: in
`password` mode only for a token without a user name — a JMAP Bearer —
while an XOAUTH2 or OAUTHBEARER token of IMAP or ManageSieve, which
carries one, still goes to Nauthilus as before; in `introspect` mode
for every token. ManageSieve offers the two mechanisms since 0.35.1;
a failed token gets NO at once, without the error challenge of
RFC 7628, which neither the director nor webmail clients read. A webmail that
logged its user in with OIDC reaches the Sieve scripts that way, and so
does the director, which authenticates to the backend with the
mechanism the client used.
A user name that comes with an introspected token — SASL XOAUTH2 and
OAUTHBEARER carry one, often an alias or an address rather than the
canonical account — counts when it names the token's account or
resolves to it in a lookup without a password; a name that resolves to
another account, or to none, is refused. `introspect` mode without
`introspect_url`, and an introspection client without its secret, are
refused at startup; in `password` mode without `introspect_url` a
Bearer login without a user name is refused. The introspection
endpoint is asked with `nauthilus.introspect_client_id` and
`introspect_client_secret` (or `_file`), falling back to
`oidc_client_id`/`oidc_client_secret`: a client that may introspect the
tokens other clients were issued, which a service client usually may
not. `nauthilus.oidc_audience` and `oidc_scope`, when set, are required
of the token. Authentication on a plaintext connection is refused unless
`jmap.insecure_auth` is set.

## Configuration

**Behind nauthilus-director.** The lasting deployment puts JMAP behind
the Director (v1.1.0 and later, `protocol: jmap`): it authenticates every
request against Nauthilus, routes it by the account's `mailShard` to this
shard's JMAP listener and connects with PROXY v2 and TLS; ShardPost
authenticates the request again. `jmap.public_base_url` is the Director's
public URL, so the session resource names the Director in every URL and
nothing is rewritten on the way. The public listener takes OAuth Bearer
tokens only (`jmap.auth.basic.enabled: false` in the Director): a password
login there would tell a guesser whether a password is right past MFA and
`delayed_response` of the OIDC login. ShardPost itself still takes Basic,
which only the Director and `kubectl port-forward` reach (the acceptance
scripts use it, see `hack/jmapbase.py`). Bearer tokens are introspected by both
with the same Nauthilus client, because they form one resource server and
an RFC 8707 resource has exactly one owner. See the Director's
`docs/operations/jmap.md`, and [JMAP by example](jmap-examples.md) for the
traffic.

**One shard behind a proxy.** A proxy that does not route JMAP per
account — a load balancer in front of one ShardPost shard — lets every
account of the deployment reach this server. An account whose mail
lives on another shard would find an empty account here, and what it
wrote would land in this shard. With
`jmap.shard_attribute` and `jmap.shard` set, JMAP serves only the
accounts whose authentication result carries this shard in that
attribute (Nauthilus with the Director returns `mailShard`); any other
account, and one without the attribute, is answered 403 before its
store is opened — not 401, so a client does not ask for the password
again. Both empty serve every authenticated account; the server logs a
warning at startup when JMAP runs without them. When the attribute is
missing from the authentication result altogether, the log says so
(usually a misspelt `shard_attribute`), apart from an account that lives
elsewhere.

**Provisioning.** Fetching the session resource gives the account its
INBOX and the `server.autocreate` mailboxes it does not have and refreshes
its quota from the authentication result, as an IMAP login does — an
account that only ever uses JMAP needs Drafts and Sent too. It happens
when a client fetches the session, which it does when it starts, not on
every API request.

**Behind a TLS-terminating proxy.** The listener takes the PROXY
protocol (v1, v2) from the peers in `server.proxy_protocol_trusted_cidrs`,
like the other front-ends, so the client address — for rate limits,
logs and `XCLIENT ADDR` — is the client's and not the proxy's. A proxy
that re-encrypts towards ShardPost sends the header before TLS
(`send-proxy-v2` in HAProxy). The list of trusted peers is global and
a NodePort in between rewrites the peer address; see
[Client IP behind the Director](configuration.md#client-ip-behind-the-director).
The certificate files are reloaded when they change
(`tls.reload_seconds`).

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
  public_base_url: https://mail.example.org   # the Director's public URL
  shard_attribute: mailShard
  shard: shardpost
  max_size_upload: 52428800
  max_size_request: 10485760
  max_concurrent_upload: 4
  max_concurrent_requests: 4
  max_calls_in_request: 16
  max_objects_in_get: 500
  max_objects_in_set: 500
  max_size_blob_set: 52428800    # Blob/upload; at most max_size_upload
  max_data_sources: 64           # per Blob/upload creation; at least 64
  max_size_blob_get: 10485760    # the blob data one Blob/get returns
  max_shared_accounts: 64        # owners the session lists; the public account is one more
  max_shared_mailboxes: 1000     # mailboxes one shared account examines
  max_session_pointers: 8000     # pointers the session examines in all; 0 = 8 × max_shared_mailboxes
  max_principal_lookups: 5       # exact address lookups per API request (Principal/*, shareWith)
  max_principals: 1000           # principals a user sees without a lookup
  principal_miss_per_minute: 30  # lookups that find nothing, per account and front-end
  push:
    vapid_key_file: /secrets/jmap-vapid/vapid.pem
    vapid_subject: mailto:postmaster@example.org
    max_subscriptions: 16
    max_expiry_seconds: 604800
    event_poll_seconds: 30
    allow_internal_endpoints: false
    check_accounts: false
    # the worker (shardpost push)
    scan_seconds: 30
    poll_seconds: 60
    timeout_seconds: 10
    message_ttl_seconds: 86400
  submission:
    relay_addr: relay.example.org:587
    helo: shardpost.example.org
    starttls: true
    xclient: true
    xclient_addr: true
    check_sender: true
    address_attributes: mail,mailAlternateAddress
    max_recipients: 100
    retention_seconds: 604800
    max_kept: 500
```

The relay side: a Postfix listener of its own that relays only for
sessions XCLIENT has named a user in. Everything else it refuses, so a
host that may reach it gains nothing without XCLIENT:

```
# master.cf, a listener behind the proxy that passes the client address
<address>:<port> inet n - n - - smtpd
  -o syslog_name=postfix/jmap-submission
  -o smtpd_upstream_proxy_protocol=haproxy
  -o smtpd_tls_security_level=encrypt
  -o mynetworks=
  -o smtpd_authorized_xclient_hosts=<the addresses ShardPost reaches it from>
  -o smtpd_sasl_auth_enable=yes           # else XCLIENT LOGIN counts for nothing in permit_sasl_authenticated
  -o smtpd_sasl_type=dovecot
  -o smtpd_sasl_path=<the SASL service the submission listener uses>
  -o smtpd_sender_login_maps=<the same source the submission listener uses>
  -o smtpd_sender_restrictions=reject_authenticated_sender_login_mismatch
  -o smtpd_relay_restrictions=permit_sasl_authenticated,reject
  -o smtpd_recipient_restrictions=permit_sasl_authenticated,reject
```

Add whatever the ordinary submission listener checks for a logged-in
user — account status, quota flag, policy service — so that mail sent
through JMAP meets the same rules. `reject_authenticated_sender_login_mismatch`
applies to logged-in sessions only, which is what makes the listener
usable for nothing else. XCLIENT is trusted by address: whoever can
reach the listener from an address in `smtpd_authorized_xclient_hosts`
can name any user, so that list is as narrow as the path to it allows.

`jmap.submission.xclient_addr` (on by default) passes the JMAP client's
address as `ADDR`, so rate limits and policies see the client rather
than ShardPost. Turn it off only for a relay that relays by ShardPost's
own address (`mynetworks`): with `ADDR` the session would no longer come
from there.

How the JMAP client logged in does not reach the relay: ShardPost
checks Basic credentials and Bearer tokens alike against the
authentication backend, and the relay learns the account from
`XCLIENT LOGIN`, never a password or a token.

## Limits of this stage

- `SearchSnippet/get` (RFC 8621 5) is not here: highlighting needs the
  matched text, which the full-text index does not keep.
- `hasAttachment` is refused as a query condition, and a `header`
  condition without an indexable value reads the header of every
  candidate (see above).
- `Email/queryChanges` answers for the default order only and ignores
  `upToId` (see above). Over a public folder it shares the limitation
  of the private `\Seen` marks below.
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
- `EmailSubmission` hands messages to the relay and does not evaluate
  DSNs or MDNs, and delayed sending is not offered (see Sending).
- A message that is not kept — discarded, or only forwarded
  (`redirect` without `:copy` or `keep`) — gets no vacation reply, from
  the JMAP vacation or from a vacation in the user's script: the reply
  is sent once the message is stored, and here nothing is. This is how
  every Sieve vacation of this server behaves.
- Push has no `Identity`, `PushSubscription`, `Quota`,
  `VacationResponse`, `SieveScript` or `Principal` type in its
  `StateChange`s: none of them is in the account change log (and a
  principal state costs a `Principal/get`). A client
  that shows the quota fetches it again, for example when a push names
  `Email`.
- `SieveScript`: there is no `SieveScript/changes`, a script that does
  not compile for delivery is silently not activated, and a rename over
  ManageSieve, or an export and restore, gives a script its derived id
  rather than the one it had (see Sieve scripts).
- `Blob/lookup` answers for `Email`, `Mailbox`, `Thread` and
  `SieveScript` only, and matches a body-part blob by its message
  without checking that the part exists. An upload that a message was built from is not found by the
  message: the message is a blob of its own. The digests of `Blob/get`
  are not rate-limited beyond `maxConcurrentRequests`; a digest reads the
  range it covers.
- The private `\Seen` marks of a public folder are recorded in the
  account log of the public namespace, not of the account the mark
  belongs to: every reader's `Email/changes` in the public account names
  the message as updated, although only one reader's `$seen` changed.
- Push of shared accounts (see
  [Shared accounts in push](#shared-accounts-in-push)): a new shared
  account is seen on the next poll, not at once; a cursor that starts
  again while the last 1000 entries of the owner's log are all
  invisible moves the state without a visible change (see
  [Sharing](#sharing)); a change the RADOS store recorded without its
  mailbox is invisible in shared accounts, also when it lay in a visible
  mailbox — the client learns of it with the next visible change or
  when it resynchronises; a cursor stays in the owner's namespace after
  its users lose access (a few hundred bytes, pruned only by the bound
  of 256); the push worker keeps the shared states it pushed in one
  admin record per account.
- Sharing (see [Sharing](#sharing)): the session lists
  shared accounts without regard to subscriptions (RFC 9670 1.4, see
  above); `Principal/changes` and the `/queryChanges` of principals and
  notifications answer `cannotCalculateChanges`; principals are found
  only by an exact address beyond the ones a user sees, and have no
  display name; a notification of a change made on another shard is not
  readable; the maintenance jobs, deleting and renaming a mailbox write
  no notification;
  a share notification is written synchronously in the path of the ACL
  change (IMAP `SETACL`, the admin API, `Mailbox/set`) and opens the
  grantee's namespace, so a slow store delays the answer to the change;
  an identifier already in an ACL of the user's own mailboxes — a typo
  written over IMAP `SETACL`, for instance — counts as a visible
  principal and is accepted in `shareWith` of other mailboxes without a
  backend lookup, so such a typo can spread; a mailbox shared only with `anyone` is not found; no mailbox is
  created in another namespace; an upload into a shared account, and
  with it an attachment from an upload in a message created there, is
  refused; `Quota/*` is not offered for the public account; a change of
  only the `anyone` entry of a shared mailbox does not change the
  session state of API responses; a JMAP
  client does not clean up the private `\Seen` marks of expunged
  messages (IMAP `SELECT` does).
