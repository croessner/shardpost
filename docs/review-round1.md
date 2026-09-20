**I would not yet release this state for production mail stores.** Particularly critical are possible data losses caused by the scrubber, refcount management and insufficiently protected store modifications. On top of that, there are several ways to bring down the entire server process through protocol input.

The three documents were read, as well as the code under `cmd/` and `internal/`, including tests; `vendor/` was excluded. The findings were cross-checked statically, but not reproduced through runtime, race detector or Ceph integration tests. The [full review report](/private/tmp/codex-security-scans/imapcephpoc/review-20260914/report.md) additionally contains further individual findings.

1. **CAS may use the version of another operation — high.**  
   `internal/cephstore/fts.go:493–505`, parallel callers at `:107–109`. Between `ReadOp.Operate` and `GetLastVersion()`, other operations on the same `u.idx` may run. The go-ceph version in use returns an **IOContext-wide** last object version, not a version bound to this read. This causes spurious CAS conflicts and, with a matching version number, can let stale updates through. **Suggestion:** dedicated IOContexts for parallel workers, or full serialization of all context operations up to taking the version.

2. **Flags and CONDSTORE are checked outside the protected read/modify/write — high.**  
   `internal/cephstore/user.go:941–991`. Messages are read before `casHeader`; even after a CAS conflict, existence, flags and `UNCHANGEDSINCE` are not re-evaluated. Two parallel `+FLAGS` can overwrite each other's changes. An EXPUNGE that happened in between can be undone by the later write of the old meta record — possibly without an existing blob. **Suggestion:** re-read and re-check existence, ModSeq and flags within the same protected transaction.

3. **Higher shards publish changes before they are actually committed — high.**  
   `internal/cephstore/user.go:611–617`, `:883–907`, `:964–999`. Header, change log and notify are updated before the entry is written to a higher shard. A session can consume the new ModSeq, not yet find the entry, and then miss it permanently. In addition, later shard writes can overtake earlier ones. **Suggestion:** introduce a confirmed commit watermark; perform shard updates bound to a version and publish only after completion. Deferring only the notify is not sufficient.

4. **EXPUNGE can release the same blob reference multiple times — high.**  
   `internal/cephstore/user.go:1027–1058`. `unlink` does not check which of the requested UIDs actually still exist; afterwards all passed references are decremented. Example: a message also exists in Archive. Two sessions use the same INBOX snapshot and each remove it; the second operation can delete the Archive blob. `CLOSE` likewise uses an old snapshot. **Suggestion:** atomically determine the entries actually removed and release only their references, exactly once.

5. **A reference dropped to zero can be revived during deletion — high.**  
   `internal/cephstore/blob.go:146–157`, `internal/cephstore/user.go:1087–1090`. Further operations lie between `refs=0` and `deleteBlob`. A parallel COPY can raise the counter again and create a target index; the deletion path already under way removes the data anyway. **Suggestion:** introduce an atomic, final GC state from which new references are rejected.

6. **The scrubber deletes valid mail after an incomplete index scan — high.**  
   `internal/cephstore/scrub.go:162–175`, deletion at `:245–268`. Header or index read errors merely lead to `continue`. The GUIDs missing as a result are later treated as orphans and deleted, provided they are old enough. The grace period does not protect existing messages here. **Suggestion:** on an incomplete reference scan, abort all destructive repairs as well as refcount and quota corrections. Corrupted, skipped meta records must also invalidate completeness.

7. **Scrub repairs are not protected against concurrent changes — high.**  
   `internal/cephstore/scrub.go:209–217`, `:285–292`. Between the mtime check and the unconditional `SetXattr`, COPY can increase the refcount; the scrubber then overwrites it with its own stale count. A later EXPUNGE can then delete data that is still referenced. Quota is likewise overwritten from a temporally inconsistent scan. **Suggestion:** consistent scan/repair boundaries with fencing or version checking; recount when there are competing changes.

8. **MOVE recovery removes the source despite a failed target placement — high.**  
   `internal/cephstore/scrub.go:127–152`. If the target is missing or `u.link` fails, `unlinkWithIntent` is nevertheless executed for the source messages afterwards. This can make the last index reference disappear; the orphan phase then removes the blob. **Suggestion:** preserve source and intent until every concrete target link is confirmed. A vanished target must be handled conservatively.

9. **MOVE removes its recovery intent too early — high.**  
   `internal/cephstore/user.go:1176–1191`. The intent disappears in the header CAS before source messages are removed on higher shards. A crash leaves source and target behind, but only the original refcount and no intent any more. **Suggestion:** remove the intent only after all durably completed steps; make recovery idempotent based on unique operation and entry identities.

10. **RENAME can overwrite existing target subfolders — high.**  
    `internal/cephstore/user.go:387–401`. Only the exact new parent name is checked. If `A/B` and `X/B` exist while `X` is merely an implicit parent node, `RENAME A X` can replace the registry entry of `X/B`. Its previous contents become inaccessible. **Suggestion:** check all resulting target names for collisions within a consistent registry view.

11. **Log trimming and QRESYNC do not reliably detect history gaps — medium.**  
    `internal/cephstore/user.go:649–659`, `internal/imapproto/mailbox.go:562–584`. An older parallel trim can reset `h:logstart` even though newer trims have already deleted more records. Independently of that, QRESYNC replay does not check `LogStart` at all and returns incomplete VANISHED responses for old client states. **Suggestion:** advance `LogStart` monotonically via CAS and, on history gaps, reconcile known UIDs against the current contents.

12. **Any logged-in user can switch off their administrative quota — high.**  
    `internal/imapproto/mailbox.go:674–708`. `SETQUOTA "" (STORAGE 0 MESSAGE 0)` is stored permanently without any special permission. In the store, zero means unlimited; negative values and an overflow in `n*1024` are likewise unchecked. **Suggestion:** reject SETQUOTA for normal users or require a separate administrative permission; validate values and overflows.

13. **COPY bypasses the quota even without concurrency — medium.**  
    `internal/cephstore/user.go:1082–1099`. Every element of a COPY batch checks the same stale usage; the total is only booked after the entire batch. Many individually acceptable messages can jointly exceed the limit. On a later partial failure, earlier copies even remain unbooked. **Suggestion:** reserve the total requirement atomically and reliably account for the steps actually completed.

14. **Unbounded parser input enables DoS before login — high.**  
    `internal/imapproto/wire.go:75`, `internal/pop3proto/pop3.go:191`, `internal/sieve/managesieve.go:164–213`. Lines are accumulated up to LF without a size limit. ManageSieve additionally collects arbitrarily many individually permissible literals; over `ID`, IMAP allows unboundedly nested lists. Literal limits and time limits do not sufficiently bound these resources. **Suggestion:** hard limits on line bytes, total command bytes, argument count and nesting before the respective allocation.

15. **A short STORE command creates an infinite loop with memory growth — high.**  
    `internal/imapproto/selected.go:322–324`. After SELECT, `STORE 1 +FLAGS foo (` suffices: `atom()` fails without parser progress, its error is ignored, and the loop keeps appending empty flags. Socket timeouts do not terminate this CPU loop. **Suggestion:** return parser errors immediately and validate the flag grammar strictly.

16. **Unknown commands create unboundedly many permanent Prometheus series — high.**  
    `internal/imapproto/conn.go:324`, `internal/pop3proto/pop3.go:205`. Arbitrary command names are used as labels even before authentication. A single connection can continuously send new names; the series persist after the connection ends. **Suggestion:** use a fixed command list and map all unknown names to `UNKNOWN`.

17. **Deferred EXPUNGE notifications are lost permanently — medium.**  
    `internal/imapproto/selected.go:66–107`. `poll(false)` suppresses removals during FETCH/STORE/SEARCH, but still adopts the new `HighestModSeq`. A subsequent NOOP takes the unchanged watermark as evidence that there is nothing to do. Phantom messages and incorrect EXISTS/sequence states persist. **Suggestion:** store pending removals separately and apply them on the next permissible command.

18. **MULTIAPPEND is not atomic — medium.**  
    `internal/imapproto/mailbox.go:396–433`. Every literal is stored immediately via `Deliver`. An error or connection drop on the second literal leaves the first message behind, even though the overall command did not complete successfully. Retries create duplicates. **Suggestion:** implement an atomic batch commit; until then, do not advertise MULTIAPPEND.

19. **FETCH confirms missing or incomplete data as success — medium.**  
    Two independent bugs: `internal/imapproto/fetch.go:130–147` appends the expansion of ALL/FAST/FULL to the input list during a `range`; the new elements are never processed. At `:351–353`, a blob read error is additionally returned as `nil`, upon which the command ends with OK. **Suggestion:** expand macros beforehand and propagate read errors as a failed command.

20. **WriteTimeout is consumed during normal idle time — medium.**  
    `internal/imapproto/conn.go:270–272`, IDLE at `:751–815`. The write deadline is set before the blocking read. After more than the default two minutes of idling, even the next NOOP response can fail, although the IdleTimeout is nowhere near reached. The same affects later IDLE pushes and DONE. **Suggestion:** renew the write deadline immediately before actual write operations.

21. **POP3 loses maildrop exclusivity during active sessions — medium.**  
    `internal/pop3proto/pop3.go:439`, `internal/cephstore/user.go:1226–1238`. The lock is acquired once with a TTL and never renewed. NOOPs keep the session open beyond the ten minutes; afterwards a second session can lock the same maildrop. **Suggestion:** lease renewal with safe handling of a lease loss, or a correspondingly limited session lifetime.

22. **Faulty session management leaks resources permanently — medium.**  
    `internal/imapproto/conn.go:675–685`: if `Store.User` fails, the previously reserved per-user connection slot is not returned; after enough failures the account stays blocked even though the storage has recovered. `internal/sieve/managesieve.go:432–437`: a repeated AUTHENTICATE overwrites the old user without Close and leaks native IOContexts as well as connection references. **Suggestion:** roll back slot reservations on error paths and reject ManageSieve re-authentication.

23. **Sieve can silently run the wrong filter or none at all — medium.**  
    `internal/cephstore/scripts.go:38–42`: active names longer than 256 bytes can be written but not read; the ignored xattr error appears as "no active script". At `:119–128`, RENAMESCRIPT onto an existing target name reports success and may activate that target's existing content. **Suggestion:** bound names consistently, propagate read errors, and perform rename together with the active status atomically and with an explicit collision check.

24. **ManageSieve quoted strings are not decoded correctly — low.**  
    `internal/sieve/managesieve.go:188–194`. The first quotation mark terminates the string even after a backslash; escapes are not decoded. Valid scripts with escaped quotes and emitted script names therefore cannot be read back reliably. **Suggestion:** use a scanner with explicit escape handling.

25. **LMTP ignores the secure auth configuration — medium.**  
    `internal/lmtpd/lmtpd.go:50`. `AllowInsecureAuth` is hard-wired to true, independent of `INSECURE_AUTH=false`. With service credentials configured, AUTH PLAIN before STARTTLS remains permitted. **Suggestion:** configure the TLS requirement for LMTP service auth explicitly and consistently.

26. **Partial Sieve delivery creates duplicates on LMTP retries — medium.**  
    `internal/lmtpd/lmtpd.go:217–223`. Multiple fileinto targets are written durably one after another. If a later target fails, the entire recipient gets an error even though earlier placements remain. **Suggestion:** use persistent delivery intents with idempotent target steps, or an atomic batch.

27. **HTTP auth classifies backend problems as a wrong user password — medium.**  
    `internal/auth/nauthilus.go:263–281`. HTTP 401 is treated like a definitive user rejection, although faulty backchannel credentials can be the cause. An error inside an HTTP 200 response is likewise not evaluated as temporary the way the gRPC path does. **Suggestion:** map backchannel, backend and user errors separately, according to the actual Nauthilus contract. I did not find a demonstrable cross-account auth bypass in the HTTP/gRPC/master user path.

28. **Key rotation can fail permanently and contains a Go data race — medium.**  
    `internal/rookcreds/rookcreds.go:157–164`: the new key is stored as processed before a successful `Rotate`; a temporary connection error is therefore not retried. A rotation between the startup fetch and the first poll can also be swallowed. In addition, `store.go:94` writes the config under a lock, while `Store.User` at `:170–184` reads without that lock. **Suggestion:** track a confirmed start/active key, retry failed switches, and reference the connection together with an immutable config.

29. **FTS returns incorrect SEARCH results — medium.**  
    `internal/imapproto/search.go:503–525`. After the candidate search, the original text criteria disappear entirely. For example, `HEADER X-Special needle` can succeed because of a hit in a different header; TO/CC/BCC also share one index field. `internal/cephstore/fts.go:368–372` additionally truncates candidates after 256 hits. **Suggestion:** use FTS only as a conservative candidate filter, re-check the original criteria exactly, and fall back to a full search path when coverage is incomplete.

30. **Permissible operations exceed the deployment's memory budget — medium.**  
    `internal/cephstore/blob.go:75–85`, `internal/imapproto/mailbox.go:423`, `internal/lmtpd/lmtpd.go:133–141`. Entire messages are buffered even when FETCH only needs a small partial; further byte/string copies increase peak usage. With a 256 MiB message limit and a 512 MiB pod limit, a few parallel valid operations suffice for OOM. Whole mailbox snapshots per session amplify this. **Suggestion:** streaming, a global weighted memory budget, and bounded or shared snapshots.

31. **FTS updates and the scrubber scale with the entire data set — medium.**  
    `internal/cephstore/fts.go:467–476`: every frequent term is read, searched and copied as a complete GUID list on every change; CAS retries repeat this work. `scrub.go:66–85` first enumerates all index objects of all namespaces just to discover users; afterwards `cmd/imapcephpoc/main.go:253` processes the accounts serially. **Suggestion:** segmented postings and bounded workers; for scrub, a user registry as well as partitioned, resumable jobs with bounded memory requirements.

The existing in-memory protocol tests do not refute the store findings: in particular, they do not model Ceph CAS, blob refcounts, shard commit ordering or the actual behavior of a POP3 lock TTL.

| Finding | Severity | File |
|---|---|---|
| 1. Foreign IOContext version in CAS | high | `internal/cephstore/fts.go:493` |
| 2. Stale flags/CONDSTORE check | high | `internal/cephstore/user.go:941` |
| 3. Publication before shard commit | high | `internal/cephstore/user.go:883` |
| 4. Multiple reference releases by EXPUNGE | high | `internal/cephstore/user.go:1027` |
| 5. COPY races with final blob deletion | high | `internal/cephstore/blob.go:146` |
| 6. Scrub deletes after incomplete scan | high | `internal/cephstore/scrub.go:162` |
| 7. Scrub overwrites concurrent changes | high | `internal/cephstore/scrub.go:209` |
| 8. MOVE recovery deletes source despite target failure | high | `internal/cephstore/scrub.go:144` |
| 9. MOVE intent removed too early | high | `internal/cephstore/user.go:1176` |
| 10. RENAME overwrites target subfolders | high | `internal/cephstore/user.go:387` |
| 11. Unreliable log/QRESYNC coverage | medium | `internal/cephstore/user.go:649`; `internal/imapproto/mailbox.go:562` |
| 12. User can switch off quota | high | `internal/imapproto/mailbox.go:674` |
| 13. COPY batch bypasses quota | medium | `internal/cephstore/user.go:1082` |
| 14. Unbounded parser input | high | `internal/imapproto/wire.go:75`; `internal/pop3proto/pop3.go:191`; `internal/sieve/managesieve.go:164` |
| 15. Endless STORE allocation loop | high | `internal/imapproto/selected.go:322` |
| 16. Unbounded metric cardinality | high | `internal/imapproto/conn.go:324`; `internal/pop3proto/pop3.go:205` |
| 17. Deferred EXPUNGEs lost | medium | `internal/imapproto/selected.go:66` |
| 18. Non-atomic MULTIAPPEND | medium | `internal/imapproto/mailbox.go:423` |
| 19. Empty/missing FETCH data with OK | medium | `internal/imapproto/fetch.go:130`; `:351` |
| 20. Write deadline expires during idle time | medium | `internal/imapproto/conn.go:270` |
| 21. POP3 lock expires during session | medium | `internal/pop3proto/pop3.go:439` |
| 22. Leaked slots and native resources | medium | `internal/imapproto/conn.go:675`; `internal/sieve/managesieve.go:437` |
| 23. Wrong or lost active Sieve script | medium | `internal/cephstore/scripts.go:38`; `:119` |
| 24. Faulty ManageSieve escapes | low | `internal/sieve/managesieve.go:188` |
| 25. LMTP allows auth despite intended TLS requirement | medium | `internal/lmtpd/lmtpd.go:50` |
| 26. Duplicates after partial Sieve delivery | medium | `internal/lmtpd/lmtpd.go:217` |
| 27. HTTP auth errors misclassified | medium | `internal/auth/nauthilus.go:263` |
| 28. Key rotation without retry; config data race | medium | `internal/rookcreds/rookcreds.go:157`; `internal/cephstore/store.go:94` |
| 29. Incorrect or missing SEARCH hits | medium | `internal/imapproto/search.go:503`; `internal/cephstore/fts.go:368` |
| 30. Memory budget exceeded by valid operations | medium | `internal/cephstore/blob.go:75`; `internal/lmtpd/lmtpd.go:133` |
| 31. FTS/scrub effort grows with total data set | medium | `internal/cephstore/fts.go:467`; `internal/cephstore/scrub.go:66` |
