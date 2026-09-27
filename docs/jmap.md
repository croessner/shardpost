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
| 4 | `Email/query`, `Email/queryChanges`, `Email/set`, `Email/import`, `Email/parse`, `Email/copy` | built |
| 5 | Push (EventSource and Web Push), `Identity/*`, `EmailSubmission/*` | built; delayed sending prepared, not offered |
| 6 | `Quota/*`, `VacationResponse/*` | built |
| 6 | `SieveScript/*`, `Blob/*`, sharing | planned |

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
| `text`, `subject`, `body` | The full-text index, with the word semantics IMAP `SEARCH` has here |
| `from`, `to`, `cc`, `bcc` | The full-text index narrows by the words of the value (`to`, `cc` and `bcc` share an index field); the envelope decides: every word has to occur, without regard to case, in `name <address>` of one address |
| `header` | The full-text index narrows a value; the header block of each candidate decides. A name alone, or a value without an indexable word, reads the header of every candidate |
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

**`Email/copy`** copies between accounts, and a session here has one:
another account is `fromAccountNotFound`, the same account on both sides
`invalidArguments` (RFC 8620 5.4). Within an account, a copy is an
update of `mailboxIds`.

**The new state** of a write advances over the log entries that name
only the call's own messages. Such an entry can still be another
session's — an IMAP `STORE` on the message the call just moved — so the
messages are read back afterwards, and whatever differs from what the
call meant is reported in `created` or `updated` (RFC 8620 5.3 lets the
server report properties it changed beyond the request): keywords and
`mailboxIds` as they are now. A destroyed message that is still there
keeps the old state. A client that adopts the new state therefore ends
up at least as far as the state says, never behind it.

## Push

Push (RFC 8620 7) tells a client that something changed, never what:
a `StateChange` naming the data types whose state moved. Both ways of
telling it compute that from the account change log.

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
Each `state` event carries the state as its event id; a client that
reconnects with `Last-Event-ID` is sent what it missed at once.
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
treated as an attack vector: it has to be `https`, a front-end refuses
one that names an internal address, and the worker refuses to connect to
any loopback, private, link-local or shared address after name
resolution, and follows no redirects. An in-house push service needs
`jmap.push.allow_internal_endpoints` on both sides.

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
ManageSieve and the admin API show it but refuse to store, rename,
delete or activate it; an import of scripts that names it is refused,
and an export, a restore or a migration leaves it out.

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

## Server-managed metadata

The entries under `/private/vendor/shardpost/jmap/` — the identities
and the vacation response — are the server's. They can be read over
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
while an IMAP XOAUTH2 or OAUTHBEARER token, which carries one, still
goes to Nauthilus as before; in `introspect` mode for every token.
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
  public_base_url: https://jmap.example.org
  shard_attribute: mailShard
  shard: shardpost
  max_size_upload: 52428800
  max_size_request: 10485760
  max_concurrent_upload: 4
  max_concurrent_requests: 4
  max_calls_in_request: 16
  max_objects_in_get: 500
  max_objects_in_set: 500
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
- Push has no `Identity`, `PushSubscription`, `Quota` or
  `VacationResponse` type in its `StateChange`s: none of them is in the
  account change log. A client
  that shows the quota fetches it again, for example when a push names
  `Email`.
- The private `\Seen` marks of a public folder are recorded in the
  account log of the public account, not of the account the mark belongs
  to. JMAP Sharing (RFC 9670) is where that is resolved.
