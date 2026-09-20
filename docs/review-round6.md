# Review round 6 (2026-09-15, Codex gpt-6-astra, state 0.8.6 → fixes in 0.8.7)

Focus: the parts that are new since round 5 (ACL/METADATA/NOTIFY/CATENATE,
\Recent watermark, address parser, Sieve extensions, redirect/vacation).
21 findings, all addressed in 0.8.7; the implementation is given under
"Fix" in each case. The original report follows below.

| # | Finding | Fix in 0.8.7 |
|---|---|---|
| 1 | Sieve `duplicate` writes the marker during the run | The test is now only a query (`Hooks.Duplicate(key)`), the answer is cached per key within the run; `Delivery.Tracked` is written by the LMTP server only after the final result (store or discard) |
| 2 | `stop` skips the implicit keep | Fork: `ErrStop` breaks out of the loop, the finalization (implicit keep) runs as it does at the end of a script; test `TestStopKeepsImplicitKeep` |
| 3 | FETCH/CATENATE partial ranges can crash | Negative offsets/lengths are BAD, `off+size` is bounded against MaxInt64 up front; CATENATE bounds by subtraction; test `TestPartialRangesRejectedNotCrashing` |
| 4 | Parallel DELETE releases blobs twice | The registry CAS checks that the entry still carries the same mailbox ID; only the winner releases references, subscription/ACL come from its snapshot, the loser gets `ErrNoMailbox` |
| 5 | COPY/MOVE in another user's namespace without flag rights | `flagsForTarget` applies `allowedFlags` even when source and target lie in the same (foreign) namespace |
| 6 | A replacing STORE checks against a stale snapshot | `replaceFlags(rights, flags)`: the mutation runs in the store against the current flag state and preserves flags the user has no right to; test `TestReplaceFlagsKeepsProtectedFlags` |
| 7 | Vacation `:from` header injection | The value is parsed as a single address (`net/mail`), CR/LF is rejected, otherwise it falls back to the recipient; test `TestVacationFromInjection` |
| 8 | Redirect before the durable store | With a local copy, the message is stored first and forwarded afterwards; a relay error only loses the forward (logged), never the mail, and an LMTP retry does not send twice. A pure redirect without a copy still sends immediately (nothing was stored). An outbox remains open (see below) |
| 9 | Redirect loops across accounts | Header `X-Sieve-Redirected-From: <account>` on every forward; if the own account appears in it, or there are > 25 Received headers, nothing is forwarded |
| 10 | Vacation suppression period not atomic | Reservation via CAS before sending (`VacationSeen(key, period)`), release on a send error (`ttl == 0` deletes the entry, new semantics in `model.User`) |
| 11 | LMTP reset keeps `rcptRes` | `Reset` clears `rcptRes` as well |
| 12 | Shared pointer race SETACL/DELETEACL | `MailboxRec.ACLGen` counts ACL changes; pointers carry the generation, a revocation leaves a tombstone with a generation; a later write of an older generation is ignored; `ListShared` hides tombstones |
| 13 | LIST deletes pointers on temporary errors | A pointer is only removed when the mailbox is missing (`ErrNoMailbox`), and even then only when its generation is not newer (`RemoveSharedRef(ref)`); otherwise it is merely hidden |
| 14 | NOTIFY vs. writer outside `cmdMu` | The error paths (line too long, read error) take `cmdMu` for the response and the flush |
| 15 | REPLACE without `drain` on an early rejection | All early rejections consume the line including the announced literal |
| 16 | EXAMINE consumes \Recent via poll | `c.examine` prevents `claimRecent`; test `TestExamineDoesNotClaimRecent` |
| 17 | MOVE loses concurrent changes | `announceExpunge` no longer jumps ahead in the change log but calls `poll(true)`; the session's own expunges are no longer in the snapshot and print nothing, foreign deliveries and the copies of a MOVE into the same mailbox are announced regularly |
| 18 | TO/CC/BCC not distinguished | The search keys keep the header name (`key`), the FTS index only serves candidate selection, what is checked is the respective header or the parsed address list; test `TestSearchToCcBccDistinct` |
| 19 | Address parser strips spaces from quoted local parts | Words are concatenated without separators for the local part (quoted strings keep their characters), and with spaces for display names |
| 20 | Nested MODSEQ without a return value | `hasModseq` checks the criteria tree; test `TestNestedModseqInSearchResponse` |
| 21 | memstore RENAME/DELETE pass on the ACL map unlocked | `copyACL` snapshots under the mutex |

Additionally found while verifying 0.8.7 and fixed in 0.8.8:
ManageSieve `SETACTIVE ""` (deactivate) wrote an empty xattr, to which
go-ceph reacts with an index panic, killing the whole process
(`SetActiveScript` now removes the xattr). The connection goroutines of
IMAP, ManageSieve and LMTP have caught panics ever since (log with stack,
only the affected connection or transaction ends; LMTP answers 451).

Left open (deliberately): a persistent outbox for redirect/vacation
(finding 8) is architectural work for regular operation; until then the
rule is: local copy first, forward afterwards, errors logged.

---

## Original report

# Review round 6

**This state still contains data loss risks and a process crash reachable through IMAP input.** Several fixes from round 5 take effect only in partial paths.

**Basis of the review:** read-only source code review including the store/interpreter paths being called, plus an RFC comparison. No files changed, no tests run. In the absence of git metadata, the date individual bugs were introduced cannot be determined reliably.

## High severity

### 1. Sieve `duplicate` can discard the only delivery

**Locations:** `internal/lmtpd/lmtpd.go:277`, `third_party/go-sieve/interp/extensions.go:229`, `internal/cephstore/scripts.go:161`.

The test persists the key immediately, during script execution. If storing then fails temporarily, the next delivery attempt already recognizes the message as a duplicate. With `if duplicate { discard; }`, LMTP success follows without a stored message. Two identical tests within the same execution can also return different results.

**Fix:** cache results within one execution; write final tracking entries only after a successful delivery completion. Failed attempts must not produce positive duplicate hits. [RFC 7352 §3](https://www.rfc-editor.org/rfc/rfc7352.html#section-3)

### 2. `stop` skips the implicit keep

**Locations:** `third_party/go-sieve/interp/script.go:87`, `internal/lmtpd/lmtpd.go:310`.

`ErrStop` leads immediately to `return nil`, before the implicit keep is created. Even the valid script `stop;` leaves no delivery action behind and is confirmed as a successful discard by the LMTP path. Likewise affected: `vacation "..."; stop;`.

**Fix:** on `ErrStop`, leave the execution loop and then run the same finalization logic as at the normal end of a script. [RFC 5228 §3.3](https://www.rfc-editor.org/rfc/rfc5228.html#section-3.3)

### 3. FETCH/CATENATE partial ranges can crash the server process

**Locations:** `internal/imapproto/fetch.go:292`, `internal/imapproto/mailbox.go:1392`; passed on to `vendor/github.com/emersion/go-imap/v2/imapserver/message.go:159`.

FETCH accepts negative offsets and sizes; `BODY[]<-1.1>`, for example, reaches an invalid slice access. CATENATE rejects negative values but does not check the overflow of `off + ln`: `PARTIAL=1.9223372036854775807` produces a negative end index. The IMAP goroutine has no panic recovery point; the entire process is affected.

**Fix:** validate protocol-valid number ranges up front and bound partial ranges by subtraction before adding or slicing.

### 4. Parallel DELETE can delete blobs of another mailbox

**Locations:** `internal/cephstore/user.go:361`, `internal/cephstore/user.go:373`.

The registry CAS ignores the entry that was just read. Two DELETE calls can read the same metadata up front and then both release the blob references. If a COPY of the same blob exists in another mailbox, its refcount incorrectly drops to zero as a result.

A late DELETE can additionally remove a registry entry of the same name that has been re-created in the meantime. The new subscription preservation likewise uses the stale snapshot.

**Fix:** check the mailbox ID and the current registry state in the CAS; determine exactly one deletion winner. Only that one may release references. Take subscription and ACL from its successful CAS snapshot.

## Medium severity

### 5. COPY within a foreign namespace still bypasses flag rights

**Location:** `internal/imapproto/selected.go:572`.

`allowedFlags` is only applied in `copyAcross`. If Bob selects a folder of Alice's and copies into another of Alice's folders, `dt.u == c.mu` holds: `\Deleted`, `\Seen` and keywords are adopted unfiltered, although Bob only has `i` on the target. **Round 5, finding 4 remains partially open.**

**Fix:** apply target flag rights independently of the namespace branch. [RFC 4314 §4](https://www.rfc-editor.org/rfc/rfc4314.html#section-4)

### 6. A replacing STORE checks flag rights against a stale snapshot

**Locations:** `internal/imapproto/selected.go:411`, `internal/imapproto/selected.go:440`.

The new rights check examines `c.msgs`; the later store CAS, by contrast, completely replaces the flags that are current at that point. If Alice sets `\Deleted` between the check and the CAS, Bob can remove that flag with just `rs` through `STORE ... FLAGS (\Seen)`.

**Fix:** check the permitted changes inside the store mutation against its current flag state, respectively preserve protected flags.

### 7. Vacation allows header injection via `:from`

**Locations:** `third_party/go-sieve/interp/vacation.go:104`, `internal/sieve/vacation.go:125`.

The expanded `:from` value is written directly after `From:` without an address or CR/LF check. A multi-line Sieve string can inject additional headers or an early end of the header section. That does not automatically extend the SMTP recipient list, but it corrupts the generated message.

**Fix:** after variable expansion, reject control characters, parse the value as a valid address list and serialize it safely.

### 8. Redirect still happens before the durable store

**Locations:** `internal/lmtpd/lmtpd.go:295`, `internal/lmtpd/lmtpd.go:350`.

With `redirect` together with `keep` or `fileinto`, the external send happens first. If the local store then fails temporarily, LMTP returns an error; the retry sends again. The subsequent vacation fix does not resolve this redirect path.

**Fix:** record send jobs durably together with the delivery completion and then process them through an outbox. Merely swapping the two operations only shifts the retry problem.

### 9. Redirect loops across multiple accounts are not detected

**Location:** `internal/lmtpd/lmtpd.go:405`.

The target set only knows the current recipient and the targets of this one execution. With A→B→A, neither the self comparison nor `Auto-Submitted` takes effect, since redirect does not set that field. `maxRedirects` also only bounds the fan-out, not the number of loop iterations. Termination depends entirely on an external relay hop limit.

**Fix:** implement a persistently carried loop marker or a checked Received hop limit and break detected loops. [RFC 5228 §4.2](https://www.rfc-editor.org/rfc/rfc5228.html#section-4.2)

### 10. The vacation suppression period is no longer atomic after the fix

**Locations:** `internal/lmtpd/lmtpd.go:444`, `internal/lmtpd/lmtpd.go:452`, `internal/lmtpd/lmtpd.go:456`.

Two parallel deliveries both read "not yet answered", both send, and only then record the suppression period. The CAS on the final entry does not prevent the second reply that has already been sent. **Follow-up bug of the fix for round 5, finding 14.**

**Fix:** reserve a time-limited send claim atomically; confirm it after a successful send, release it on an unambiguous send error. [RFC 5230 §8](https://www.rfc-editor.org/rfc/rfc5230.html#section-8)

### 11. LMTP reset keeps the auth results of previous recipients

**Locations:** `internal/lmtpd/lmtpd.go:123`, `internal/lmtpd/lmtpd.go:208`.

`Reset` clears `rcpt` and `rcptAddr`, but not `rcptRes`. In the next transaction, the first old auth result is therefore handed to the new recipient. `provision.Apply`, for example, can overwrite that recipient's quota with the quota of a previous account. In addition, the list grows over the connection's lifetime.

**Fix:** clear `rcptRes` on every transaction reset as well.

### 12. The shared pointer race persists despite the re-read of the ACL

**Location:** `internal/cephstore/acl.go:108`.

A possible sequence: DELETEACL reads "no rights"; SETACL restores rights and pointer; the older DELETEACL then removes the pointer. The additional read still lies outside the pointer mutation. **Round 5, finding 7 is not fixed.**

**Fix:** introduce pointer versioning, respectively a repeatable, generation-bound synchronization. A single check read before the second write is not enough.

### 13. LIST removes shared pointers even on temporary errors

**Location:** `internal/imapproto/mailbox.go:308`.

Every error from `rightsOf`, including a temporary RADOS read error, leads to permanent pointer deletion. A single failed LIST query can thereby remove a still valid share from later listings.

**Fix:** on temporary errors, omit only the current output. Permanent cleanup only for a reference proven invalid and protected against concurrent changes.

### 14. NOTIFY still races with writer access outside of `cmdMu`

**Locations:** `internal/imapproto/conn.go:347`, `internal/imapproto/conn.go:355`.

The responses for overlong command lines and other read errors use the writer and flush without `cmdMu`. At the same time, `notifyLoop` can access the same buffered writer under its own lock. The IDLE DONE fix does not cover these error paths.

**Fix:** serialize these response and flush paths as well; leave the blocking network read itself outside the lock.

### 15. An early REPLACE rejection leaves literal content behind as follow-up commands

**Location:** `internal/imapproto/mailbox.go:1422`.

Unlike APPEND, the early rights/target checks leave REPLACE without a `drain`. With a non-synchronizing literal that has already been sent, its message text is subsequently read as new IMAP commands. The connection thereby loses its command boundaries.

**Fix:** on all early rejection paths, correctly consume the remaining arguments including literals that have already been announced.

### 16. EXAMINE consumes `\Recent` after all through later polling

**Locations:** `internal/imapproto/selected.go:121`, `internal/imapproto/selected.go:222`.

SELECT correctly skips `MarkRecent` for EXAMINE. If a new message arrives afterwards, however, NOOP/IDLE polling calls `claimRecent` without restriction and changes the durable watermark. A subsequent session no longer sees those messages as recent.

**Fix:** take the EXAMINE state into account for the claim as well. [RFC 3501 §6.3.2](https://www.rfc-editor.org/rfc/rfc3501.html#section-6.3.2)

### 17. MOVE can permanently lose concurrent changes from the session snapshot

**Locations:** `internal/imapproto/selected.go:333`, `internal/imapproto/selected.go:718`.

`announceExpunge` adopts the newest mailbox header without reading in all changes up to its ModSeq. If another message arrives during MOVE, its ModSeq can therefore already count as processed. On a MOVE into the same mailbox, only the session's own copies are appended afterwards; further polls skip the foreign delivery.

**Fix:** advance the snapshot and the processed ModSeq together; raise the watermark only up to changes that were actually read in.

### 18. Address search still does not distinguish TO, CC and BCC

**Locations:** `internal/imapproto/search.go:262`, `internal/imapproto/search.go:577`.

All three search keys are reduced to `FieldTo`. The final header/envelope check also searches all three fields again. A message with the searched-for recipient only in `Cc` therefore also matches `SEARCH TO`.

**Fix:** preserve the original header name in the search criterion; use the shared FTS index only for candidate selection.

## Low severity

### 19. The address parser modifies valid quoted local parts

**Location:** `internal/mailmeta/address.go:173`.

For `"a b"@example.org`, `word()` initially preserves the space correctly. The subsequent `ReplaceAll`, however, turns it into `ab@example.org`. The spelling `<"a b"@example.org>` runs through a different path and returns a different result.

**Fix:** handle syntactic whitespace separately from characters inside quoted strings; no blanket space removal on the finished local part.

### 20. Nested MODSEQ is missing from SEARCH/ESEARCH responses

**Locations:** `internal/imapproto/search.go:374`, `internal/imapproto/search.go:406`.

The output only checks `cr.modseq` of the root node. With `SEARCH RETURN (MIN) (MODSEQ 1)`, for example, the key lies in a subnode: the search is evaluated, but the required MODSEQ return value is missing.

**Fix:** record the occurrence of a MODSEQ criterion across the entire criteria tree. [RFC 7162 §3.1.5](https://www.rfc-editor.org/rfc/rfc7162.html#section-3.1.5)

### 21. memstore RENAME passes on a still mutable ACL map unlocked

**Locations:** `internal/memstore/memstore.go:187`, `internal/memstore/memstore.go:194`, `internal/memstore/memstore.go:157`.

The deadlock fix releases the owner mutex before `repoint`, but does not copy the ACL map. A parallel SETACL on the renamed folder writes to the same map while `repoint` iterates over it: a data race up to the fatal concurrent map error. The test store is affected.

**Fix:** deep-copy ACLs under the owner mutex and use only those snapshots for the subsequent pointer changes.

---

**Limits of validation:** the described sequences were traced statically, not reproduced at runtime. RADOS CAS, crash and race behavior in particular remain unconfirmed without corresponding tests. Older scrubber/recovery problems that are already documented are not listed here again in full.
