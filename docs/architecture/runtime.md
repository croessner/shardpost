# Laufzeit, Interfaces und Protokollabläufe

[Dokumentationsindex](../README.md) · [Architektur](../../ARCHITECTURE.md)

## UML: die wichtigsten Verträge

![UML-Interfaces](../diagrams/interfaces.svg)

Die UML zeigt einen Ausschnitt der Go-Verträge, keine vollständige API-
Referenz. Methodensignaturen sind zur Lesbarkeit verkürzt, insbesondere
Fehlerrückgaben. In Go erfolgt die Implementierung implizit über Methodensätze.
Die maßgebliche vollständige Definition ist [model.go](../../internal/model/model.go).

`auth.Authenticator` trennt Zugangsdatenprüfung von Protokollverarbeitung.
`Authenticate` liefert das kanonische Konto; `Lookup` löst LMTP-Empfänger
ohne Benutzerpasswort auf. `ErrInvalid` und `ErrTemporary` sind verschiedene
Ergebnisse: Ein Backend-Ausfall darf nicht als sicher falsches Passwort gelten.

`model.User` ist eine kontogebundene Sicht auf Mailboxen, Nachrichten,
Skripte, Metadaten und Kontingente. Der Aufrufer muss `Close()` ausführen.
Watches und Leases haben eigene Lebenszyklen; ein verlorenes Lease verlangt
das Ende geschützter Änderungen. Der In-Memory-Store ist ein Testbackend,
kein Beweis für die Fehlersemantik verteilter RADOS-Operationen.

Die tatsächlichen Store-Typen und Adapter stehen in
[cephstore/store.go](../../internal/cephstore/store.go),
[memstore.go](../../internal/memstore/memstore.go) und
[main.go](../../cmd/shardpost/main.go).

## Start und Shutdown

1. `loadConfig` wählt Datei und Unterbefehl; `config.Load` führt Defaults,
   YAML und explizite Umgebungswerte zusammen und validiert sie.
2. Nach erfolgreicher Konfiguration wird der JSON-Logger mit dem gewählten
   Level eingerichtet. `config check` endet hier ohne Öffnen der Dienste.
3. `run` richtet Signalbehandlung und Telemetrie ein. Ceph-Zugang kommt aus
   Konfiguration oder aus dem durch RBAC lesbaren Rook-Secret.
4. Store und Authenticator werden geöffnet; TLS-Schlüssel, Sieve- und
   Programmkonfiguration sowie optionaler Lernworker werden verbunden.
5. Mail-Listener und HTTP-Endpunkte werden gestartet. Ohne TLS-Konfiguration
   werden keine impliziten TLS-Listener gestartet.
6. Bei Shutdown wird Readiness zuerst deaktiviert. Nach einer kurzen
   Verteilungsfrist werden Dienste beendet und IMAP/POP3 begrenzt geleert.
   Worker und Tracing werden durch die vorgesehenen Abschlussfunktionen beendet.

`/healthz` meldet, dass der HTTP-Prozess antwortet. `/readyz` prüft zusätzlich
Drain-Zustand und Ceph-Erreichbarkeit. Keiner der beiden Endpunkte prüft
vollständig Nauthilus, Sieve, Lernziel oder einen Mailboxzugriff.

## Anmeldung und Sitzungszuordnung

![Login-Sequenz](../diagrams/login.svg)

Das Diagramm zeigt den Backend-Pfad; Director und Nauthilus können im
Frontend weitere Prüfungen durchführen. Die konkrete Backend-Anmeldung
hängt vom Director-Vertrag ab (z. B. Master-User oder Bearer-Replay).
Der Server öffnet erst nach erfolgreicher Authentifizierung und Kontolimit-
Prüfung die kontogebundene Store-Sicht. Speicherfehler können einen Login
auch nach erfolgreicher Identitätsprüfung noch verhindern.

Einstiegspunkte: `newConn`, `serve`, `handleLine`, `authenticate`,
`finishLogin` in [imapproto/conn.go](../../internal/imapproto/conn.go).
Die SASL-Zerlegung liegt in [auth/sasl.go](../../internal/auth/sasl.go),
die Backend-Auswahl in [cmd/shardpost/auth.go](../../cmd/shardpost/auth.go).

## IMAP-Zustände

![IMAP-Zustandsdiagramm](../diagrams/imap-states.svg)

Ausgewählte Mailbox, Sequenznummern und Watches gehören zur Sitzung.
`SELECT`/`EXAMINE` laden eine Sicht; `EXAMINE` ist schreibgeschützt.
`CLOSE` kann markierte Nachrichten expungieren, `UNSELECT` beendet nur die
Auswahl. Transportende und Shutdown können aus jedem Zustand eintreten.
Das Diagramm verdichtet diese Abbruchpfade als Übergang zu `Logout`.

`UNAUTHENTICATE` gibt Kontozustand und Watches frei, erhält aber den Transport
und die Logging-Sitzungs-ID. Ein nachfolgender Login darf keine Benutzer-
Attribute der vorigen Identität erben. `IDLE` ist hier ein Wartezustand
innerhalb der ausgewählten Mailbox, kein zusätzlicher Authentifizierungsstatus.

## LMTP-Zustellung

![LMTP-Zustellung](../diagrams/delivery.svg)

Die Empfängerauflösung erfolgt vor DATA. LMTP kann für einzelne Empfänger
unterschiedliche Ergebnisse zurückgeben. Sieve liefert einen Aktionsplan;
mehrere lokale Ziele werden mit einem Delivery-Intent zusammengehalten.
`RecordDelivery` speichert Mailbox und vorab gewählte GUID **vor** dem
Nachrichtenschreiben. Erst erfolgreiches `EndDelivery` erlaubt den dafür
vorgesehenen positiven Abschluss.

Die Abbildung zeigt den lokalen Erfolgspfad. Reject, Discard, Pipe,
Redirect, Vacation und Notify besitzen eigene Regeln. Externe Effekte
lassen sich nicht durch RADOS-Rollback rückgängig machen. Der vollständige
Ablauf einschließlich Fehlerpfaden steht in
[lmtpd.go](../../internal/lmtpd/lmtpd.go) (`LMTPData`, Status pro
Empfänger) und im gemeinsamen Zustell-Service
[internal/delivery](../../internal/delivery/delivery.go) (Sieve, Speichern,
Rollback, Relay-Helfer), den auch die lokale Zustellung der
[Admin-API](../admin-api.md) nutzt.

## Ereignisse und Lernaufträge

![Lernjob-Zustände](../diagrams/learning.svg)

Für den vorbereiteten Lernpfad wird eine unabhängige Nachrichtenkopie vor
dem Zielcommit persistiert. Commit-Belege verbinden Lernjob und sichtbare
Mailboxänderung. Der Worker prüft diese vor dem externen Aufruf. EXPUNGE
kann die bereits persistierte Lernkopie deshalb nicht ungültig machen.

`Delivered` trennt den externen Erfolg von späterer Bereinigung. Trotzdem
besteht ein Crash-Fenster zwischen externem Erfolg und dessen Speicherung:
Der Aufruf kann wiederholt werden. Exactly-once ist nicht zugesichert.
Dead-Jobs sind keine erfolgreichen Jobs und benötigen eine Betreiberentscheidung.

Die Darstellung konzentriert sich auf `Prepare`/Commit-Belege. Der Code
enthält außerdem `Enqueue` für direkt eingereihte Jobs ohne Prepared-Phase.
Details: [learning.go](../../internal/imapsieve/learning.go),
[RADOS-Queue](../../internal/cephstore/learning.go),
[Sieve-Verhalten](../imapsieve.md).
