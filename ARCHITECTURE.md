# Architektur

Der Server verbindet mehrere Mailprotokolle mit einem gemeinsamen Mailstore.
Er speichert direkt über librados in RADOS und kapselt dieses Backend hinter
Go-Interfaces. Dauerhafte Daten sind zwischen Replikas geteilt; aktive
Verbindungen und deren Zustände gehören jeweils einem Prozess.

Quellbasis: [main.go](cmd/shardpost/main.go), [model.User](internal/model/model.go),
[cephstore](internal/cephstore/store.go). Details zu Schreibreihenfolge und
Reparatur stehen im [Speicherkapitel](docs/architecture/storage.md).

## Systemgrenzen

![Systemkontext](docs/diagrams/context.svg)

| Komponente | Aufgabe | Grenze |
|---|---|---|
| Mailserver dieses Projekts | IMAP, POP3, LMTP, ManageSieve, Sieve, Mailstore, Lernworker | Kein öffentlicher SMTP-MTA und keine Benutzerverwaltung |
| nauthilus-director | Öffentliche Protokollzugänge und Backend-Routing; Client-IP per PROXY | Eigene Konfiguration, Zertifikate und Healthchecks |
| Nauthilus | Anmeldung und Auflösung einer Adresse zum kanonischen Konto | Kein Mailinhalt, kein Postfachindex |
| Ceph/Rook | RADOS-Persistenz, Replikation/EC; Bereitstellung der Ceph-Zugangsdaten | Keine globale Transaktion zwischen Mailobjekten |
| SMTP-Relay | Versand von Redirect, Vacation und Notify | Externer Seiteneffekt, nicht gemeinsam mit Ceph atomar |
| Lernprogramm/Rspamd | Verarbeitung eines bestätigten Lernauftrags | Wiederholungen müssen verträglich sein |
| Monitoring | Logs, Metriken und Traces | Ersetzt keinen echten Protokolltest |

Nauthilus wird im Produktivprofil über gRPC und einen OIDC-authentifizierten
Backchannel angesprochen. Das generische Auth-Interface erlaubt außerdem
HTTP und ein statisches Testbackend. Ein Loginname ist nicht automatisch der
Speichernamespace: Entscheidend ist das kanonische `auth.Result.Account`.

## Komponenten im Prozess

![Komponenten und Abhängigkeiten](docs/diagrams/components.svg)

`cmd/shardpost` ist die Verdrahtungsstelle. Sie lädt und validiert die
Konfiguration, initialisiert Logging und Telemetrie, beschafft Ceph-Zugang,
öffnet den Store und verbindet Protokollserver, Authentifizierung,
Sieve-Engine, Programmstarter und Lernworker. Sie startet außerdem
Health-/Readiness-/Metrik-Endpunkte und koordiniert das Herunterfahren.

Die Protokollserver besitzen ihre Verbindungen und übersetzen Befehle in
Operationen auf `model.User`. `imapproto` implementiert Parser, Dispatcher,
Sitzungszustände und Ausgabe selbst; go-imap liefert Typen und Hilfsbausteine.
LMTP verwendet go-smtp und ist derzeit direkt mit dem Ceph-Store gekoppelt.
Das bedeutet: Nicht sämtliche Protokollpakete sind cgo-frei.

`internal/sieve` wertet Skripte mit dem mitgelieferten go-sieve-Fork aus.
`internal/imapsieve` ordnet Mailboxereignisse diesen Skripten und Lernregeln
zu. Der externe Programmstarter akzeptiert ausschließlich konfigurierte
Programme. Er startet keine Shell und begrenzt Eingabe, Ausgabe, Laufzeit
und Parallelität. Der Klassifizierer ist ein konfiguriertes Programm, keine
Rspamd-Sonderbehandlung im Go-Mailstore.

## Deployment und Verbindungen

![Deployment](docs/diagrams/deployment.svg)

Das vorhandene Kubernetes-Profil enthält zwei Replikas, einen Service,
ein PodDisruptionBudget, Netzregeln und einen Scrub-CronJob. Alle Replikas
verwenden dieselben Pools. Topologieverteilung verbessert die Platzierung,
ist aber keine Garantie gegen korrelierte Ausfälle.

| Service-Port | Listener im Container | Verwendung |
|---|---|---|
| 143 | 1143 | IMAP, optional STARTTLS |
| 993 | 1993 | IMAP mit implizitem TLS |
| 110 | 1110 | POP3, optional STLS |
| 995 | 1995 | POP3 mit implizitem TLS |
| 4190 | 14190 | ManageSieve, optional STARTTLS |
| 24 | 2424 | LMTP, optional STARTTLS |
| kein Mail-Service-Port | 8080 | `/healthz`, `/readyz`, `/metrics` am Pod |

Die tatsächliche Aktivierung hängt von Konfiguration und TLS-Dateien ab.
Die Existenz eines Service-Ports beweist weder Listenergesundheit noch
öffentliche Erreichbarkeit. Der aktuelle öffentliche Nachweis umfasst IMAP
und ManageSieve, nicht jede denkbare POP3-Route.

Die Listener werden zuerst mit dem vertrauensgebundenen PROXY-Parser und
anschließend gegebenenfalls mit implizitem TLS umhüllt. `RemoteAddr()` liefert
damit die effektive Client-Adresse. Die Receiver-Vertrauensliste und die
PROXY-Ausgabe des Directors sind ein gemeinsamer Vertrag; die NetworkPolicy
begrenzt zusätzlich die erreichbaren Absender.

## Zustand, Parallelität und Skalierung

Verbindung, ausgewählte Mailbox, Parserpuffer, Watch und Antwortschreiber
leben im Pod. `cmdMu` serialisiert im IMAP-Pfad Befehle und asynchrone
Benachrichtigungen. Speicherbudgets begrenzen eingelesene Literale und
Nachrichtenarbeit; `GOMEMLIMIT` ersetzt dieses Anwendungsbudget nicht.

Dauerhafte Maildaten, Änderungsjournale, Skripte und Lernjobs liegen in RADOS.
Einzelne Objektänderungen verwenden Compare-and-Swap; über mehrere Objekte
arbeiten Intents, Pending-Markierungen und Reparaturpfade. POP3-Maildrop-
und Lernoperationen verwenden darüber hinaus erneuerbare Leases. Es gibt
keine allgemeine Zusage einer transaktionalen Datenbank über alle Objekte.

Watch/Notify weckt IMAP-Sitzungen nach Änderungen anderer Pods. Benachrichtigungen
sind Hinweise; Header, MODSEQ und Änderungsjournal bleiben die Grundlage für
die Synchronisierung. Verlorene Hinweise dürfen nicht mit verlorenem
Mailzustand verwechselt werden.

Mehr Replikas verteilen Verbindungen und Arbeit. Heiße Konten, CAS-Konflikte,
Ceph-Latenz, Speicherbudgets und nachgelagerte Dienste können weiter begrenzen.
Eine garantierte Millionenkapazität ist nicht aus den bisherigen Tests ableitbar.

## Ausfälle und ihre sichtbaren Folgen

| Ausfall | Wirkung | Reaktion/Nachweis |
|---|---|---|
| Nauthilus/Backchannel | Neue Anmeldung oder Empfängerauflösung scheitert temporär | Auth-Fehler getrennt von falschen Zugangsdaten behandeln |
| Ceph nicht erreichbar | Store-Operationen schlagen fehl; Readiness prüft `Store.Ping` | Ceph und echte Mailoperationen prüfen; Liveness allein reicht nicht |
| Pod wird beendet | Sitzungen werden beendet; Clients verbinden neu | Readiness wird rot, Drain, begrenztes Shutdown-Fenster |
| Watch fällt aus | Verzögerte Aktualisierung | Polling/Neuanmeldung am Watch, persistente Änderungsdaten bleiben maßgeblich |
| Skript schlägt fehl | LMTP nutzt vorgesehenen INBOX-Fallback; IMAP erhält das Original gemäß Aktionspfad | Protokollkontext beachten; kein einheitliches Verhalten für alle Aktionen |
| Lernziel nicht erreichbar | Persistenter Job wartet bzw. läuft in Retry/Dead | Queue-/Programmmetriken und Jobzustand prüfen |
| Collector nicht erreichbar | Telemetrie kann ausfallen, Mailbetrieb kann weiterlaufen | Exportfehler und Telemetrie gesondert beurteilen |

## Entwurfsentscheidungen

Die [Entscheidungsübersicht](docs/decisions/architecture.md) erklärt RADOS,
Pooltrennung, Interfaces, Intents und die Programm-Allowlist samt Konsequenzen.
[UML und Sequenzdiagramme](docs/architecture/runtime.md) führen von den
Interfaces zu konkreten Login- und Zustellpfaden.
