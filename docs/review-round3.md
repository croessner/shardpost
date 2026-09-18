**Noch keine Freigabe für produktive Mailbestände.** Die Shard-Korrekturen beseitigen wesentliche Rennen, lassen aber Fehler bei ModSeq-Reihenfolge, Snapshot-Abgleich und Budgetreservierung bestehen.

„Behoben“ bedeutet statisch geprüft. Tests für IMAP, POP3, Auth, FTS und Sieve bestehen. Der Gesamtlauf scheitert am fehlenden `rados/librados.h`; Ceph-, Crash- und Race-Tests bleiben unbestätigt. `vendor/` wurde nicht reviewed.

| Befund | Status |
|---|---|
| R1 #1 – Fremde IOContext-Version | **Behoben:** exklusiver Versionslesekontext, `internal/cephstore/verpool.go:72`. |
| R1 #2 – Flags/CONDSTORE außerhalb CAS | **Teilweise:** aktueller Shardzustand wird erneut gelesen; neue ModSeq-/Retry-Fehler unten. `internal/cephstore/user.go:1191`. |
| R1 #3 – Veröffentlichung vor Shard-Commit | **Offen:** Pending ist weiterhin zeitbegrenzt; PendingFloor schließt das Snapshot-Rennen nicht. `internal/cephstore/user.go:764`, `internal/imapproto/mailbox.go:650`. |
| R1 #4 – Doppelte EXPUNGE-Referenzfreigabe | **Behoben für konkurrierende EXPUNGEs:** `modifyEntries` setzt die entfernten Schlüssel bei jedem Versuch zurück; nur erfolgreich entfernte Einträge werden freigegeben. `internal/cephstore/user.go:957`, `:1355`. |
| R1 #5 – COPY belebt gelöschten Blob wieder | **Behoben:** positive Referenzänderung gegen null abgelehnt. `internal/cephstore/blob.go:138`. |
| R1 #6 – Scrub nach unvollständigem Scan | **Teilweise:** Registry und Zwischenshards geprüft; fehlender **letzter** Shard bleibt ungeschützt. Löschungen erfolgen außerdem vor dem Vollständigkeitsentscheid. `internal/cephstore/scrub.go:212`, `:238`. |
| R1 #7 – Konkurrierende Scrub-Reparaturen | **Offen:** Scan und Reparatur bleiben inkonsistent; Orphan-Löschung ohne CAS. `internal/cephstore/scrub.go:277`, `:330`. |
| R1 #8/#9 – MOVE-Recovery/Intent | **Teilweise:** Zielfehler und regulärer Intent-Abschluss verbessert; GUID-Gleichheit ersetzt weiterhin konkrete Zielidentität, Recovery bleibt unzureichend idempotent. `internal/cephstore/scrub.go:172`, `internal/cephstore/user.go:1380`. |
| R1 #10 – RENAME-Kollisionen | **Behoben:** Live-Auflistung innerhalb des Registry-CAS-Versuchs. `internal/cephstore/user.go:399`. |
| R1 #11 – Log/QRESYNC-Lücken | **Teilweise:** monotones Trimming; ohne known-UIDs weiterhin nur ALERT. Pending-EXPUNGEs stehen bereits im Vanished-Log. `internal/imapproto/mailbox.go:586`, `internal/cephstore/user.go:638`. |
| R1 #12 – Eigene Quota abschalten | **Behoben im Standardbetrieb:** explizite Freischaltung und Werteprüfung. `internal/imapproto/mailbox.go:739`. |
| R1 #13 – COPY-Quota | **Teilweise:** Batchsumme berücksichtigt, aber keine atomare Reservierung zwischen parallelen Zustellungen. `internal/cephstore/user.go:1399`. |
| R1 #14 – Parser-Ressourcen | **Teilweise:** Literalbytes summiert, Argumentzahl weiterhin unbegrenzt. Beliebig viele `{0+}`-Fortsetzungen verbrauchen Token-/Callback-Speicher bei null Budgetbytes. `internal/imapproto/wire.go:183`, `internal/imapproto/conn.go:294`. |
| R1 #15/#16 – STORE-Endlosschleife/Metrik-Kardinalität | **Behoben:** `internal/imapproto/selected.go:351`, `internal/imapproto/conn.go:202`, `internal/pop3proto/pop3.go:202`. |
| R1 #17 – Zurückgestellte EXPUNGEs | **Behoben im beschriebenen Reload-Pfad:** alter Wasserstand bleibt bei zurückgestellten Entfernungen erhalten. `internal/imapproto/selected.go:175`. |
| R1 #18 – MULTIAPPEND-Atomarität | **Teilweise:** Rollback bleibt bestmöglich, ohne Crash-Recovery. `internal/imapproto/mailbox.go:438`. |
| R1 #19 – Falscher FETCH-Erfolg | **Behoben:** Makroexpansion und Fehlerweitergabe. `internal/imapproto/fetch.go:144`, `:374`. |
| R1 #20 – WriteDeadline | **Teilweise:** vor Kommandobearbeitung erneuert; automatische Writer-Ausgaben bleiben ohne eigene Erneuerung. `internal/imapproto/conn.go:367`, `internal/imapproto/wire.go:331`. |
| R1 #21 – POP3-Leaseverlust | **Teilweise:** erkannter Verlust sperrt Transaktionskommandos; QUIT überspringt dann jedoch Löschungen und antwortet trotzdem positiv. `internal/pop3proto/pop3.go:326`, `:612`. |
| R1 #22/#24 – Session-Ressourcen/ManageSieve-Escapes | **Behoben:** `internal/imapproto/conn.go:744`, `internal/sieve/managesieve.go:330`, `:237`. |
| R1 #23 – Sieve-Aktivierung/Rename | **Teilweise:** Rename-Inhalt und Aktivstatus bleiben aus ungeschützten Vorab-Reads; Aktivierung separat. `internal/cephstore/scripts.go:129`, `:153`. |
| R1 #25/#27 – LMTP-TLS-Vorgabe/HTTP-Authfehler | **Behoben:** `internal/lmtpd/lmtpd.go:46`, `internal/auth/nauthilus.go:269`. |
| R1 #26 – Sieve-Teilzustellung | **Teilweise:** Rollback ohne dauerhaften Zustellintent. `internal/lmtpd/lmtpd.go:269`. |
| R1 #28 – Rotation/Config-Race | **Die beschriebenen Pfade behoben:** bestätigter Key und gesperrter ListUsers-Config-Read. `internal/rookcreds/rookcreds.go:147`, `internal/cephstore/scrub.go:61`. |
| R1 #29 – SEARCH-Korrektheit | **Teilweise:** Textkriterien werden weiterhin entfernt; Kandidatengrenze jetzt 4096 statt 256, weiterhin ohne Vollständigkeitsnachweis. `internal/imapproto/search.go:522`, `internal/cephstore/fts.go:371`. |
| R1 #30 – Speicherbudget | **Teilweise:** Pufferlebensdauer besser erfasst; Kopien, FETCH-Ausgabepuffer, POP3-Zeilenarrays und Snapshots fehlen weiterhin. `internal/imapproto/fetch.go:415`, `:429`, `internal/pop3proto/pop3.go:573`. |
| R1 #31 – FTS-/Scrub-Skalierung | **Weiter offen:** vollständige Postinglisten und Namespace-Enumeration. `internal/cephstore/fts.go:467`, `internal/cephstore/scrub.go:75`. |
| R2 neu #1 – Blob-Löschung nach Commitfehler | **Behoben:** erfolgreicher Shardwrite bleibt trotz Commitfehler erfolgreich. `internal/cephstore/user.go:1074`. |
| R2 neu #2 – Pending serialisiert keine Shardänderungen | **Teilweise:** Flag-RMW und Löschentscheidung jetzt versionsgebunden; ModSeq-Reihenfolge bleibt falsch, siehe unten. |
| R2 neu #3 – Poll hängt an fehlender UID | **Teilweise:** konkrete Blockade beseitigt; langsame Writer werden nach 30 Sekunden weiterhin übergangen. `internal/imapproto/selected.go:73`, `internal/cephstore/user.go:764`. |
| R2 neu #4 – MULTIAPPEND-Speicher-DoS | **Teilweise:** Byte-Summe begrenzt; Null-Literale und Budgetstillstand bleiben. `internal/imapproto/conn.go:285`. |
| R2 neu #5 – Automatische Buffer-Flushes | **Teilweise**, siehe R1 #20. |
| R2 neu #6 – Scriptnamenprüfung nach Mutation | **Behoben:** Prüfung vor Reads und Mutation. `internal/cephstore/scripts.go:126`. |
| R2 neu #7 – Header-Unfolding/verschluckte Lesefehler | **Beschriebene Fälle behoben:** Unfolding und Fehlerweitergabe an SEARCH. `internal/imapproto/search.go:499`, `:674`. |

**Neue Befunde**

1. **Hoch – Budgetreservierungen können dauerhaft gegenseitig blockieren.**  
   `internal/membudget/membudget.go:69`, `internal/imapproto/conn.go:289`, `internal/lmtpd/lmtpd.go:148`. Zwei Kommandos halten bereits Teilreservierungen und warten auf weitere Bytes; freigegeben wird erst nach Kommandoende. Selbst eine einzelne LMTP-Nachricht mit exakt 256 MiB blockiert beim zusätzlichen EOF-Leseversuch am standardmäßigen 256-MiB-Budget. Socket-Timeouts unterbrechen `cond.Wait()` nicht. Erweiterungen müssen nichtblockierend scheitern oder vorab vollständig reserviert werden.

2. **Hoch – STORE kann die Nachrichten-ModSeq zurücksetzen.**  
   `internal/cephstore/user.go:625`, `:1209`. A reserviert ModSeq 10, B reserviert 11 und schreibt zuerst. A liest danach korrekt Bs Flags, schreibt seine Änderung aber mit **10**. Der Shard-CAS schützt den Inhalt, nicht die vorab reservierte Reihenfolge. CONDSTORE und CHANGEDSINCE werden dadurch falsch. ModSeq-Vergabe und tatsächliche Schreibreihenfolge müssen zusammen abgesichert werden.

3. **Hoch – PendingFloor übersieht einen Commit zwischen Snapshot und Nachprüfung.**  
   `internal/imapproto/mailbox.go:642`, `:646`, `:650`; `internal/cephstore/user.go:677`, `:789`. Header enthält Pending-ModSeq 10, Snapshot enthält dessen Nachricht noch nicht; anschließend schreibt und committet der Writer. PendingFloor findet jetzt nichts mehr. Die Session bleibt auf 10 und überspringt die Nachricht dauerhaft. Zusätzlich ignoriert die Suche Pending-Einträge außerhalb der letzten 512 ModSeqs sowie Abfragefehler. Erforderlich ist eine validierte Snapshot-Grenze über alle beteiligten Reads.

4. **Mittel – Fehlgeschlagene CAS-Versuche gelangen ins STORE-Ergebnis.**  
   `internal/cephstore/user.go:1191`, `:1201`, `:1211`; Wiederholung in `internal/cephstore/fts.go:530`. Der Callback hängt `Updated`, `Modified` und Keywords bei jedem Versuch an. Nach einem Konflikt werden diese Ergebnisse nicht verworfen. Beispielsweise bleibt eine vermeintliche Änderung in `Updated`, obwohl der erfolgreiche Wiederholungsversuch die inzwischen gelöschte UID überspringt. Ergebnisse pro Versuch isolieren und erst nach erfolgreichem CAS übernehmen.

5. **Hoch – MOVE konkurriert ungeschützt mit EXPUNGE um die letzte Blob-Referenz.**  
   `internal/cephstore/user.go:1469`, `:1475`, `:1277`. MOVE legt das Ziel ohne zusätzliche Referenz an. EXPUNGE kann zwischen Ziel-Link und Quell-Unlink die Quelle entfernen und den einzigen Refcount auf null setzen. Der Zielindex bleibt ohne Blob zurück; der Intent verhindert das nicht. Auch zwei parallele MOVEs können mehrere Zielreferenzen bei unverändertem Refcount erzeugen.