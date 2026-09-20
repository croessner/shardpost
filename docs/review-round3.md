**Still no release for production mail stores.** The shard fixes eliminate major races, but leave bugs in ModSeq ordering, snapshot reconciliation and budget reservation in place.

"Fixed" means verified statically. The tests for IMAP, POP3, auth, FTS and Sieve pass. The full run fails on the missing `rados/librados.h`; Ceph, crash and race tests remain unconfirmed. `vendor/` was not reviewed.

| Finding | Status |
|---|---|
| R1 #1 – Foreign IOContext version | **Fixed:** exclusive version read context, `internal/cephstore/verpool.go:72`. |
| R1 #2 – Flags/CONDSTORE outside CAS | **Partially:** the current shard state is re-read; new ModSeq/retry bugs below. `internal/cephstore/user.go:1191`. |
| R1 #3 – Publication before shard commit | **Open:** pending is still time-bounded; PendingFloor does not close the snapshot race. `internal/cephstore/user.go:764`, `internal/imapproto/mailbox.go:650`. |
| R1 #4 – Duplicate EXPUNGE reference release | **Fixed for concurrent EXPUNGEs:** `modifyEntries` resets the removed keys on every attempt; only successfully removed entries are released. `internal/cephstore/user.go:957`, `:1355`. |
| R1 #5 – COPY revives deleted blob | **Fixed:** a positive reference change against zero is rejected. `internal/cephstore/blob.go:138`. |
| R1 #6 – Scrub after incomplete scan | **Partially:** registry and intermediate shards are checked; a missing **last** shard remains unprotected. Deletions also happen before the completeness decision. `internal/cephstore/scrub.go:212`, `:238`. |
| R1 #7 – Concurrent scrub repairs | **Open:** scan and repair remain inconsistent; orphan deletion without CAS. `internal/cephstore/scrub.go:277`, `:330`. |
| R1 #8/#9 – MOVE recovery/intent | **Partially:** target failures and the regular intent completion improved; GUID equality still substitutes for a concrete target identity, recovery remains insufficiently idempotent. `internal/cephstore/scrub.go:172`, `internal/cephstore/user.go:1380`. |
| R1 #10 – RENAME collisions | **Fixed:** live listing inside the registry CAS attempt. `internal/cephstore/user.go:399`. |
| R1 #11 – Log/QRESYNC gaps | **Partially:** monotonic trimming; without known UIDs still only an ALERT. Pending EXPUNGEs already appear in the vanished log. `internal/imapproto/mailbox.go:586`, `internal/cephstore/user.go:638`. |
| R1 #12 – Switching off one's own quota | **Fixed in the default setup:** explicit opt-in and value validation. `internal/imapproto/mailbox.go:739`. |
| R1 #13 – COPY quota | **Partially:** the batch total is taken into account, but there is no atomic reservation between parallel deliveries. `internal/cephstore/user.go:1399`. |
| R1 #14 – Parser resources | **Partially:** literal bytes are summed, the argument count is still unbounded. Arbitrarily many `{0+}` continuations consume token/callback memory at zero budget bytes. `internal/imapproto/wire.go:183`, `internal/imapproto/conn.go:294`. |
| R1 #15/#16 – STORE infinite loop/metric cardinality | **Fixed:** `internal/imapproto/selected.go:351`, `internal/imapproto/conn.go:202`, `internal/pop3proto/pop3.go:202`. |
| R1 #17 – Deferred EXPUNGEs | **Fixed in the described reload path:** the old watermark is preserved when removals are deferred. `internal/imapproto/selected.go:175`. |
| R1 #18 – MULTIAPPEND atomicity | **Partially:** the rollback remains best-effort, without crash recovery. `internal/imapproto/mailbox.go:438`. |
| R1 #19 – False FETCH success | **Fixed:** macro expansion and error propagation. `internal/imapproto/fetch.go:144`, `:374`. |
| R1 #20 – WriteDeadline | **Partially:** renewed before command processing; automatic writer output still has no renewal of its own. `internal/imapproto/conn.go:367`, `internal/imapproto/wire.go:331`. |
| R1 #21 – POP3 lease loss | **Partially:** a detected loss blocks transaction commands; QUIT, however, then skips deletions and still answers positively. `internal/pop3proto/pop3.go:326`, `:612`. |
| R1 #22/#24 – Session resources/ManageSieve escapes | **Fixed:** `internal/imapproto/conn.go:744`, `internal/sieve/managesieve.go:330`, `:237`. |
| R1 #23 – Sieve activation/rename | **Partially:** rename content and active status still come from unprotected up-front reads; activation is separate. `internal/cephstore/scripts.go:129`, `:153`. |
| R1 #25/#27 – LMTP TLS setting/HTTP auth errors | **Fixed:** `internal/lmtpd/lmtpd.go:46`, `internal/auth/nauthilus.go:269`. |
| R1 #26 – Partial Sieve delivery | **Partially:** rollback without a durable delivery intent. `internal/lmtpd/lmtpd.go:269`. |
| R1 #28 – Rotation/config race | **The described paths fixed:** confirmed key and a locked ListUsers config read. `internal/rookcreds/rookcreds.go:147`, `internal/cephstore/scrub.go:61`. |
| R1 #29 – SEARCH correctness | **Partially:** text criteria are still removed; the candidate limit is now 4096 instead of 256, still without a completeness guarantee. `internal/imapproto/search.go:522`, `internal/cephstore/fts.go:371`. |
| R1 #30 – Memory budget | **Partially:** buffer lifetimes are tracked better; copies, FETCH output buffers, POP3 line arrays and snapshots are still missing. `internal/imapproto/fetch.go:415`, `:429`, `internal/pop3proto/pop3.go:573`. |
| R1 #31 – FTS/scrub scaling | **Still open:** complete posting lists and namespace enumeration. `internal/cephstore/fts.go:467`, `internal/cephstore/scrub.go:75`. |
| R2 new #1 – Blob deletion after commit error | **Fixed:** a successful shard write stays successful despite a commit error. `internal/cephstore/user.go:1074`. |
| R2 new #2 – Pending does not serialize shard changes | **Partially:** the flag RMW and the deletion decision are now bound to a version; ModSeq ordering remains wrong, see below. |
| R2 new #3 – Poll stuck on a missing UID | **Partially:** the concrete blockage is eliminated; slow writers are still skipped after 30 seconds. `internal/imapproto/selected.go:73`, `internal/cephstore/user.go:764`. |
| R2 new #4 – MULTIAPPEND memory DoS | **Partially:** the byte total is bounded; zero literals and budget stalls remain. `internal/imapproto/conn.go:285`. |
| R2 new #5 – Automatic buffer flushes | **Partially**, see R1 #20. |
| R2 new #6 – Script name check after mutation | **Fixed:** the check happens before reads and mutation. `internal/cephstore/scripts.go:126`. |
| R2 new #7 – Header unfolding/swallowed read errors | **The described cases fixed:** unfolding and error propagation to SEARCH. `internal/imapproto/search.go:499`, `:674`. |

**New findings**

1. **High – Budget reservations can block each other permanently.**  
   `internal/membudget/membudget.go:69`, `internal/imapproto/conn.go:289`, `internal/lmtpd/lmtpd.go:148`. Two commands already hold partial reservations and wait for further bytes; release only happens at the end of the command. Even a single LMTP message of exactly 256 MiB blocks on the additional EOF read attempt against the default 256 MiB budget. Socket timeouts do not interrupt `cond.Wait()`. Extensions must fail non-blockingly or be reserved completely up front.

2. **High – STORE can reset the message ModSeq.**  
   `internal/cephstore/user.go:625`, `:1209`. A reserves ModSeq 10, B reserves 11 and writes first. A then correctly reads B's flags, but writes its change with **10**. The shard CAS protects the content, not the order reserved up front. CONDSTORE and CHANGEDSINCE become incorrect as a result. ModSeq assignment and the actual write order must be protected together.

3. **High – PendingFloor misses a commit between snapshot and re-check.**  
   `internal/imapproto/mailbox.go:642`, `:646`, `:650`; `internal/cephstore/user.go:677`, `:789`. The header contains pending ModSeq 10, the snapshot does not yet contain its message; afterwards the writer writes and commits. PendingFloor now no longer finds anything. The session stays at 10 and skips the message permanently. In addition, the lookup ignores pending entries outside the last 512 ModSeqs as well as query errors. What is needed is a validated snapshot boundary across all reads involved.

4. **Medium – Failed CAS attempts make it into the STORE result.**  
   `internal/cephstore/user.go:1191`, `:1201`, `:1211`; repeated in `internal/cephstore/fts.go:530`. The callback appends `Updated`, `Modified` and keywords on every attempt. After a conflict, these results are not discarded. For example, a supposed change remains in `Updated` even though the successful retry skips the UID that has since been deleted. Results should be isolated per attempt and adopted only after a successful CAS.

5. **High – MOVE races unprotected with EXPUNGE for the last blob reference.**  
   `internal/cephstore/user.go:1469`, `:1475`, `:1277`. MOVE creates the target without an additional reference. Between the target link and the source unlink, EXPUNGE can remove the source and drop the only refcount to zero. The target index is left behind without a blob; the intent does not prevent this. Two parallel MOVEs can likewise create multiple target references with an unchanged refcount.
