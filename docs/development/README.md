# Entwicklerhandbuch

[Dokumentationsindex](../README.md) · [UML und Laufzeit](../architecture/runtime.md)

## Voraussetzungen und Buildmodell

Die Go-Version steht in [go.mod](../../go.mod), aktuell `1.27.1`.
Der Server baut gegen Ceph Tentacle/librados mit Buildtag `tentacle`.
Der [Dockerfile](../../Dockerfile) enthält die dazu passende Linux-Umgebung,
führt `go vet` aus und erzeugt Server und Lastgenerator. Die Runtime läuft
als UID/GID `167:167`; die externen Programme bleiben root-owned.

`vendor` ist der reproduzierbare Dependency-Bestand des Containerbuilds.
Der go-sieve-Fork liegt zusätzlich in `third_party/go-sieve`; `go.mod`
enthält dafür ein lokales `replace`. Änderungen am Fork müssen bewusst in
den Vendor-Bestand übernommen werden. Nur eine der beiden Kopien zu ändern
kann dazu führen, dass Tests und ausgeliefertes Image verschiedenen Code nutzen.

## Codekarte

| Bereich | Einstieg | Verantwortung |
|---|---|---|
| Start/Konfiguration | [main.go](../../cmd/shardpost/main.go), [config.go](../../internal/config/config.go) | Verdrahtung, Defaults, Validierung, Lifecycle |
| Listener | [listen.go](../../cmd/shardpost/listen.go) | TCP, vertrauensgebundenes PROXY, TLS |
| IMAP | [conn.go](../../internal/imapproto/conn.go), [wire.go](../../internal/imapproto/wire.go) | Parser, Zustände, Befehlsdispatch, Antwortkodierung |
| Mailbox-Synchronisierung | [selected.go](../../internal/imapproto/selected.go), [notify.go](../../internal/imapproto/notify.go) | MODSEQ, Snapshots, IDLE/NOTIFY |
| POP3 | [pop3.go](../../internal/pop3proto/pop3.go) | Maildrop, Lease, Löschmarkierungen |
| LMTP | [lmtpd.go](../../internal/lmtpd/lmtpd.go) | Empfängerauflösung, Puffern, Status pro Empfänger |
| Zustellung | [delivery](../../internal/delivery/delivery.go) | Sieve-Aktionen, Speichern, Rückabwicklung, Redirect/Vacation/Notify; gemeinsam für LMTP und LDA |
| Admin-API | [admin](../../internal/admin/admin.go), [Vertrag](../../api/admin/openapi.yaml), [shardpostctl](../../cmd/shardpostctl/main.go) | Listener, Rollen, Audit, lokale Zustellung |
| Identität | [auth.go](../../internal/auth/auth.go), [SASL](../../internal/auth/sasl.go) | Request/Result-Vertrag, Nauthilus, Master-User, OIDC |
| Store-Vertrag | [model.go](../../internal/model/model.go) | Persistenzunabhängige Typen, User, Watch, Lease |
| Persistenz | [cephstore](../../internal/cephstore/store.go), [user.go](../../internal/cephstore/user.go) | RADOS, Kontosicht, CAS, Indexoperationen |
| Testbackend | [memstore.go](../../internal/memstore/memstore.go) | Protokolltests ohne librados |
| Suche | [fts](../../internal/fts/tokenize.go), [postings](../../internal/postings/postings.go) | Tokenisierung und kompakte Postinglisten |
| Sieve | [sieve.go](../../internal/sieve/sieve.go), [ManageSieve](../../internal/sieve/managesieve.go) | Auswertung und Skriptverwaltung |
| IMAPSieve | [engine.go](../../internal/imapsieve/engine.go), [learning.go](../../internal/imapsieve/learning.go) | Mailereignisse und dauerhafte Lernjobs |
| Externe Programme | [runner.go](../../internal/extprograms/runner.go), [learn.py](../../programs/learn.py) | Allowlist, Ressourcenlimits, austauschbarer Klassifizierer |
| Betrieb | [logging](../../internal/logging/logging.go), [metrics](../../internal/metrics/metrics.go), [telemetry](../../internal/telemetry/telemetry.go) | JSON-Logs, Prometheus, OTLP |
| Ressourcen | [membudget](../../internal/membudget/membudget.go), [rookcreds](../../internal/rookcreds/rookcreds.go) | Nachrichtenbudget und Ceph-Schlüsselrotation |

## Schnelle Tests ohne Ceph

Vom Projektverzeichnis aus:

```bash
go test -race ./internal/config ./internal/logging ./internal/auth ./internal/imapproto ./internal/pop3proto ./internal/sieve ./internal/imapsieve ./internal/extprograms ./internal/fts ./internal/postings
```

Die Tests öffnen teilweise lokale Sockets und starten Testprogramme. Eine
Umgebung, die Loopback oder Prozessausführung sperrt, ist dafür ungeeignet.
Das ist von einem tatsächlichen Protokollfehler zu unterscheiden.

`make test` führt die cgo-freie Race-Suite aus. Der frühere schreibende
Cluster-Test ist ausdrücklich als `make test-static-cluster` benannt und
setzt ein isoliertes statisches Testprofil voraus.

Ein einzelner Regressionstest:

```bash
go test -race ./internal/imapproto -run TestSessionLoggingAndSecretSafety -count=1
```

`newTestServer` in [memstore_test.go](../../internal/imapproto/memstore_test.go)
erzeugt einen echten TCP-Listener vor dem In-Memory-Store. Ergänzungen sollten
sichtbares Protokollverhalten prüfen, nicht nur interne Implementierungsdetails.

## Vollständiger Linux-Build und Tests

```bash
docker build --platform linux/amd64 --target build -t shardpost-build:dev .
docker run --rm \
  -v "$PWD/third_party:/src/third_party:ro" \
  shardpost-build:dev go test -tags tentacle ./...
docker build --platform linux/amd64 -t shardpost:dev .
docker run --rm -e LOG_LEVEL=info shardpost:dev config check
```

Der Fixture-Mount ist erforderlich: Ein Server-Regressionstest lädt
Pigeonhole-Testdateien aus `third_party`; der produktive Dockerfile kopiert
diesen Testbestand nicht in seine Buildstufe. Die Go-Pakete werden aus
`vendor` gebaut. `go test` ist hier kein produktiver Ceph-End-to-End-Test.

Bei Änderungen an Nebenläufigkeit die komplette Linux-Suite zusätzlich mit
`go test -p 1 -race -tags tentacle ./...` ausführen. Umfang und Plattform der
ausgeführten Prüfungen in einem Release-Bericht festhalten.

Für einen isolierten echten Serverstart werden Ceph-Pools, cephx-Zugang,
TLS und ein Authentifizierungsprofil benötigt. Keine produktiven Secrets
in Shell-Historie, Beispielkonfiguration oder Logs kopieren. Die vorhandenen
Kubernetes-Profile sind infrastrukturspezifisch; sie sind kein lokales
Ein-Befehl-Entwicklungssystem.

## Typische Änderungen

**Neuer IMAP-Befehl:** Parser-/Dispatchpfad in `conn.go`, passendes Fachmodul,
Zustands- und Rechteprüfung, Antwortsemantik, Ressourcenfreigabe und Tests
gegen den TCP-Testserver ergänzen. Capability nur ankündigen, wenn die
Semantik tragfähig ist. Den bekannten Befehlsnamen für Metriken und Debug-
Logs ergänzen; keine frei eingegebenen Argumente zu Labels machen.

**Neue Store-Operation:** Vertrag in `model.User`, In-Memory-Implementierung
und Ceph-Implementierung zusammen ändern. CAS-, Lease- und Absturzverhalten
im Speicherkapitel erklären. Bei mehreren Objekten eine Wiederherstellungs-
strategie und Tests an den Abbruchgrenzen vorsehen.

**Neue Konfigurationsoption:** typisiertes Feld mit `mapstructure`, bei
passenden skalaren Werten `env`, Default und Validierung ergänzen. YAML,
Env-Vorrang, ungültige Werte und geheimnisfreie Fehlermeldung testen.
Neue Secrets bevorzugt über Datei-Verweise laden. Dateibeispiele und
Betriebsprofil getrennt halten.

**Neue Sieve-Erweiterung:** zuerst den Fork-Vertrag und dessen
[AGENTS.md](../../third_party/go-sieve/AGENTS.md) lesen. Loader und Runtime,
Capability-/Require-Prüfung, Aktionssemantik und beide Protokollkontexte
beachten. Der Fork hat eigenständige Compiler- und Pigeonhole-Tests.
Anschließend Vendor-Parität und das tatsächlich gebaute Image prüfen.

**Neues Lernziel:** einen begrenzten Helper bereitstellen und als Programm
konfigurieren. Eingabe auf stdin; keine Shell. Exit 0 bedeutet Erfolg,
75 temporären Fehler, andere Codes endgültigen Fehler. Ein bereits gespeicherter
Job referenziert den Programmnamen; diesen nicht ohne Übergang entfernen.

## Ressourcen und Logging

Jede geöffnete `model.User`-Sicht, Watch, Lease und Memory-Reservation braucht
einen passenden Abschluss. Callback-Code darf nach Freigabe der RADOS-
IOContexts nicht weiter darauf zugreifen. Schreibe keine Antwort gleichzeitig
zum IMAP-NOTIFY-Pusher; beachte dessen `cmdMu`-Vertrag.

Logs verwenden `slog`/JSON. Für Verbindungen `logging.Session` und
`logging.Begin` verwenden; `session_id`, `proto`, `remote` und `local` bilden
den Zusammenhang. Info trägt Lebenszyklus und Auth-Ergebnis, Debug bekannte
Befehlsnamen/Laufzeiten, Warn einen beeinträchtigten Betrieb und Error einen
fehlgeschlagenen Vorgang. Keine Auth-Requests, Mailtexte oder Skriptargumente
als Ganzes loggen. Details: [Log-Konfiguration](../configuration.md).

## Dokumentation und Review

Vor einer Änderung den betroffenen Vertrag und einen realen Aufrufer lesen.
Nach der Änderung Quelllinks, Diagramme, Konfigurationsbeschreibung und
Regressionstest gemeinsam aktualisieren. Das [Diagrammwerkzeug](../diagrams/README.md)
hält Mermaid-Quellen und SVGs synchron. Release- und Testberichte nennen
Version, Plattform, tatsächlich ausgeführte Prüfungen und verbleibende Lücken.

Im aktuellen Arbeitsverzeichnis fehlt Git-Metadatenbestand. Deshalb lässt
sich hier kein Commit- oder Tag-Nachweis ableiten. In einem späteren Repository
gehören Quellstand, Vendor-Stand, Tests und Release-Digest in denselben
nachvollziehbaren Freigabeprozess.
