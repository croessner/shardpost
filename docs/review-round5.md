## New findings – round 5

1. **High – Redirect confirms message loss.**  
   `internal/lmtpd/lmtpd.go:354`: with `Auto-Submitted != no`, success is returned without sending. A pure redirect script has cancelled the implicit keep: neither storage nor forwarding, yet LMTP success. The same applies to self-targets that are filtered out entirely (`:372`).

2. **High – CATENATE bypasses the command memory budget.**  
   `internal/imapproto/mailbox.go:646`, `:655`, `:1128`: URL results are collected completely in `parts`, without `cmdBytes` or a durable budget reservation. The size limit applies per message; many CATENATE messages in one MULTIAPPEND can exhaust the process memory.

3. **Medium – A replacing STORE removes protected flags.**  
   `internal/imapproto/selected.go:367`, `:384`: only explicitly given flags are checked. With just `rs`, `STORE 1 FLAGS ()` also removes existing `\Deleted` flags and keywords without `t/w`.

4. **Medium – APPEND/COPY adopt disallowed target flags.**  
   `internal/imapproto/mailbox.go:701`, `internal/imapproto/selected.go:537`: after checking `i`, flags are stored unfiltered. Plain insert rights are therefore enough for `\Deleted`, `\Seen` and keywords; missing `t/s/w` rights must be taken into account. [RFC 4314 §4](https://www.rfc-editor.org/rfc/rfc4314.html#section-4)

5. **Medium – The existence of other users' mailboxes is detectable without rights.**  
   `internal/imapproto/selected.go:491`, `internal/imapproto/mailbox.go:596`: COPY/MOVE/APPEND return `TRYCREATE` for a missing target and `NONEXISTENT` for an existing target the user is not entitled to. CREATE makes a corresponding distinction based on existing parents (`mailbox.go:361`).

6. **Medium – Cross-namespace COPY leaves unconfirmed partial copies behind.**  
   `internal/imapproto/selected.go:537`: if message 2 fails, for instance on the target quota, message 1 remains stored; the command ends with NO and without COPYUID. A retry creates duplicates. With MOVE, the sources additionally remain.

7. **Medium – A SETACL/DELETEACL race loses valid LIST references.**  
   `internal/cephstore/acl.go:104`: after rights are withdrawn, a parallel SETACL can restore the rights and the reference before the older DELETEACL then removes the reference. The valid share disappears from LIST permanently.

8. **Medium – An accepted `/private` is stored shared.**  
   `internal/imapproto/metadata.go:23`, `internal/cephstore/metadata.go:67`: `/private` is accepted as an entry, but only `/private/…` is stored privately. A supposedly private value set there is visible to other entitled users; `/shared` analogously bypasses the intended `w` check.

9. **Medium – NOTIFY swaps the reader during parallel network access.**  
   `internal/imapproto/notify.go:432`: `fetchAttributes` replaces `c.r` while the command loop, respectively IDLE, reads from it without that lock (`conn.go:341`, `:865`). The network reader can catch the synthetic reader: race, misparsing or connection drop.

10. **Medium – IDLE DONE writes outside of `cmdMu`.**  
    `internal/imapproto/conn.go:904`: DONE processing writes OK/BAD and changes reader state before the defer reacquires the lock. Concurrent NOTIFY output can corrupt responses in the shared writer.

11. **Medium – NOTIFY loses requested MessageNew FETCH attributes.**  
    `internal/imapproto/notify.go:406`: if the additional IDLE poll or a NOOP poll wins, `c.msgs` already contains the new message. NOTIFY then no longer detects a new UID and does not deliver its requested FETCH attributes.

12. **Medium – Vacation also runs after a failed store.**  
    `internal/lmtpd/lmtpd.go:265`: the unconditional `defer` sends the out-of-office reply even after quota/storage errors and a rollback. The original message is rejected, but the reply period is consumed anyway.

13. **Medium – A QUIT error duplicates already confirmed forwards.**  
    `internal/outbound/outbound.go:109`: after a positively confirmed DATA, a QUIT error is treated as a send error. LMTP answers 451; the delivery retry sends the already accepted message again.

14. **Medium – A failed vacation send consumes the suppression period.**  
    `internal/lmtpd/lmtpd.go:394`: `VacationSeen` is persisted before `Relay.Send`. A temporary relay/TLS error therefore suppresses further replies for the entire period, seven days by default.

15. **Medium – CATENATE rejects the relative URLs it is supposed to accept.**  
    `internal/imapproto/mailbox.go:1137`: only `imap:///…` is accepted. Valid relative URLs such as `/INBOX/;UID=1` fail with BADURL; those are exactly what [RFC 4469 §3](https://www.rfc-editor.org/rfc/rfc4469.html#section-3) requires.

16. **Low – SELECT reports contradictory write rights.**  
    `internal/imapproto/mailbox.go:811`: READ-ONLY depends on `examine` instead of `c.readOnly`; a SELECT with just `lr` answers READ-WRITE. With restricted flag rights, all PERMANENTFLAGS are additionally offered (`:801`).

17. **Low – MYRIGHTS refuses permissible queries.**  
    `internal/imapproto/acl.go:321`: only `l/r` permit the response. The rights `i/k/x/a` on their own must also allow this query. [RFC 4314 §4](https://www.rfc-editor.org/rfc/rfc4314.html#section-4)

18. **Low – Mutual shares can block memstore.**  
    `internal/memstore/memstore.go:145`: DELETE/RENAME hold the owner mutex and then lock the entitled user. Concurrent operations on mutually shared folders enable Alice→Bob/Bob→Alice deadlocks.

**Validation:** findings 2–5, 8 and 15 reproduced through six temporary protocol tests. The existing IMAP/Sieve/outbound tests as well as the NOTIFY race tests passed; the latter do not cover the race windows mentioned. Ceph/LMTP/server tests fail on the missing `rados/librados.h`. Project files unchanged.
