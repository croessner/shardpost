**Ich würde diesen Stand noch nicht für produktive Mailbestände freigeben.** Besonders kritisch sind mögliche Datenverluste durch Scrubber, Refcount-Verwaltung und unzureichend abgesicherte Store-Änderungen. Dazu kommen mehrere Wege, den gesamten Serverprozess durch Protokolleingaben lahmzulegen.

Gelesen wurden die drei Dokumente sowie der Code unter `cmd/` und `internal/`, einschließlich Tests; `vendor/` blieb ausgeschlossen. Die Befunde sind statisch gegengeprüft, aber nicht durch Laufzeit-, Race-Detector- oder Ceph-Integrationstests reproduziert. Der [vollständige Review-Bericht](/private/tmp/codex-security-scans/imapcephpoc/review-20260914/report.md) enthält zusätzlich weitere Einzelbefunde.

1. **CAS verwendet möglicherweise die Version einer anderen Operation — hoch.**  
   `internal/cephstore/fts.go:493–505`, parallele Aufrufer bei `:107–109`. Zwischen `ReadOp.Operate` und `GetLastVersion()` können andere Operationen auf demselben `u.idx` laufen. Die verwendete go-ceph-Version liefert eine **IOContext-weite** letzte Objektversion, keine an diesen Read gebundene Version. Das verursacht falsche CAS-Konflikte und kann bei passender Versionsnummer veraltete Updates durchlassen. **Vorschlag:** eigene IOContexts für parallele Worker oder vollständige Serialisierung aller Kontextoperationen bis zur Versionsabnahme.

2. **Flags und CONDSTORE werden außerhalb des geschützten Read/Modify/Write geprüft — hoch.**  
   `internal/cephstore/user.go:941–991`. Nachrichten werden vor `casHeader` gelesen; auch nach einem CAS-Konflikt werden Existenz, Flags und `UNCHANGEDSINCE` nicht neu ausgewertet. Zwei parallele `+FLAGS` können Änderungen gegenseitig überschreiben. Ein zwischenzeitliches EXPUNGE kann durch das spätere Schreiben des alten Meta-Records rückgängig gemacht werden – gegebenenfalls ohne vorhandenen Blob. **Vorschlag:** Existenz, ModSeq und Flags innerhalb derselben abgesicherten Transaktion erneut lesen und prüfen.

3. **Höhere Shards veröffentlichen Änderungen vor deren tatsächlichem Commit — hoch.**  
   `internal/cephstore/user.go:611–617`, `:883–907`, `:964–999`. Header, Änderungslog und Notify werden aktualisiert, bevor der Eintrag auf einem höheren Shard geschrieben wird. Eine Session kann die neue ModSeq konsumieren, den Eintrag noch nicht finden und ihn anschließend dauerhaft verpassen. Außerdem können spätere Shard-Schreibvorgänge frühere überholen. **Vorschlag:** einen bestätigten Commit-Wasserstand einführen; Shard-Updates versionsgebunden durchführen und erst nach Abschluss veröffentlichen. Nur das Notify zu verschieben genügt nicht.

4. **EXPUNGE kann dieselbe Blob-Referenz mehrfach freigeben — hoch.**  
   `internal/cephstore/user.go:1027–1058`. `unlink` prüft nicht, welche angeforderten UIDs tatsächlich noch existieren; anschließend werden sämtliche übergebenen Referenzen dekrementiert. Beispiel: Eine Nachricht existiert zusätzlich in Archive. Zwei Sessions verwenden denselben INBOX-Snapshot und entfernen sie jeweils; der zweite Vorgang kann den Archive-Blob löschen. `CLOSE` verwendet ebenfalls einen alten Snapshot. **Vorschlag:** atomar die tatsächlich entfernten Einträge ermitteln und ausschließlich deren Referenzen einmal freigeben.

5. **Eine auf null gesetzte Referenz kann während der Löschung wiederbelebt werden — hoch.**  
   `internal/cephstore/blob.go:146–157`, `internal/cephstore/user.go:1087–1090`. Zwischen `refs=0` und `deleteBlob` liegen weitere Operationen. Ein paralleles COPY kann den Zähler wieder erhöhen und einen Zielindex anlegen; der bereits laufende Löschpfad entfernt trotzdem die Daten. **Vorschlag:** einen atomaren, endgültigen GC-Zustand einführen, ab dem neue Referenzen abgelehnt werden.

6. **Der Scrubber löscht gültige Mails nach einem unvollständigen Index-Scan — hoch.**  
   `internal/cephstore/scrub.go:162–175`, Löschung bei `:245–268`. Header- oder Indexlesefehler führen lediglich zu `continue`. Die dadurch fehlenden GUIDs werden später als Orphans behandelt und gelöscht, sofern sie alt genug sind. Die Karenzzeit schützt bestehende Nachrichten hier nicht. **Vorschlag:** Bei unvollständigem Referenzscan sämtliche destruktiven Reparaturen sowie Refcount- und Quota-Korrekturen abbrechen. Beschädigte, übersprungene Meta-Records müssen ebenfalls die Vollständigkeit ungültig machen.

7. **Scrub-Reparaturen sind gegenüber laufenden Änderungen nicht abgesichert — hoch.**  
   `internal/cephstore/scrub.go:209–217`, `:285–292`. Zwischen mtime-Prüfung und unbedingtem `SetXattr` kann COPY den Refcount erhöhen; der Scrubber überschreibt ihn anschließend mit seinem alten Zählergebnis. Späteres EXPUNGE kann dann noch referenzierte Daten löschen. Quota wird ebenfalls aus einem zeitlich inkonsistenten Scan überschrieben. **Vorschlag:** konsistente Scan-/Reparaturgrenzen mit Fencing oder Versionsprüfung; bei konkurrierenden Änderungen neu zählen.

8. **MOVE-Recovery entfernt die Quelle trotz gescheiterter Zielablage — hoch.**  
   `internal/cephstore/scrub.go:127–152`. Fehlt das Ziel oder schlägt `u.link` fehl, wird anschließend trotzdem `unlinkWithIntent` für die Quellnachrichten ausgeführt. Damit kann die letzte Indexreferenz verschwinden; die Orphan-Phase beseitigt danach den Blob. **Vorschlag:** Quelle und Intent erhalten, bis jede konkrete Zielverknüpfung bestätigt ist. Ein verschwundenes Ziel muss konservativ behandelt werden.

9. **MOVE entfernt seinen Recovery-Intent zu früh — hoch.**  
   `internal/cephstore/user.go:1176–1191`. Der Intent verschwindet im Header-CAS, bevor Quellnachrichten auf höheren Shards entfernt werden. Ein Crash hinterlässt Quelle und Ziel, aber nur den ursprünglichen Refcount und keinen Intent mehr. **Vorschlag:** Intent erst nach allen dauerhaft abgeschlossenen Schritten entfernen; Recovery anhand eindeutiger Operations- und Eintragsidentitäten idempotent gestalten.

10. **RENAME kann bestehende Ziel-Unterordner überschreiben — hoch.**  
    `internal/cephstore/user.go:387–401`. Geprüft wird nur der exakte neue Elternname. Existieren `A/B` und `X/B`, während `X` nur ein impliziter Elternknoten ist, kann `RENAME A X` den Registry-Eintrag von `X/B` ersetzen. Dessen bisheriger Bestand wird unzugänglich. **Vorschlag:** sämtliche resultierenden Zielnamen innerhalb einer konsistenten Registry-Sicht auf Kollisionen prüfen.

11. **Log-Trimming und QRESYNC erkennen Historienlücken nicht zuverlässig — mittel.**  
    `internal/cephstore/user.go:649–659`, `internal/imapproto/mailbox.go:562–584`. Ein älterer paralleler Trim kann `h:logstart` zurücksetzen, obwohl neuere Trims bereits mehr Records gelöscht haben. Unabhängig davon prüft QRESYNC-Replay `LogStart` überhaupt nicht und liefert bei alten Clientständen unvollständige VANISHED-Antworten. **Vorschlag:** `LogStart` monoton per CAS fortschreiben und bei Historienlücken bekannte UIDs gegen den aktuellen Bestand abgleichen.

12. **Jeder angemeldete Benutzer kann seine administrative Quota abschalten — hoch.**  
    `internal/imapproto/mailbox.go:674–708`. `SETQUOTA "" (STORAGE 0 MESSAGE 0)` wird ohne besondere Berechtigung dauerhaft gespeichert. Null bedeutet im Store unbegrenzt; negative Werte und ein Überlauf bei `n*1024` sind ebenfalls ungeprüft. **Vorschlag:** SETQUOTA für normale Benutzer ablehnen oder eine getrennte administrative Berechtigung verlangen; Werte und Überläufe prüfen.

13. **COPY umgeht die Quota bereits ohne Parallelität — mittel.**  
    `internal/cephstore/user.go:1082–1099`. Jedes Element eines COPY-Batches prüft denselben alten Verbrauch; die Summe wird erst nach dem gesamten Batch verbucht. Viele einzeln passende Nachrichten können gemeinsam die Grenze überschreiten. Bei einem späteren Teilfehler bleiben frühere Kopien sogar unverbucht. **Vorschlag:** Gesamtbedarf atomar reservieren und tatsächlich abgeschlossene Schritte zuverlässig abrechnen.

14. **Unbegrenzte Parser-Eingaben ermöglichen DoS vor der Anmeldung — hoch.**  
    `internal/imapproto/wire.go:75`, `internal/pop3proto/pop3.go:191`, `internal/sieve/managesieve.go:164–213`. Zeilen werden ohne Größenlimit bis LF akkumuliert. ManageSieve sammelt zusätzlich beliebig viele einzeln zulässige Literale; IMAP erlaubt über `ID` unbeschränkt verschachtelte Listen. Literal-Limits und Zeitlimits begrenzen diese Ressourcen nicht ausreichend. **Vorschlag:** harte Grenzen für Zeilenbytes, gesamte Kommandobytes, Argumentzahl und Verschachtelung vor der jeweiligen Allokation.

15. **Ein kurzes STORE-Kommando erzeugt eine Endlosschleife mit Speicherwachstum — hoch.**  
    `internal/imapproto/selected.go:322–324`. Nach SELECT reicht `STORE 1 +FLAGS foo (`: `atom()` scheitert ohne Parserfortschritt, sein Fehler wird ignoriert und die Schleife hängt fortlaufend leere Flags an. Socket-Timeouts beenden diese CPU-Schleife nicht. **Vorschlag:** Parserfehler sofort zurückgeben und die Flag-Grammatik strikt validieren.

16. **Unbekannte Kommandos erzeugen unbegrenzt dauerhafte Prometheus-Serien — hoch.**  
    `internal/imapproto/conn.go:324`, `internal/pop3proto/pop3.go:205`. Beliebige Kommandonamen werden auch vor Authentifizierung als Labels verwendet. Eine einzelne Verbindung kann fortlaufend neue Namen senden; die Serien bleiben nach Verbindungsende erhalten. **Vorschlag:** eine feste Kommandoliste verwenden und alle unbekannten Namen auf `UNKNOWN` abbilden.

17. **Zurückgestellte EXPUNGE-Meldungen gehen dauerhaft verloren — mittel.**  
    `internal/imapproto/selected.go:66–107`. `poll(false)` unterdrückt Entfernungen während FETCH/STORE/SEARCH, übernimmt aber trotzdem die neue `HighestModSeq`. Ein folgendes NOOP nimmt den unveränderten Wasserstand als Beleg, dass nichts zu tun sei. Phantomnachrichten und falsche EXISTS-/Sequenzzustände bleiben bestehen. **Vorschlag:** ausstehende Entfernungen separat speichern und beim nächsten zulässigen Kommando anwenden.

18. **MULTIAPPEND ist nicht atomar — mittel.**  
    `internal/imapproto/mailbox.go:396–433`. Jedes Literal wird unmittelbar durch `Deliver` gespeichert. Ein Fehler oder Verbindungsabbruch beim zweiten Literal lässt die erste Nachricht zurück, obwohl das Gesamtkommando nicht erfolgreich abgeschlossen wurde. Wiederholungen erzeugen Duplikate. **Vorschlag:** atomaren Batch-Commit implementieren; bis dahin MULTIAPPEND nicht bewerben.

19. **FETCH bestätigt fehlende oder unvollständige Daten als Erfolg — mittel.**  
    Zwei eigenständige Fehler: `internal/imapproto/fetch.go:130–147` hängt die Expansion von ALL/FAST/FULL während einer `range` an die Eingabeliste an; die neuen Elemente werden nie verarbeitet. Bei `:351–353` wird außerdem ein Blob-Lesefehler als `nil` zurückgegeben, worauf das Kommando mit OK endet. **Vorschlag:** Makros vorab expandieren und Lesefehler als fehlgeschlagenes Kommando weiterreichen.

20. **WriteTimeout wird während normaler Ruhezeit verbraucht — mittel.**  
    `internal/imapproto/conn.go:270–272`, IDLE bei `:751–815`. Die WriteDeadline wird vor dem blockierenden Read gesetzt. Nach mehr als den standardmäßigen zwei Minuten Ruhe kann schon die nächste NOOP-Antwort scheitern, obwohl der IdleTimeout noch lange nicht erreicht ist. Dasselbe betrifft spätere IDLE-Pushes und DONE. **Vorschlag:** die WriteDeadline unmittelbar vor tatsächlichen Schreiboperationen erneuern.

21. **POP3 verliert die Maildrop-Exklusivität während aktiver Sessions — mittel.**  
    `internal/pop3proto/pop3.go:439`, `internal/cephstore/user.go:1226–1238`. Der Lock wird einmal mit TTL erworben und niemals erneuert. NOOPs halten die Session über die zehn Minuten hinaus offen; danach kann eine zweite Session denselben Maildrop sperren. **Vorschlag:** Lease-Erneuerung mit sicherer Behandlung eines Leaseverlusts oder eine entsprechend begrenzte Session-Lebensdauer.

22. **Fehlerhafte Session-Verwaltung verliert Ressourcen dauerhaft — mittel.**  
    `internal/imapproto/conn.go:675–685`: Scheitert `Store.User`, wird der vorher reservierte Benutzer-Verbindungsslot nicht zurückgegeben; nach genug Fehlern bleibt das Konto trotz Storage-Erholung gesperrt. `internal/sieve/managesieve.go:432–437`: Wiederholtes AUTHENTICATE überschreibt den alten User ohne Close und verliert native IOContexts sowie Connection-Referenzen. **Vorschlag:** Slotreservierungen auf Fehlerpfaden zurückrollen und ManageSieve-Reauthentifizierung ablehnen.

23. **Sieve kann unbemerkt den falschen oder keinen Filter ausführen — mittel.**  
    `internal/cephstore/scripts.go:38–42`: Aktivnamen über 256 Bytes können geschrieben, aber nicht gelesen werden; der ignorierte Xattr-Fehler erscheint als „kein aktives Script“. Bei `:119–128` meldet RENAMESCRIPT auf einen existierenden Zielnamen Erfolg und aktiviert gegebenenfalls dessen vorhandenen Inhalt. **Vorschlag:** Namen konsistent begrenzen, Lesefehler weiterreichen und Rename samt Aktivstatus atomar und mit expliziter Kollisionsprüfung durchführen.

24. **ManageSieve-Quoted-Strings sind nicht korrekt dekodiert — niedrig.**  
    `internal/sieve/managesieve.go:188–194`. Das erste Anführungszeichen beendet den String auch nach einem Backslash; Escapes werden nicht dekodiert. Gültige Skripte mit escaped Quotes und ausgegebene Scriptnamen lassen sich dadurch nicht zuverlässig wieder einlesen. **Vorschlag:** einen Scanner mit expliziter Escape-Behandlung verwenden.

25. **LMTP ignoriert die sichere Auth-Konfiguration — mittel.**  
    `internal/lmtpd/lmtpd.go:50`. `AllowInsecureAuth` ist fest auf true gesetzt, unabhängig von `INSECURE_AUTH=false`. Bei konfigurierten Service-Credentials ist AUTH PLAIN vor STARTTLS weiterhin zulässig. **Vorschlag:** TLS-Pflicht für LMTP-Serviceauth explizit und konsistent konfigurieren.

26. **Teilweise Sieve-Zustellung erzeugt bei LMTP-Retries Duplikate — mittel.**  
    `internal/lmtpd/lmtpd.go:217–223`. Mehrere fileinto-Ziele werden nacheinander dauerhaft beschrieben. Scheitert ein späteres Ziel, erhält der gesamte Empfänger einen Fehler, obwohl frühere Ablagen bestehen bleiben. **Vorschlag:** persistente Zustellintents mit idempotenten Zielschritten oder einen atomaren Batch verwenden.

27. **HTTP-Auth klassifiziert Backendprobleme als falsches Benutzerpasswort — mittel.**  
    `internal/auth/nauthilus.go:263–281`. HTTP 401 wird wie eine definitive Benutzerablehnung behandelt, obwohl fehlerhafte Backchannel-Credentials die Ursache sein können. Ein Fehler in einer HTTP-200-Antwort wird ebenfalls nicht entsprechend dem gRPC-Pfad als temporär ausgewertet. **Vorschlag:** Backchannel-, Backend- und Benutzerfehler gemäß dem tatsächlichen Nauthilus-Vertrag getrennt abbilden. Einen belegten kontenübergreifenden Auth-Bypass habe ich im HTTP-/gRPC-/Master-User-Pfad nicht gefunden.

28. **Key-Rotation kann dauerhaft ausfallen und enthält ein Go-Datenrennen — mittel.**  
    `internal/rookcreds/rookcreds.go:157–164`: Der neue Key wird vor erfolgreichem `Rotate` als verarbeitet gespeichert; ein temporärer Verbindungsfehler wird daher nicht erneut versucht. Auch eine Rotation zwischen Startup-Fetch und erstem Poll kann verschluckt werden. Zusätzlich schreibt `store.go:94` die Config unter Lock, während `Store.User` bei `:170–184` ohne diesen Lock liest. **Vorschlag:** bestätigten Start-/Aktivkey verfolgen, fehlgeschlagene Wechsel wiederholen und Connection samt unveränderlicher Config gemeinsam referenzieren.

29. **FTS liefert falsche SEARCH-Ergebnisse — mittel.**  
    `internal/imapproto/search.go:503–525`. Nach der Kandidatensuche verschwinden die ursprünglichen Textkriterien vollständig. Beispielsweise kann `HEADER X-Special needle` wegen eines Treffers in einem anderen Header erfolgreich sein; TO/CC/BCC teilen ebenfalls ein Indexfeld. `internal/cephstore/fts.go:368–372` schneidet zusätzlich Kandidaten nach 256 Treffern ab. **Vorschlag:** FTS nur als konservativen Kandidatenfilter verwenden, Originalkriterien exakt nachprüfen und bei unvollständiger Abdeckung auf einen vollständigen Suchpfad wechseln.

30. **Zulässige Operationen überschreiten das Speicherbudget des Deployments — mittel.**  
    `internal/cephstore/blob.go:75–85`, `internal/imapproto/mailbox.go:423`, `internal/lmtpd/lmtpd.go:133–141`. Ganze Nachrichten werden gepuffert, auch wenn FETCH nur einen kleinen Partial benötigt; weitere Byte/String-Kopien erhöhen den Spitzenverbrauch. Bei 256 MiB Nachrichtenlimit und 512 MiB Podlimit genügen wenige parallele gültige Operationen für OOM. Ganze Mailbox-Snapshots pro Session verstärken das. **Vorschlag:** Streaming, ein globales gewichtetes Speicherbudget und begrenzte beziehungsweise gemeinsam genutzte Snapshots.

31. **FTS-Updates und Scrubber skalieren mit dem gesamten Bestand — mittel.**  
    `internal/cephstore/fts.go:467–476`: Jeder häufige Term wird bei jeder Änderung als vollständige GUID-Liste gelesen, durchsucht und kopiert; CAS-Retries wiederholen diese Arbeit. `scrub.go:66–85` enumeriert zunächst sämtliche Indexobjekte aller Namespaces, nur um Benutzer zu entdecken; danach bearbeitet `cmd/imapcephpoc/main.go:253` die Konten seriell. **Vorschlag:** segmentierte Postings und begrenzte Worker; für Scrub eine Benutzerregistry sowie partitionierte, fortsetzbare Jobs mit beschränktem Speicherbedarf.

Die bestehenden In-Memory-Protokolltests widerlegen die Store-Befunde nicht: Sie bilden insbesondere Ceph-CAS, Blob-Refcounts, Shard-Commit-Reihenfolgen und den tatsächlichen Ablauf einer POP3-Lock-TTL nicht ab.

| Befund | Schwere | Datei |
|---|---|---|
| 1. Fremde IOContext-Version im CAS | hoch | `internal/cephstore/fts.go:493` |
| 2. Veraltete Flags-/CONDSTORE-Prüfung | hoch | `internal/cephstore/user.go:941` |
| 3. Veröffentlichung vor Shard-Commit | hoch | `internal/cephstore/user.go:883` |
| 4. Mehrfache Referenzfreigabe durch EXPUNGE | hoch | `internal/cephstore/user.go:1027` |
| 5. COPY konkurriert mit endgültiger Blob-Löschung | hoch | `internal/cephstore/blob.go:146` |
| 6. Scrub löscht nach unvollständigem Scan | hoch | `internal/cephstore/scrub.go:162` |
| 7. Scrub überschreibt laufende Änderungen | hoch | `internal/cephstore/scrub.go:209` |
| 8. MOVE-Recovery löscht Quelle trotz Zielfehler | hoch | `internal/cephstore/scrub.go:144` |
| 9. MOVE-Intent zu früh entfernt | hoch | `internal/cephstore/user.go:1176` |
| 10. RENAME überschreibt Ziel-Unterordner | hoch | `internal/cephstore/user.go:387` |
| 11. Unzuverlässige Log-/QRESYNC-Abdeckung | mittel | `internal/cephstore/user.go:649`; `internal/imapproto/mailbox.go:562` |
| 12. Benutzer kann Quota abschalten | hoch | `internal/imapproto/mailbox.go:674` |
| 13. COPY-Batch umgeht Quota | mittel | `internal/cephstore/user.go:1082` |
| 14. Unbegrenzte Parser-Eingaben | hoch | `internal/imapproto/wire.go:75`; `internal/pop3proto/pop3.go:191`; `internal/sieve/managesieve.go:164` |
| 15. Endlose STORE-Allokationsschleife | hoch | `internal/imapproto/selected.go:322` |
| 16. Unbegrenzte Metrik-Kardinalität | hoch | `internal/imapproto/conn.go:324`; `internal/pop3proto/pop3.go:205` |
| 17. Zurückgestellte EXPUNGEs verloren | mittel | `internal/imapproto/selected.go:66` |
| 18. Nicht atomares MULTIAPPEND | mittel | `internal/imapproto/mailbox.go:423` |
| 19. Leere/fehlende FETCH-Daten mit OK | mittel | `internal/imapproto/fetch.go:130`; `:351` |
| 20. WriteDeadline läuft während Ruhezeit ab | mittel | `internal/imapproto/conn.go:270` |
| 21. POP3-Lock verfällt während Session | mittel | `internal/pop3proto/pop3.go:439` |
| 22. Verlorene Slots und native Ressourcen | mittel | `internal/imapproto/conn.go:675`; `internal/sieve/managesieve.go:437` |
| 23. Falsches oder verlorenes aktives Sieve-Script | mittel | `internal/cephstore/scripts.go:38`; `:119` |
| 24. Fehlerhafte ManageSieve-Escapes | niedrig | `internal/sieve/managesieve.go:188` |
| 25. LMTP erlaubt Auth trotz gewünschter TLS-Pflicht | mittel | `internal/lmtpd/lmtpd.go:50` |
| 26. Duplikate nach partieller Sieve-Zustellung | mittel | `internal/lmtpd/lmtpd.go:217` |
| 27. HTTP-Auth-Fehler falsch klassifiziert | mittel | `internal/auth/nauthilus.go:263` |
| 28. Key-Rotation ohne Retry; Config-Datenrennen | mittel | `internal/rookcreds/rookcreds.go:157`; `internal/cephstore/store.go:94` |
| 29. Falsche beziehungsweise fehlende SEARCH-Treffer | mittel | `internal/imapproto/search.go:503`; `internal/cephstore/fts.go:368` |
| 30. Speicherbudget durch gültige Operationen überschritten | mittel | `internal/cephstore/blob.go:75`; `internal/lmtpd/lmtpd.go:133` |
| 31. FTS-/Scrub-Aufwand wächst mit Gesamtbestand | mittel | `internal/cephstore/fts.go:467`; `internal/cephstore/scrub.go:66` |