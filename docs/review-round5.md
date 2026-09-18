## Neue Befunde – Runde 5

1. **Hoch – Redirect bestätigt Nachrichtenverlust.**  
   `internal/lmtpd/lmtpd.go:354`: Bei `Auto-Submitted != no` wird ohne Versand Erfolg zurückgegeben. Ein reines Redirect-Script hat den impliziten Keep aufgehoben: weder Speicherung noch Weiterleitung, trotzdem LMTP-Erfolg. Dasselbe gilt für ausschließlich herausgefilterte Selbst-Ziele (`:372`).

2. **Hoch – CATENATE umgeht das Kommando-Speicherbudget.**  
   `internal/imapproto/mailbox.go:646`, `:655`, `:1128`: URL-Ergebnisse werden vollständig in `parts` gesammelt, ohne `cmdBytes` oder dauerhafte Budgetreservierung. Das Größenlimit gilt pro Nachricht; viele CATENATE-Nachrichten in einem MULTIAPPEND können den Prozessspeicher ausschöpfen.

3. **Mittel – Ersetzendes STORE entfernt geschützte Flags.**  
   `internal/imapproto/selected.go:367`, `:384`: Geprüft werden nur ausdrücklich angegebene Flags. Mit lediglich `rs` entfernt `STORE 1 FLAGS ()` auch vorhandene `\Deleted`-Flags und Keywords ohne `t/w`.

4. **Mittel – APPEND/COPY übernehmen unerlaubte Zielflags.**  
   `internal/imapproto/mailbox.go:701`, `internal/imapproto/selected.go:537`: Nach Prüfung von `i` werden Flags ungefiltert gespeichert. Damit reichen reine Einfügerechte für `\Deleted`, `\Seen` und Keywords; fehlende `t/s/w`-Rechte müssen berücksichtigt werden. [RFC 4314 §4](https://www.rfc-editor.org/rfc/rfc4314.html#section-4)

5. **Mittel – Fremde Mailboxexistenz ohne Rechte erkennbar.**  
   `internal/imapproto/selected.go:491`, `internal/imapproto/mailbox.go:596`: COPY/MOVE/APPEND liefern bei fehlendem Ziel `TRYCREATE`, bei vorhandenem unberechtigtem Ziel `NONEXISTENT`. CREATE unterscheidet entsprechend anhand vorhandener Eltern (`mailbox.go:361`).

6. **Mittel – Cross-Namespace-COPY hinterlässt unbestätigte Teilkopien.**  
   `internal/imapproto/selected.go:537`: Scheitert Nachricht 2 etwa an der Zielquota, bleibt Nachricht 1 gespeichert; der Befehl endet ohne COPYUID mit NO. Wiederholung erzeugt Duplikate. Beim MOVE bleiben zusätzlich die Quellen erhalten.

7. **Mittel – SETACL/DELETEACL-Race verliert gültige LIST-Verweise.**  
   `internal/cephstore/acl.go:104`: Nach ACL-Entzug kann ein paralleles SETACL Rechte und Verweis wiederherstellen, bevor das ältere DELETEACL anschließend den Verweis entfernt. Die gültige Freigabe verschwindet dauerhaft aus LIST.

8. **Mittel – Akzeptiertes `/private` wird gemeinsam gespeichert.**  
   `internal/imapproto/metadata.go:23`, `internal/cephstore/metadata.go:67`: `/private` wird als Entry akzeptiert, aber nur `/private/…` privat gespeichert. Ein dort gesetzter vermeintlich privater Wert ist für andere Berechtigte sichtbar; `/shared` umgeht analog die vorgesehene `w`-Prüfung.

9. **Mittel – NOTIFY tauscht den Reader während paralleler Netzwerkzugriffe.**  
   `internal/imapproto/notify.go:432`: `fetchAttributes` ersetzt `c.r`, während Kommandoschleife beziehungsweise IDLE ohne diese Sperre daraus lesen (`conn.go:341`, `:865`). Der Netzwerkleser kann den synthetischen Reader erwischen: Race, Fehlparsing oder Verbindungsabbruch.

10. **Mittel – IDLE-DONE schreibt außerhalb von `cmdMu`.**  
    `internal/imapproto/conn.go:904`: DONE-Verarbeitung schreibt OK/BAD und verändert Readerzustand, bevor das Defer die Sperre zurückholt. Gleichzeitige NOTIFY-Ausgabe kann Antworten im gemeinsamen Writer beschädigen.

11. **Mittel – NOTIFY verliert bestellte MessageNew-FETCH-Attribute.**  
    `internal/imapproto/notify.go:406`: Gewinnt der zusätzliche IDLE-Poll oder ein NOOP-Poll, enthält `c.msgs` die neue Nachricht bereits. NOTIFY erkennt anschließend keine neue UID mehr und liefert deren bestellte FETCH-Attribute nicht.

12. **Mittel – Vacation läuft auch nach fehlgeschlagener Speicherung.**  
    `internal/lmtpd/lmtpd.go:265`: Das uneingeschränkte `defer` sendet die Abwesenheitsantwort auch nach Quota-/Speicherfehlern und Rollback. Die Originalnachricht wird abgelehnt, die Antwortfrist trotzdem verbraucht.

13. **Mittel – QUIT-Fehler dupliziert bereits bestätigte Weiterleitungen.**  
    `internal/outbound/outbound.go:109`: Nach positiv bestätigtem DATA wird ein QUIT-Fehler als Versandfehler behandelt. LMTP antwortet 451; der Zustellwiederholungsversuch sendet die bereits angenommene Nachricht erneut.

14. **Mittel – Fehlgeschlagener Vacation-Versand verbraucht die Sperrfrist.**  
    `internal/lmtpd/lmtpd.go:394`: `VacationSeen` persistiert vor `Relay.Send`. Ein temporärer Relay-/TLS-Fehler unterdrückt dadurch weitere Antworten für die gesamte Frist, standardmäßig sieben Tage.

15. **Mittel – CATENATE lehnt die vorgesehenen relativen URLs ab.**  
    `internal/imapproto/mailbox.go:1137`: Akzeptiert wird ausschließlich `imap:///…`. Gültige relative URLs wie `/INBOX/;UID=1` scheitern mit BADURL; genau diese verlangt [RFC 4469 §3](https://www.rfc-editor.org/rfc/rfc4469.html#section-3).

16. **Niedrig – SELECT meldet widersprüchliche Schreibrechte.**  
    `internal/imapproto/mailbox.go:811`: READ-ONLY hängt an `examine` statt `c.readOnly`; SELECT mit lediglich `lr` antwortet READ-WRITE. Bei eingeschränkten Flagrechten werden außerdem sämtliche PERMANENTFLAGS angeboten (`:801`).

17. **Niedrig – MYRIGHTS verweigert zulässige Abfragen.**  
    `internal/imapproto/acl.go:321`: Ausschließlich `l/r` erlauben die Antwort. Auch alleinige Rechte `i/k/x/a` müssen diese Abfrage ermöglichen. [RFC 4314 §4](https://www.rfc-editor.org/rfc/rfc4314.html#section-4)

18. **Niedrig – Gegenseitige Freigaben können memstore blockieren.**  
    `internal/memstore/memstore.go:145`: DELETE/RENAME halten den Eigentümermutex und sperren anschließend den Berechtigten. Gleichzeitige Operationen auf gegenseitig freigegebenen Ordnern ermöglichen Alice→Bob/Bob→Alice-Deadlocks.

**Validierung:** Befunde 2–5, 8 und 15 durch sechs temporäre Protokolltests reproduziert. Vorhandene IMAP-/Sieve-/Outbound-Tests sowie NOTIFY-Race-Tests bestanden; letztere decken die genannten Race-Fenster nicht ab. Ceph-/LMTP-/Server-Tests scheitern am fehlenden `rados/librados.h`. Projektdateien unverändert.