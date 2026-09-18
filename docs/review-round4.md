**Neue Datenverlustrisiken in der Intent-Recovery; deutliche Speicherverstärkung bei der FTS-Migration.** Befunde statisch geprüft, `vendor/` nicht reviewed.

1. **Hoch – Bestätigte Zustellungen können nachträglich gelöscht werden.**  
   [mailbox.go:465](/Users/croessner/GoLandProjects/imapcephpoc/internal/imapproto/mailbox.go:465), `internal/lmtpd/lmtpd.go:282`: Scheitert `EndDelivery`, folgt trotzdem APPEND-OK beziehungsweise LMTP-Erfolg. Der verbliebene Intent wird später durch `scrub.go:147` zurückgerollt. **Abhilfe:** dauerhaften Commit-Zustand vor der Erfolgsantwort verlangen.

2. **Hoch – Intent-Rollback kann falsche Nachrichten entfernen.**  
   [scrub.go:147](/Users/croessner/GoLandProjects/imapcephpoc/internal/cephstore/scrub.go:147), `internal/cephstore/user.go:1625`, `:1421`: Gespeichert werden Mailboxname und Meta; gelöscht wird anhand der UID ohne GUID-Prüfung. Nach DELETE/CREATE desselben Mailboxnamens kann die UID eine andere Nachricht bezeichnen. RENAME lässt umgekehrt den Rollback ins Leere laufen. **Abhilfe:** Mailbox-ID und konkrete Nachrichtenidentität beim Löschen atomar prüfen.

3. **Mittel – Zustell-Intents schließen das Crash-Fenster nicht.**  
   [mailbox.go:448](/Users/croessner/GoLandProjects/imapcephpoc/internal/imapproto/mailbox.go:448), `:461`, `internal/lmtpd/lmtpd.go:270`, `:277`: Erst wird zugestellt, danach protokolliert; ein Crash dazwischen hinterlässt nicht erfasste Teilzustellungen. Zusätzlich wird der Intent selbst nach fehlgeschlagenem Rollback entfernt (`mailbox.go:455`, `lmtpd.go:273`). **Abhilfe:** Zielschritte vorab dauerhaft identifizieren und Intents bis zum bestätigten Abschluss behalten.

4. **Hoch – Legacy-Konvertierung vervielfacht Speicher und Doc-ID-Zuordnungen.**  
   [fts.go:163](/Users/croessner/GoLandProjects/imapcephpoc/internal/cephstore/fts.go:163), `:279`, `:295`: `need` enthält dieselbe GUID für jeden gemeinsamen Term erneut. `ensureDocIDs` prüft ausschließlich `cur`, vergibt deshalb innerhalb eines Versuchs mehrfach IDs für dieselbe fehlende GUID und hinterlässt überzählige `i:<id>`-Einträge. Dazu kommen vollständige Postinglisten, Hexstrings, Schlüssel und Maps über parallele Shard-Worker, ohne Speicherbudget. **Abhilfe:** vor Sammlung und Vergabe deduplizieren, Migration portionieren und Parallelität begrenzen. Das passt zum einmaligen OOM bei anschließenden 230 MiB RSS; ohne Heap-/RSS-Profil ist die konkrete OOM-Ursache nicht bewiesen.

5. **Hoch – QRESYNC-Fallback benötigt Speicher proportional zu UIDNEXT.**  
   [mailbox.go:613](/Users/croessner/GoLandProjects/imapcephpoc/internal/imapproto/mailbox.go:613), `:620`, `internal/imapproto/numset.go:108`: Ohne known-UIDs werden sämtliche historischen UIDs materialisiert, fehlende nochmals gesammelt und für `formatSet` kopiert. Auch eine inzwischen leere Mailbox kann dadurch einen Pod überfordern. **Abhilfe:** VANISHED direkt als komprimierte Lückenintervalle aus den vorhandenen UIDs erzeugen.

6. **Hoch – Neue mbox-Löschung überspringt bei Stat-Fehlern die Karenzprüfung.**  
   [scrub.go:432](/Users/croessner/GoLandProjects/imapcephpoc/internal/cephstore/scrub.go:432): Jeder `Stat`-Fehler führt weiter zu `Delete`. Eine nach dem Registry-Snapshot angelegte Mailbox kann somit trotz laufender Nutzung gelöscht werden, wenn ihre Altersprüfung fehlschlägt. **Abhilfe:** bei Lesefehlern nicht löschen; Registrierung und Objektversion vor Löschung erneut absichern.

7. **Mittel – Kurzlebige OIDC-Tokens werden bei jedem Aufruf erneuert.**  
   [oidc.go:60](/Users/croessner/GoLandProjects/imapcephpoc/internal/auth/oidc.go:60): Bei `expires_in <= 60` liegt jedes frisch geholte Token bereits im festen Refresh-Fenster. Jeder wartende Aufrufer führt deshalb unter dem Mutex einen weiteren Tokenrequest aus. **Abhilfe:** Refresh-Vorlauf relativ zur tatsächlichen Lebensdauer begrenzen.

**Abgleich der gezielt nachgeprüften Punkte aus Runde 3**

| Punkt | Status jetzt |
|---|---|
| Neu #1 – dauerhafte Budgetblockade | **Behoben:** begrenztes Warten (`membudget.go:82`); zusätzlicher LMTP-EOF-Schritt korrigiert (`lmtpd.go:149`). |
| Neu #2 – STORE-ModSeq rückläufig | **Teilweise:** `max` verhindert Rückwärtsbewegung (`user.go:1291`). Zwei verschiedene Flagzustände können jedoch dieselbe ModSeq erhalten; `CHANGEDSINCE` übersieht dann die spätere Änderung. |
| Neu #3 – PendingFloor nach Snapshot / 512er-Fenster | **Beschriebene Fehler behoben:** Prüfung vor Snapshot (`mailbox.go:675`), Sondierung statt Fenster (`user.go:840`), Fehlerweitergabe. |
| Neu #4 – STORE-Ergebnis enthält verworfene CAS-Versuche | **Behoben:** Ergebnis pro Versuch zurückgesetzt (`user.go:1269`). |
| Neu #5 – MOVE ohne eigene Blob-Referenz | **Behoben im regulären Pfad:** Referenz vor Ziel-Link, Freigabe tatsächlich entfernter Quellen (`user.go:1578`, `:1595`). |
| R1 #13 – parallele Quota-Überschreitung | **Reservierung behoben:** `reserveQuota` in Deliver/Copy. Scrub kann Reservierungen weiterhin überschreiben (`scrub.go:455`). |
| R1 #18/#26 – MULTIAPPEND/Sieve-Teilzustellung | **Nicht vollständig behoben:** siehe neue Intent-Befunde. |
| R1 #3/#11 – Pending/QRESYNC | **Teilweise:** Antwort ohne known-UIDs ergänzt. Pending verfällt weiterhin nach 30 Sekunden; VANISHED wird weiterhin vor Shard-Commit geschrieben (`user.go:793`, `:665`). |
| R1 #6/#7 – destruktiver Scrub | **Weiter offen:** fehlender letzter Shard bleibt ungeschützt; zweistufige Waisenprüfung endet weiterhin ohne atomare Löschabsicherung (`scrub.go:263`, `:406`). |
| R1 #8/#9 – MOVE-Recovery | **Teilweise:** eigene Referenz ergänzt; irgendein GUID-Treffer im Ziel ersetzt weiterhin die konkrete MOVE-Zielidentität (`scrub.go:211`). |
| R1 #31 – FTS-Skalierung | **Teilweise:** kompaktere Postings; vollständige Listen und neue Migrationsverstärkung bleiben. |

Vorhandene Tests für Auth, Postings, Membudget, IMAP, POP3, FTS und Sieve bestehen aus dem Testcache. Nauthilus-Kustomize-Overlay erfolgreich gerendert. Ceph-/LMTP-/Server-Tests scheitern am fehlenden `rados/librados.h`; Crash-Verhalten und Lastspitzen bleiben laufzeitseitig unbestätigt.