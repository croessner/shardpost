# Public folders

Status: package A live with 0.25.0, package B live with 0.26.0
(2026-09-20).
Decided with the owner; a review and a rollout for each package.

Shared folders exist: a mailbox of an account, opened by someone else
under `Shared/<owner>/<name>` with RFC 4314 rights. What is missing
are folders that belong to no one — an archive, an announcement folder, a
team inbox that outlives the person who created it.

## What Dovecot does, and what we take from it

| Dovecot | Here |
| --- | --- |
| `namespace { type = public; prefix = Public/ }`, storage below a path | Reserved store namespace `~public`, IMAP prefix `Public/` |
| `dovecot-acl` file per mailbox, changed on disk | ACL in the mailbox record, changed at runtime through the admin API |
| No rights by default in public namespaces | Same: no rights unless an entry grants them |
| `subscriptions = no`: subscriptions land in the user's own namespace | Same: the subscription belongs to the user, not to the folder |
| `INDEXPVT`: a private index per user, `\Seen` only | A private flag object per user, `\Seen` only |
| Delivery through Sieve `fileinto` into the public namespace, right `p` | Same |
| Public folders have no user quota; an optional shared namespace quota exists (`:ns=Public/`) | The namespace has its own counters; an optional limit from the configuration |

The one place where we are deliberately better: Dovecot keeps the rights
in files that a running server does not re-read on its own, so a change
means editing files on disk and reloading. Here the rights are data in
the store, `shardpostctl acl set` changes them, and the next command sees
the new state.

## Decisions

1. **Namespace.** Public folders live in the reserved store namespace
   `~public`, alongside `~admin` and `~learning`. It is not an account:
   no login, no user database entry, no INBOX.
2. **Addressing.** Clients see `Public/<name>`. The prefix is a server
   setting (`server.public_prefix`, default `Public/`), because clients
   migrated from another server may expect a different one.
3. **Rights.** Only ACL entries grant access; there is no implicit owner.
   `anyone` covers every authenticated account, an entry for an account
   wins over it. Without the `l` right a folder is invisible, exactly as
   for shared folders.
4. **Visibility.** `LIST` reads the public registry directly and filters
   by the `l` right. No pointer is written into each account's registry:
   a public folder that a thousand accounts may see must not cost a
   thousand writes.
5. **Subscriptions** are per account and live in the account's own
   registry, so every user decides for themselves what their client
   shows.
6. **Private `\Seen`.** `\Seen` is private per account, stored in an
   object of that account's namespace, keyed by mailbox id and UID.
   Every other flag and every keyword is shared, which is what makes a
   team folder useful: "answered" and "flagged" are statements about the
   folder, "read" is a statement about a person. This follows Dovecot,
   where the private index holds `\Seen` and nothing else. The shared
   `\Seen` of the message stays what a delivery, an APPEND or a
   migration put there and is the state for everyone who has no mark of
   their own yet; a STORE never changes it, it writes the account's own
   mark. So a message that arrives read is read for everybody until
   somebody says otherwise for themselves. This holds for the implicit
   `\Seen` of a `FETCH BODY[]` as well — most clients use it instead of
   `BODY.PEEK[]`, and it is the most common way a message is marked
   read at all.
7. **MODSEQ.** A private mark is a change of the public mailbox like
   any other, and it takes its MODSEQ from that mailbox's counter: the
   session allocates the next number there, announces the UIDs in the
   change log and writes the mark under that number. So there is one
   number space per mailbox, `HIGHESTMODSEQ` advances when somebody
   marks a message read, and CONDSTORE and QRESYNC work for private and
   shared changes alike. A private counter of its own would collide
   with the next shared change — a client filtering on `> value` would
   then miss it — and `NOMODSEQ` (RFC 7162 3.1.2.2) would force a full
   flag synchronization on every SELECT, which for a large public
   archive is exactly the cost we avoid elsewhere. Other accounts read
   the record of the message once and then send nothing, because
   nothing changed for them; the other sessions of the same account
   re-read the marks of exactly those UIDs and report the new state. The mark is
   written while the change is still pending, so no session learns of
   the number before the mark is in place. Key of a mark:
   `(mailbox id, UIDVALIDITY, UID)`. An EXPUNGE by one account cannot
   reach into another account's namespace, so every account drops the
   marks of messages that are gone when it opens the folder, at most
   1000 per SELECT.
8. **Quota.** The namespace counts its own usage, like every namespace
   here. The limit comes from the configuration
   (`server.public_quota_bytes`, `server.public_quota_msgs`, default 0 =
   unlimited). A message filed into a public folder does **not** count
   against the account that filed it; that matches Dovecot, where the
   user quota root covers the user's own namespace.
9. **Delivery** goes through Sieve: a normal account receives the
   message and its script files it into the public folder
   (`fileinto "Public/Support/Inbox"`). This requires the `p` right for
   that account on that folder, and the message is stored in `~public`.
   No mail is delivered to a public folder directly, because a public
   folder has no address of its own. A delivery that names its mailbox
   explicitly (`deliver -m`, LMTP with a target mailbox) reaches a
   public folder the same way and under the same right, but never falls
   back: whoever names a mailbox wants that one, so a missing folder or
   a missing right is an error.
10. **Account deletion.** When an account is purged, its ACL entries in
    public folders go with it, and so do its private `\Seen` records
    (they live in its own namespace and are removed anyway).
11. **Administration** uses the existing tools: the admin API and
    `shardpostctl` address `~public` like an account, so provisioning,
    ACL, quota, diagnosis, export, restore and maintenance jobs work
    unchanged. The API accepts the reserved name without a user database
    lookup; delivery and import sessions refuse it.

## Package A — the namespace

Store: open `~public`, keep the scrubber, `ListUsers` and account purge
aware of it. IMAP: prefix handling, `NAMESPACE`, `LIST`/`LSUB` including
per-account subscriptions, `SELECT`/`EXAMINE`/`STATUS`, `FETCH`,
`SEARCH`, `STORE`, `APPEND`, `COPY`, `MOVE`, `EXPUNGE`, all with rights
enforcement. Admin: accept `~public`, configured quota limit. CLI and
manual pages.

In package A, `\Seen` is still shared. That is documented, and it is
acceptable for an archive; it is not acceptable for a team inbox, which
is what package B is for.

Acceptance: a folder created by an administrator is visible to a second
account with `anyone lr`, invisible without the `l` right; APPEND needs
`i`, EXPUNGE needs `e`; a purge of an account removes its entries; the
usage of `~public` is counted and bounded by the configured limit.

## Package B — private `\Seen` and delivery

The private flag object per account, `\Seen` handling in `FETCH`,
`STORE`, `SEARCH` (`SEEN`, `UNSEEN`, `NEW`), `STATUS` (`UNSEEN`,
`RECENT`) and the `\Seen` part of `COPY`/`MOVE`; Sieve `fileinto` into
the public namespace with the `p` right.

Copying out of a public folder into a private mailbox takes the copying
account's `\Seen` with it; a copy inside the public namespace cannot
carry everyone else's private marks, so the copying account's state
becomes the new shared default, and everyone else starts from there.

A delivery that files into a public folder and keeps the message writes
into two namespaces. Its delivery intent — what lets the scrubber roll
back a half-finished delivery after a crash — therefore exists in both:
an intent, like the rollback it exists for, only finds a message in the
namespace that holds it.

Acceptance: two accounts read the same folder and see their own
`\Seen`; `STATUS UNSEEN` differs between them; a flag other than `\Seen`
set by one is visible to the other; a Sieve script files into a public
folder when it has `p` and is refused without it; a `FETCH BODY[]` by
one account leaves the folder unread for everyone else. For CONDSTORE
clients: a private mark moves `HIGHESTMODSEQ`, is reported by the STORE
itself, is found by `CHANGEDSINCE`, reaches the account's other
sessions, and a shared change made after it still reaches everyone.
