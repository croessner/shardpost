# Review-Runde 6 (15.09.2026, Codex gpt-6-astra, Stand 0.8.6 → Fixes in 0.8.7)

Schwerpunkt: die seit Runde 5 neuen Teile (ACL/METADATA/NOTIFY/CATENATE,
\Recent-Wasserzeichen, Adressparser, Sieve-Erweiterungen, Redirect/Vacation).
21 Befunde, alle in 0.8.7 behandelt; die Umsetzung steht jeweils unter
"Fix". Der Originalbericht folgt unten.

| # | Befund | Fix in 0.8.7 |
|---|---|---|
| 1 | Sieve `duplicate` schreibt den Merker während des Laufs | Test ist nur noch eine Abfrage (`Hooks.Duplicate(key)`), Antwort pro Schlüssel im Lauf gecacht; `Delivery.Tracked` wird vom LMTP-Server erst nach dem endgültigen Ergebnis (Speichern oder Discard) geschrieben |
| 2 | `stop` überspringt den impliziten Keep | Fork: `ErrStop` bricht die Schleife ab, der Abschluss (impliziter Keep) läuft wie am Skriptende; Test `TestStopKeepsImplicitKeep` |
| 3 | FETCH/CATENATE-Teilbereiche können abstürzen | negative Offsets/Längen sind BAD, `off+size` wird vorab gegen MaxInt64 begrenzt; CATENATE begrenzt per Subtraktion; Test `TestPartialRangesRejectedNotCrashing` |
| 4 | Paralleles DELETE gibt Blobs doppelt frei | Registry-CAS prüft, dass der Eintrag noch dieselbe Mailbox-ID trägt; nur der Gewinner gibt Referenzen frei, Abo/ACL kommen aus seinem Snapshot, der Verlierer bekommt `ErrNoMailbox` |
| 5 | COPY/MOVE im fremden Namespace ohne Flag-Rechte | `flagsForTarget` wendet `allowedFlags` auch an, wenn Quelle und Ziel im selben (fremden) Namespace liegen |
| 6 | Ersetzendes STORE prüft gegen veralteten Snapshot | `replaceFlags(rights, flags)`: die Mutation läuft im Store gegen den aktuellen Flagbestand und erhält Flags ohne Recht; Test `TestReplaceFlagsKeepsProtectedFlags` |
| 7 | Vacation `:from` Header-Injection | Wert wird als eine Adresse geparst (`net/mail`), CR/LF abgewiesen, sonst Fallback auf den Empfänger; Test `TestVacationFromInjection` |
| 8 | Redirect vor dem dauerhaften Speichern | Mit lokaler Kopie wird zuerst gespeichert, dann weitergeleitet; ein Relay-Fehler verliert nur die Weiterleitung (geloggt), nie die Mail, und ein LMTP-Retry sendet nicht doppelt. Reiner Redirect ohne Kopie sendet weiterhin sofort (nichts gespeichert). Eine Outbox bleibt offen (siehe unten) |
| 9 | Redirect-Loops über Konten | Header `X-Sieve-Redirected-From: <konto>` bei jeder Weiterleitung; ist das eigene Konto darin oder gibt es > 25 Received-Header, wird nicht weitergeleitet |
| 10 | Vacation-Sperrfrist nicht atomar | Reservierung per CAS vor dem Versand (`VacationSeen(key, period)`), Freigabe bei Versandfehler (`ttl == 0` löscht den Eintrag, neue Semantik in `model.User`) |
| 11 | LMTP-Reset behält `rcptRes` | `Reset` leert auch `rcptRes` |
| 12 | Shared-Pointer-Race SETACL/DELETEACL | `MailboxRec.ACLGen` zählt ACL-Änderungen; Pointer tragen die Generation, Widerruf hinterlässt einen Tombstone mit Generation; ein späterer Write älterer Generation wird ignoriert; `ListShared` blendet Tombstones aus |
| 13 | LIST löscht Pointer bei temporären Fehlern | Pointer wird nur entfernt, wenn die Mailbox fehlt (`ErrNoMailbox`), und auch dann nur, wenn seine Generation nicht neuer ist (`RemoveSharedRef(ref)`); sonst nur ausgeblendet |
| 14 | NOTIFY vs. Writer außerhalb `cmdMu` | die Fehlerpfade (Zeile zu lang, Lesefehler) nehmen `cmdMu` für Antwort und Flush |
| 15 | REPLACE ohne `drain` bei früher Ablehnung | alle frühen Ablehnungen konsumieren die Zeile samt angekündigtem Literal |
| 16 | EXAMINE verbraucht \Recent per Poll | `c.examine` verhindert `claimRecent`; Test `TestExamineDoesNotClaimRecent` |
| 17 | MOVE verliert konkurrierende Änderungen | `announceExpunge` springt nicht mehr im Change-Log vor, sondern ruft `poll(true)`; eigene Expunges sind nicht mehr im Snapshot und drucken nichts, fremde Zustellungen und die Kopien eines MOVE in dieselbe Mailbox werden regulär angekündigt |
| 18 | TO/CC/BCC nicht unterschieden | die Suchschlüssel behalten den Headernamen (`key`), der FTS-Index dient nur der Kandidatenauswahl, geprüft wird der jeweilige Header bzw. die geparste Adressliste; Test `TestSearchToCcBccDistinct` |
| 19 | Adressparser entfernt Leerzeichen aus quoted Localparts | Wörter werden für den Localpart ohne Trennzeichen konkateniert (quoted strings behalten ihre Zeichen), für Anzeigenamen mit Leerzeichen |
| 20 | Verschachteltes MODSEQ ohne Rückgabe | `hasModseq` prüft den Kriterienbaum; Test `TestNestedModseqInSearchResponse` |
| 21 | memstore RENAME/DELETE geben ACL-Map ungesperrt weiter | `copyACL`-Snapshots unter dem Mutex |

Beim Verifizieren von 0.8.7 zusätzlich gefunden und in 0.8.8 behoben:
ManageSieve `SETACTIVE ""` (deaktivieren) schrieb ein leeres xattr, worauf
go-ceph mit einem Index-Panic reagiert und der ganze Prozess starb
(`SetActiveScript` entfernt das xattr jetzt). Die Verbindungs-Goroutinen
von IMAP, ManageSieve und LMTP fangen Panics seither ab (Log mit Stack,
nur die betroffene Verbindung bzw. Transaktion endet; LMTP antwortet 451).

Offen geblieben (bewusst): eine persistente Outbox für Redirect/Vacation
(Befund 8) ist Architekturarbeit für den Regelbetrieb; bis dahin gilt:
lokale Kopie zuerst, Weiterleitung danach, Fehler geloggt.

---

## Originalbericht

# Review-Runde 6

**Der Stand enthält weiterhin Datenverlustrisiken und einen durch IMAP-Eingaben erreichbaren Prozessabsturz.** Mehrere Korrekturen aus Runde 5 greifen nur in Teilpfaden.

**Prüfgrundlage:** ausschließlich lesende Quellcodeprüfung einschließlich aufgerufener Store-/Interpreterpfade und RFC-Abgleich. Keine Dateien geändert, keine Tests ausgeführt. Mangels Git-Metadaten lässt sich das Einführungsdatum einzelner Fehler nicht sicher bestimmen.

## Hohe Schwere

### 1. Sieve `duplicate` kann die einzige Zustellung verwerfen

**Fundstellen:** `internal/lmtpd/lmtpd.go:277`, `third_party/go-sieve/interp/extensions.go:229`, `internal/cephstore/scripts.go:161`.

Der Test persistiert den Schlüssel unmittelbar während der Script-Ausführung. Scheitert danach die Speicherung temporär, erkennt der nächste Zustellversuch die Nachricht bereits als Duplikat. Bei `if duplicate { discard; }` folgt LMTP-Erfolg ohne gespeicherte Nachricht. Auch zwei identische Tests innerhalb derselben Ausführung können unterschiedliche Ergebnisse liefern.

**Fix:** Ergebnisse innerhalb einer Ausführung zwischenspeichern; endgültige Tracking-Einträge erst nach erfolgreichem Zustellabschluss schreiben. Fehlgeschlagene Versuche dürfen keine positiven Duplikattreffer erzeugen. [RFC 7352 §3](https://www.rfc-editor.org/rfc/rfc7352.html#section-3)

### 2. `stop` überspringt den impliziten Keep

**Fundstellen:** `third_party/go-sieve/interp/script.go:87`, `internal/lmtpd/lmtpd.go:310`.

`ErrStop` führt sofort zu `return nil`, bevor der implizite Keep erzeugt wird. Schon das gültige Script `stop;` hinterlässt keine Zustellaktion und wird vom LMTP-Pfad als Discard erfolgreich bestätigt. Ebenso betroffen: `vacation "..."; stop;`.

**Fix:** Bei `ErrStop` die Ausführungsschleife verlassen und anschließend dieselbe Abschlusslogik wie am normalen Script-Ende ausführen. [RFC 5228 §3.3](https://www.rfc-editor.org/rfc/rfc5228.html#section-3.3)

### 3. FETCH-/CATENATE-Teilbereiche können den Serverprozess abstürzen lassen

**Fundstellen:** `internal/imapproto/fetch.go:292`, `internal/imapproto/mailbox.go:1392`; weitergereicht an `vendor/github.com/emersion/go-imap/v2/imapserver/message.go:159`.

FETCH akzeptiert negative Offsets und Größen; beispielsweise erreicht `BODY[]<-1.1>` einen ungültigen Slice-Zugriff. CATENATE weist negative Werte zurück, prüft aber den Überlauf von `off + ln` nicht: `PARTIAL=1.9223372036854775807` erzeugt einen negativen Endindex. Der IMAP-Goroutine fehlt eine Panic-Abfangstelle; betroffen ist der gesamte Prozess.

**Fix:** Protokollgültige Zahlenbereiche vorab prüfen und Teilbereiche durch Subtraktion begrenzen, bevor addiert oder gesliced wird.

### 4. Paralleles DELETE kann Blobs einer anderen Mailbox löschen

**Fundstellen:** `internal/cephstore/user.go:361`, `internal/cephstore/user.go:373`.

Der Registry-CAS ignoriert den aktuell gelesenen Eintrag. Zwei DELETE-Aufrufe können dieselben Metadaten vorab lesen und anschließend beide die Blob-Referenzen freigeben. Existiert eine COPY desselben Blobs in einer anderen Mailbox, fällt dessen Refcount dadurch fälschlich auf null.

Ein verspäteter DELETE kann außerdem einen inzwischen neu angelegten gleichnamigen Registry-Eintrag entfernen. Die neue Abo-Erhaltung verwendet ebenfalls den veralteten Snapshot.

**Fix:** Im CAS Mailbox-ID und aktuellen Registry-Zustand prüfen; genau einen Löschgewinner bestimmen. Nur dieser darf Referenzen freigeben. Subscription und ACL aus dessen erfolgreichem CAS-Snapshot übernehmen.

## Mittlere Schwere

### 5. COPY innerhalb eines fremden Namespace umgeht weiterhin Flagrechte

**Fundstelle:** `internal/imapproto/selected.go:572`.

`allowedFlags` wird nur in `copyAcross` angewendet. Selektiert Bob einen Ordner von Alice und kopiert in einen anderen Alice-Ordner, gilt `dt.u == c.mu`: `\Deleted`, `\Seen` und Keywords werden ungefiltert übernommen, obwohl Bob am Ziel nur `i` besitzt. **Runde 5, Befund 4 bleibt teilweise offen.**

**Fix:** Zielflagrechte unabhängig von der Namespace-Verzweigung anwenden. [RFC 4314 §4](https://www.rfc-editor.org/rfc/rfc4314.html#section-4)

### 6. Ersetzendes STORE prüft Flagrechte gegen einen veralteten Snapshot

**Fundstellen:** `internal/imapproto/selected.go:411`, `internal/imapproto/selected.go:440`.

Die neue Rechteprüfung untersucht `c.msgs`; der spätere Store-CAS ersetzt dagegen die dann aktuellen Flags vollständig. Setzt Alice zwischen Prüfung und CAS `\Deleted`, kann Bob mit ausschließlich `rs` dieses Flag durch `STORE ... FLAGS (\Seen)` entfernen.

**Fix:** Die erlaubten Änderungen innerhalb der Store-Mutation gegen deren aktuellen Flagbestand prüfen beziehungsweise geschützte Flags erhalten.

### 7. Vacation erlaubt Header-Injection über `:from`

**Fundstellen:** `third_party/go-sieve/interp/vacation.go:104`, `internal/sieve/vacation.go:125`.

Der expandierte `:from`-Wert wird ohne Adress- oder CR/LF-Prüfung direkt hinter `From:` geschrieben. Ein mehrzeiliger Sieve-String kann zusätzliche Header oder einen vorzeitigen Headerabschluss einschleusen. Das erweitert nicht automatisch die SMTP-Empfängerliste, beschädigt aber die erzeugte Nachricht.

**Fix:** Nach Variablenexpansion Steuerzeichen zurückweisen, den Wert als gültige Adressliste parsen und sicher serialisieren.

### 8. Redirect erfolgt weiterhin vor dauerhaftem Speichern

**Fundstellen:** `internal/lmtpd/lmtpd.go:295`, `internal/lmtpd/lmtpd.go:350`.

Bei `redirect` zusammen mit `keep` oder `fileinto` wird zuerst extern versendet. Scheitert anschließend die lokale Speicherung temporär, liefert LMTP einen Fehler; der Wiederholungsversuch versendet erneut. Die nachgelagerte Vacation-Korrektur löst diesen Redirect-Pfad nicht.

**Fix:** Versandaufträge dauerhaft zusammen mit dem Zustellabschluss protokollieren und anschließend über eine Outbox abarbeiten. Bloßes Vertauschen der beiden Operationen verschiebt das Wiederholungsproblem.

### 9. Redirect-Loops über mehrere Konten werden nicht erkannt

**Fundstelle:** `internal/lmtpd/lmtpd.go:405`.

Die Zielmenge kennt nur den aktuellen Empfänger und die Ziele dieser einen Ausführung. Bei A→B→A greift weder der Selbstvergleich noch `Auto-Submitted`, da Redirect dieses Feld nicht setzt. Auch `maxRedirects` begrenzt nur die Verzweigung, nicht die Anzahl der Umläufe. Ein Abbruch hängt vollständig von einem externen Relay-Hoplimit ab.

**Fix:** Einen persistent mitgeführten Loop-Nachweis oder ein geprüftes Received-Hoplimit implementieren und erkannte Schleifen abbrechen. [RFC 5228 §4.2](https://www.rfc-editor.org/rfc/rfc5228.html#section-4.2)

### 10. Vacation-Sperrfrist ist nach der Korrektur nicht mehr atomar

**Fundstellen:** `internal/lmtpd/lmtpd.go:444`, `internal/lmtpd/lmtpd.go:452`, `internal/lmtpd/lmtpd.go:456`.

Zwei parallele Zustellungen lesen beide „noch nicht beantwortet“, senden beide und tragen erst danach die Sperrfrist ein. Der CAS beim abschließenden Eintrag verhindert die bereits versandte zweite Antwort nicht. **Folgefehler der Korrektur zu Runde 5, Befund 14.**

**Fix:** Einen befristeten Versandanspruch atomar reservieren; nach erfolgreichem Versand bestätigen, bei eindeutigem Versandfehler freigeben. [RFC 5230 §8](https://www.rfc-editor.org/rfc/rfc5230.html#section-8)

### 11. LMTP-Reset behält die Auth-Ergebnisse vorheriger Empfänger

**Fundstellen:** `internal/lmtpd/lmtpd.go:123`, `internal/lmtpd/lmtpd.go:208`.

`Reset` leert `rcpt` und `rcptAddr`, aber nicht `rcptRes`. In der nächsten Transaktion wird deshalb das erste alte Auth-Ergebnis an den neuen Empfänger übergeben. `provision.Apply` kann beispielsweise dessen Quota mit der Quota eines vorherigen Kontos überschreiben. Zusätzlich wächst die Liste über die Verbindungslaufzeit.

**Fix:** `rcptRes` bei jedem Transaktionsreset ebenfalls leeren.

### 12. Shared-Pointer-Race bleibt trotz erneutem ACL-Lesen bestehen

**Fundstelle:** `internal/cephstore/acl.go:108`.

Möglicher Ablauf: DELETEACL liest „keine Rechte“; SETACL stellt Rechte und Pointer wieder her; der ältere DELETEACL entfernt anschließend den Pointer. Der zusätzliche Read liegt weiterhin außerhalb der Pointer-Mutation. **Runde 5, Befund 7 ist nicht behoben.**

**Fix:** Pointer-Versionierung beziehungsweise eine wiederholbare, generationsgebundene Synchronisierung einführen. Ein einmaliger Kontrollread vor dem zweiten Write reicht nicht.

### 13. LIST entfernt Shared-Pointer auch bei vorübergehenden Fehlern

**Fundstelle:** `internal/imapproto/mailbox.go:308`.

Jeder Fehler aus `rightsOf`, einschließlich eines temporären RADOS-Lesefehlers, führt zur dauerhaften Pointer-Löschung. Damit kann eine einzelne fehlgeschlagene LIST-Abfrage eine weiterhin gültige Freigabe aus späteren Auflistungen entfernen.

**Fix:** Bei temporären Fehlern ausschließlich die aktuelle Ausgabe auslassen. Dauerhaftes Bereinigen nur bei nachgewiesen ungültiger, gegen konkurrierende Änderungen abgesicherter Referenz.

### 14. NOTIFY konkurriert weiterhin mit Writer-Zugriffen außerhalb von `cmdMu`

**Fundstellen:** `internal/imapproto/conn.go:347`, `internal/imapproto/conn.go:355`.

Die Antworten bei überlangen Kommandozeilen und sonstigen Lesefehlern benutzen Writer und Flush ohne `cmdMu`. Gleichzeitig kann `notifyLoop` unter seiner Sperre auf denselben gepufferten Writer zugreifen. Die IDLE-DONE-Korrektur deckt diese Fehlerpfade nicht ab.

**Fix:** Auch diese Antwort- und Flush-Pfade serialisieren; das blockierende Netzwerklesen selbst außerhalb der Sperre lassen.

### 15. Frühe REPLACE-Ablehnung lässt Literalinhalt als Folgekommandos zurück

**Fundstelle:** `internal/imapproto/mailbox.go:1422`.

Anders als APPEND verlassen die frühen Rechte-/Zielprüfungen REPLACE ohne `drain`. Bei einem bereits gesendeten nicht synchronisierenden Literal wird dessen Nachrichtentext anschließend als neue IMAP-Kommandos gelesen. Dadurch verliert die Verbindung ihre Kommandogrenzen.

**Fix:** Auf allen frühen Ablehnungspfaden verbleibende Argumente einschließlich bereits angekündigter Literale korrekt konsumieren.

### 16. EXAMINE verbraucht durch späteres Polling doch `\Recent`

**Fundstellen:** `internal/imapproto/selected.go:121`, `internal/imapproto/selected.go:222`.

SELECT überspringt `MarkRecent` bei EXAMINE korrekt. Trifft danach eine neue Nachricht ein, ruft NOOP-/IDLE-Polling jedoch uneingeschränkt `claimRecent` auf und verändert das dauerhafte Wasserzeichen. Eine nachfolgende Session sieht diese Nachrichten nicht mehr als recent.

**Fix:** Den EXAMINE-Zustand auch beim Claim berücksichtigen. [RFC 3501 §6.3.2](https://www.rfc-editor.org/rfc/rfc3501.html#section-6.3.2)

### 17. MOVE kann konkurrierende Änderungen dauerhaft aus dem Session-Snapshot verlieren

**Fundstellen:** `internal/imapproto/selected.go:333`, `internal/imapproto/selected.go:718`.

`announceExpunge` übernimmt den neuesten Mailbox-Header, ohne sämtliche Änderungen bis zu dessen ModSeq einzulesen. Trifft während MOVE eine weitere Nachricht ein, kann ihr ModSeq damit bereits als verarbeitet gelten. Beim MOVE in dieselbe Mailbox werden anschließend nur die eigenen Kopien angehängt; weitere Polls überspringen die fremde Zustellung.

**Fix:** Snapshot und verarbeiteten ModSeq gemeinsam fortschreiben; den Wasserstand ausschließlich bis zu tatsächlich eingelesenen Änderungen anheben.

### 18. Adresssuche unterscheidet TO, CC und BCC weiterhin nicht

**Fundstellen:** `internal/imapproto/search.go:262`, `internal/imapproto/search.go:577`.

Alle drei Suchschlüssel werden auf `FieldTo` reduziert. Auch die abschließende Header-/Envelope-Prüfung durchsucht wieder alle drei Felder. Eine Nachricht mit dem gesuchten Empfänger ausschließlich in `Cc` trifft deshalb auch `SEARCH TO`.

**Fix:** Den ursprünglichen Headernamen im Suchkriterium erhalten; den gemeinsamen FTS-Index nur zur Kandidatenauswahl verwenden.

## Niedrige Schwere

### 19. Adressparser verändert gültige quoted Localparts

**Fundstelle:** `internal/mailmeta/address.go:173`.

Bei `"a b"@example.org` bewahrt `word()` das Leerzeichen zunächst korrekt. Der anschließende `ReplaceAll` macht daraus jedoch `ab@example.org`. Die Schreibweise `<"a b"@example.org>` läuft durch einen anderen Pfad und liefert ein anderes Ergebnis.

**Fix:** Syntaktische Zwischenräume getrennt von Zeichen innerhalb quoted strings behandeln; keine pauschale Leerzeichenentfernung auf dem fertigen Localpart.

### 20. Verschachteltes MODSEQ fehlt in SEARCH-/ESEARCH-Antworten

**Fundstellen:** `internal/imapproto/search.go:374`, `internal/imapproto/search.go:406`.

Die Ausgabe prüft ausschließlich `cr.modseq` des Wurzelknotens. Bei beispielsweise `SEARCH RETURN (MIN) (MODSEQ 1)` liegt der Schlüssel im Unterknoten: Die Suche wird ausgewertet, aber der erforderliche MODSEQ-Rückgabewert fehlt.

**Fix:** Das Vorkommen eines MODSEQ-Kriteriums über den gesamten Kriterienbaum erfassen. [RFC 7162 §3.1.5](https://www.rfc-editor.org/rfc/rfc7162.html#section-3.1.5)

### 21. memstore-RENAME gibt eine weiter veränderbare ACL-Map ungesperrt weiter

**Fundstellen:** `internal/memstore/memstore.go:187`, `internal/memstore/memstore.go:194`, `internal/memstore/memstore.go:157`.

Die Deadlock-Korrektur gibt den Eigentümermutex vor `repoint` frei, kopiert aber die ACL-Map nicht. Ein paralleles SETACL auf dem umbenannten Ordner schreibt dieselbe Map, während `repoint` darüber iteriert: Data Race bis zum fatalen Concurrent-Map-Fehler. Betroffen ist der Test-Store.

**Fix:** ACLs unter dem Eigentümermutex tief kopieren und ausschließlich diese Snapshots für die anschließenden Pointer-Änderungen verwenden.

---

**Validierungsgrenze:** Die beschriebenen Abläufe sind statisch nachvollzogen, nicht laufzeitseitig reproduziert. Insbesondere RADOS-CAS-, Crash- und Race-Verhalten bleiben ohne entsprechende Tests unbestätigt. Bereits dokumentierte ältere Scrubber-/Recovery-Probleme sind hier nicht erneut vollständig aufgeführt.