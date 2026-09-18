# Qualitätsstand und Teststrategie

[Entwicklerhandbuch](README.md) · [Konformitätsprotokoll](../../hack/conformance.md)

## Was die einzelnen Ebenen beweisen

| Ebene | Prüft | Prüft nicht automatisch |
|---|---|---|
| Config-/Unit-Tests | Validierung, Datenstrukturen, definierte Fehlerpfade | Erreichbarkeit produktiver Dienste |
| TCP-Protokolltests mit memstore | Parser, Zustände, Antworten, ACL- und Logging-Verträge | RADOS-CAS, OSD-Ausfälle, echte Schlüsselrotation |
| Linux-Tests mit librados-Build | Kompilierung und Tests in der Serverplattform | Vollständigen Betrieb gegen ein echtes Ceph |
| Pigeonhole-Forktests | Skriptcompiler und Interpreter im Testadapter | Vollständige Dovecot-Umgebung oder externe Zustellung |
| imaptest | Konkrete IMAP-Konformitätsgruppen | Nicht angebotene oder übersprungene Capabilities |
| Native Abnahme | Tatsächlichen Netzwerk-, TLS-, Auth- und Store-Pfad | Andere Konten, unbekannte Clientvarianten oder Langzeitlast |
| Last-/Abbruchtests | Gemessene Last und getestete Fehlerzeitpunkte | Allgemeine Kapazitäts- oder Verfügbarkeitsgarantie |

## Stand ShardPost 0.13.0, 18.09.2026

`make lint` (golangci-lint 2.13.2 im Docker-Image mit librados) meldet über
den ganzen Baum 0 Issues ohne `//nolint`; `make test-docker` besteht mit
`-race` in allen 17 Testpaketen. Die Lint-Umbauten wurden gegen den Stand
vor der Bereinigung auf Verhaltensgleichheit reviewt. Live-Abnahme siehe
[Bericht 0.13.0](../reports/2026-09-18-shardpost-0.13.0.md).

## Vorheriger Stand ShardPost 0.12.0, 16.09.2026

Für die Umbenennung bestanden `go vet` und die vollständige Linux-Server-
Race-Suite `go test -p 1 -race -tags tentacle ./...`. Ergänzende Tests prüfen
neue/alte Config-Variablen, Vorrangregeln, OTel-Serviceidentität und IMAP ID.
Runtime-Checks bestätigen Binary-Aliase, Konfiguration und neuen Helperpfad.
Beide Testkonten bestanden nach Rollout öffentlich IMAP und ManageSieve
inklusive ShardPost-Identität, TLS, Anmeldung und vollständiger Log-Korrelation.
Siehe [Releasebericht](../reports/2026-09-16-shardpost.md).

## Vorherige Logging-Abnahme 0.11.3, 16.09.2026

Für die Logging-Änderung bestanden `go vet`, der vollständige Linux-
Serverlauf `go test -tags tentacle ./...` sowie gezielte Race-Tests für
Config, Logging, IMAP, POP3 und ManageSieve. Neue Tests prüfen Filterung,
Validierung, PROXY-Adresse, Sitzungs-ID und Geheimnisfreiheit der neuen
Debug-Ereignisse. `config check` im fertigen Runtime-Image bestand.

Nach dem Rollout bestanden beide Testkonten öffentlich IMAP und ManageSieve:
TLS, Login, NOOP und Logout sowie Log-Korrelation inklusive öffentlicher
Client-IP. Zwei Replikas waren Ready, ohne Neustarts. Siehe
[Release-Nachweis](../reports/2026-09-16-logging.md).

Diese Version hat nicht alle historischen Last-, RADOS-Abbruch- und
Konformitätssuiten erneut ausgeführt. Ein grüner Logging-Test ersetzt sie nicht.

## Letzter dokumentierter Konformitätsstand

Der [Bericht bis 0.11.2](../reports/2026-09-16-sieve-conformance-fixes.md)
und [hack/conformance.md](../../hack/conformance.md) dokumentieren für
Pigeonhole 139 bestandene, 0 fehlgeschlagene und 9 ausdrücklich ausgenommene
Dateien von 148. Ausnahmen zählen nicht als bestanden. Der Adapter vergleicht
bei Fehleranzahlen Fehlerfreiheit gegen Fehlerzustand, nicht exakt die Anzahl
der Dovecot-Diagnosen. Linux/TZ=UTC ist die Referenz für diese Suite.

Der letzte dokumentierte vollständige IMAP-Lauf an 0.11.0 hatte eine
fehlgeschlagene Gruppe (`fetch-envelope`, historische Source-Routes) und
11 wegen fehlender Capabilities übersprungene Gruppen von 72. Diese Einschränkung
wird durch den späteren Logging-Rollout nicht aufgehoben.

## Integrationsskripte gezielt auswählen

Die älteren `hack/test*.sh`-Skripte setzen teilweise statische Konten und
Testinfrastruktur voraus. `acceptance-nauthilus.sh` schreibt Nachrichten und
ändert kurzzeitig Mailboxen/ACLs. Es ist kein rein lesender Gesundheitstest.
`imapsieve-live-test.py` erzeugt temporäre Mailboxen/Skripte und kann Lernregeln
auslösen. Vor dem Ausführen Zielprofil, Konten, Seiteneffekte und Cleanup lesen.

Neue Release-Nachweise halten fest: Quell-/Imageidentität, Testplattform,
Endpunkte, Kontenumfang, Seiteneffekte, Ergebnis, Cleanup und offene Lücken.
Weder ein übersprungener Test noch fehlende Metriken sind ein Erfolg.
