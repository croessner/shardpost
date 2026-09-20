**New data loss risks in intent recovery; substantial memory amplification during the FTS migration.** Findings verified statically, `vendor/` not reviewed.

1. **High – Confirmed deliveries can be deleted after the fact.**  
   [mailbox.go:465](/Users/croessner/GoLandProjects/imapcephpoc/internal/imapproto/mailbox.go:465), `internal/lmtpd/lmtpd.go:282`: if `EndDelivery` fails, an APPEND OK, respectively an LMTP success, still follows. The remaining intent is later rolled back by `scrub.go:147`. **Remedy:** require a durable commit state before the success response.

2. **High – Intent rollback can remove the wrong messages.**  
   [scrub.go:147](/Users/croessner/GoLandProjects/imapcephpoc/internal/cephstore/scrub.go:147), `internal/cephstore/user.go:1625`, `:1421`: what gets stored is the mailbox name and meta; what gets deleted is based on the UID without a GUID check. After a DELETE/CREATE of the same mailbox name, the UID can denote a different message. RENAME, conversely, makes the rollback run into nothing. **Remedy:** check the mailbox ID and the concrete message identity atomically on deletion.

3. **Medium – Delivery intents do not close the crash window.**  
   [mailbox.go:448](/Users/croessner/GoLandProjects/imapcephpoc/internal/imapproto/mailbox.go:448), `:461`, `internal/lmtpd/lmtpd.go:270`, `:277`: delivery happens first and is logged afterwards; a crash in between leaves unrecorded partial deliveries. In addition, the intent is removed even after a failed rollback (`mailbox.go:455`, `lmtpd.go:273`). **Remedy:** identify target steps durably up front and keep intents until completion is confirmed.

4. **High – Legacy conversion multiplies memory and doc ID assignments.**  
   [fts.go:163](/Users/croessner/GoLandProjects/imapcephpoc/internal/cephstore/fts.go:163), `:279`, `:295`: `need` contains the same GUID again for every shared term. `ensureDocIDs` only checks `cur` and therefore assigns IDs multiple times for the same missing GUID within a single attempt, leaving surplus `i:<id>` entries behind. On top of that come complete posting lists, hex strings, keys and maps across parallel shard workers, without a memory budget. **Remedy:** deduplicate before collection and assignment, chunk the migration and bound the parallelism. This matches the one-off OOM with 230 MiB RSS afterwards; without a heap/RSS profile, the concrete OOM cause is not proven.

5. **High – The QRESYNC fallback needs memory proportional to UIDNEXT.**  
   [mailbox.go:613](/Users/croessner/GoLandProjects/imapcephpoc/internal/imapproto/mailbox.go:613), `:620`, `internal/imapproto/numset.go:108`: without known UIDs, all historical UIDs are materialized, the missing ones are collected once more and copied for `formatSet`. Even a mailbox that is empty by now can overwhelm a pod this way. **Remedy:** generate VANISHED directly as compressed gap intervals from the existing UIDs.

6. **High – The new mbox deletion skips the grace check on stat errors.**  
   [scrub.go:432](/Users/croessner/GoLandProjects/imapcephpoc/internal/cephstore/scrub.go:432): every `Stat` error still leads on to `Delete`. A mailbox created after the registry snapshot can thus be deleted despite being in use, if its age check fails. **Remedy:** do not delete on read errors; re-verify registration and object version before deletion.

7. **Medium – Short-lived OIDC tokens are renewed on every call.**  
   [oidc.go:60](/Users/croessner/GoLandProjects/imapcephpoc/internal/auth/oidc.go:60): with `expires_in <= 60`, every freshly fetched token already lies within the fixed refresh window. Every waiting caller therefore performs another token request under the mutex. **Remedy:** bound the refresh lead time relative to the actual lifetime.

**Review of the specifically re-checked items from round 3**

| Item | Status now |
|---|---|
| New #1 – permanent budget blockage | **Fixed:** bounded waiting (`membudget.go:82`); the additional LMTP EOF step corrected (`lmtpd.go:149`). |
| New #2 – STORE ModSeq going backwards | **Partially:** `max` prevents backward movement (`user.go:1291`). Two different flag states can, however, receive the same ModSeq; `CHANGEDSINCE` then misses the later change. |
| New #3 – PendingFloor after snapshot / 512-entry window | **The described bugs fixed:** the check happens before the snapshot (`mailbox.go:675`), probing instead of a window (`user.go:840`), error propagation. |
| New #4 – STORE result contains discarded CAS attempts | **Fixed:** the result is reset per attempt (`user.go:1269`). |
| New #5 – MOVE without its own blob reference | **Fixed in the regular path:** reference before the target link, release of the sources actually removed (`user.go:1578`, `:1595`). |
| R1 #13 – parallel quota overrun | **Reservation fixed:** `reserveQuota` in Deliver/Copy. Scrub can still overwrite reservations (`scrub.go:455`). |
| R1 #18/#26 – MULTIAPPEND/partial Sieve delivery | **Not fully fixed:** see the new intent findings. |
| R1 #3/#11 – Pending/QRESYNC | **Partially:** the response without known UIDs was added. Pending still expires after 30 seconds; VANISHED is still written before the shard commit (`user.go:793`, `:665`). |
| R1 #6/#7 – destructive scrub | **Still open:** a missing last shard remains unprotected; the two-stage orphan check still ends without atomic deletion safeguards (`scrub.go:263`, `:406`). |
| R1 #8/#9 – MOVE recovery | **Partially:** an own reference was added; any GUID hit in the target still substitutes for the concrete MOVE target identity (`scrub.go:211`). |
| R1 #31 – FTS scaling | **Partially:** more compact postings; complete lists and the new migration amplification remain. |

The existing tests for auth, postings, membudget, IMAP, POP3, FTS and Sieve pass from the test cache. The Nauthilus kustomize overlay rendered successfully. Ceph/LMTP/server tests fail on the missing `rados/librados.h`; crash behavior and load peaks remain unconfirmed at runtime.
