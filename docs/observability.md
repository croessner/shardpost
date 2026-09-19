# Observability

## Signalquellen

Der Service stellt Prometheus-Metriken unter `/metrics` am `HTTP_ADDR`
(Standard `:8080`) bereit. `/healthz` prüft den HTTP-Prozess; `/readyz` prüft
zusätzlich den Store und berücksichtigt den Drain-Zustand. Ein erfolgreicher
Scrape allein beweist deshalb keine funktionierende Mailzustellung.

OpenTelemetry exportiert Traces, sobald `OTEL_EXPORTER_OTLP_ENDPOINT` gesetzt
ist. `OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf` wählt HTTP; andernfalls nutzt
der Service gRPC. `telemetry.service_name` bzw. `OTEL_SERVICE_NAME` bestimmt
`service.name` (Standard `shardpost`, seit 0.13.0 auch im Betriebsprofil; bis
0.12.x `imapcephpoc`). Die Collector-Redaktion hängt nicht vom Servicenamen ab;
`service.instance.id` ist der Hostname. Der Sampler ist ParentBased mit einem
TraceIDRatioBased-Root-Sampler: `OTEL_TRACES_SAMPLER_ARG=0.1` erfasst ungefähr
zehn Prozent der Root-Traces. Ohne Argument ist der Anteil 1. Ein anderer
`OTEL_TRACES_SAMPLER`-Wert ändert diese im Code festgelegte Auswahl nicht.

## Messwerte richtig lesen

| Messwert | Bedeutung und Grenze |
| --- | --- |
| `imapceph_connections{proto}` | Aktive IMAP-/POP3-Verbindungen; keine vollständige Verbindungszahl aller Protokolle. |
| `imapceph_command_seconds` | Histogramm der instrumentierten IMAP-/POP3-Befehle. `_count` eignet sich zur Befehlsrate; `_bucket` für Quantile. |
| `imapceph_commands_total` | Protokollspezifische Ereignisse. Kein universeller Request-Counter. IMAP `ok/no/bad` getrennt von Auth-, Verbindungs- und Poll-Ereignissen auswerten. |
| `imapceph_lmtp_deliveries_total{outcome}` | Erfolgreiche bzw. fehlgeschlagene instrumentierte Zustellversuche. |
| `imapceph_lmtp_delivered_bytes_total` | Bytes erfolgreicher LMTP-Zustellungen. |
| `imapceph_rados_op_seconds{op}` | Latenzen ausgewählter Header-/Indexoperationen: `header_read`, `header_cas`, `list_entries`, `get_entries`. Kein vollständiges RADOS-I/O-Profil. |
| `imapceph_rados_cas_retries_total{kind}` | Wiederholungen nach Compare-and-swap-Konflikten. |
| `imapceph_rados_notify_seconds` | Registriert, aber aktuell ohne Messaufruf. Nicht als bestätigte Notify-Latenz interpretieren. |
| `imapceph_scrub_findings_total{kind}` | Befunde im jeweiligen Scrubber-Prozess. Ein Server-Scrape beweist keinen erfolgreichen externen Scrub-Lauf. |

POP3 zählt in `commands_total` nur ausgewählte Ereignisse; `DELE` zählt dabei
gelöschte Nachrichten. Bei IMAP können Auth-Ereignisse zusätzlich zur
Protokollantwort gezählt werden. Eine Summe aller Outcomes als Nenner einer
Fehlerquote würde unterschiedliche Ereignisse vermischen. IMAP `UID`-Befehle
werden im aktuellen Histogramm gemeinsam unter `UID` erfasst.

Fehlende Serien sind keine Nullwerte. Insbesondere entstehen viele gelabelte
Serien erst beim ersten Ereignis. Bei Quantilen müssen Histogramm-Buckets über
die ausgewählten Pods summiert werden, bevor das Quantil berechnet wird;
Pod-Quantile dürfen nicht gemittelt werden. Bei fehlenden Beobachtungen sind
Quantile nicht aussagekräftig.

## Traces und Zugriff

Instrumentiert sind IMAP-/POP3-Befehle und `lmtp.deliver`. Die Traces enthalten
derzeit auch Benutzerattribute (`imap.user`, `pop3.user`, `lmtp.user`), bei IMAP
gegebenenfalls Mailbox und Gegenstelle (`imap.mailbox`, `net.peer`). Die
Collector-Konfiguration und Zugriffsrechte müssen die geltenden Regeln für
diese Daten umsetzen. Zugangsdaten gehören weder in Dashboards noch in
Exportdateien oder Betriebsberichte.

## Betriebsverantwortung und Abnahme

Die produktive Aktivierung gehört in die maßgeblichen Manifeste des
Kubernetes-Owner-Repos. Scrape-Annotationen in diesem Quellprojekt allein
beweisen keine Erfassung durch einen Prometheus-Server. Der Metrics-Owner
verantwortet die Anbindung an `metrics.roessner-net.de`, Datenquellen und das
Operator-Dashboard im Grafana-Ordner **Mail**.

Eine vollständige Abnahme umfasst Ready-Pods, aktuelle Prometheus-Serien je
erwartetem Target, einen tatsächlichen Service-Trace in Tempo sowie das
gespeicherte und gerenderte Dashboard. Ein lesender IMAP-Befehl kann den
Trace-Pfad ohne Testzustellung prüfen; bei Sampling sind mehrere Befehle
gegebenenfalls erforderlich. Fehlende Live-Belege bleiben ausdrücklich offen.

## Produktionsabnahme am 15.09.2026

- Namespace `imap`, Deployment `imap-server`: zwei Ready-Pods; beide zentralen
  Prometheus-Targets `up=1` nach dem abschließenden Rollout.
- Scrape-Labels: `job="kubernetes-pods"`, `namespace="imap"`,
  `app_kubernetes_io_name="imap-server"`, `pod`.
- OTLP: `http://192.168.0.102:4318`, `http/protobuf`, Sampling `0.1`.
- [Mail Operator in Grafana](https://metrics.roessner-net.de/d/imapcephpoc-mail-operator/imapcephpoc-c2b7-mail-operator):
  Version 2, 21 Panels einschließlich Abschnittsüberschriften und Interpretation;
  API-Readback, gerenderte Raten/Latenzen/LMTP-Panels und Logs-/Tempo-Links geprüft.
- Ein frischer echter Trace nach der endgültigen Aktivierung (13:58:38 UTC,
  `5e9e1e204e89112703b8ea9f284236f2`, `imap.STARTTLS`) wurde durch den
  Kubernetes-Owner unabhängig in Tempo geprüft: keine der fünf oben genannten
  Benutzer-/Mailbox-/Gegenstellenattribute. Vorheriger synthetischer Filtertest
  wurde von beiden Ownern bestätigt.
- CPU-/RAM-Serien sind im zentralen Prometheus derzeit nicht vorhanden.
  Das Dashboard weist die Lücke aus. Fehlende POP3-/Fehlerserien werden nicht
  künstlich auf Null gesetzt. Rate-Abfragen nutzen mindestens vier Minuten;
  dieses Fenster wurde gegen vorhandene Serien geprüft.

Beim ersten Filter-Rollout ersetzte die Aktivierung die Hostdatei atomar,
erzwang aber keinen neuen Collector-Container. Der weiterhin gesunde Collector
exportierte deshalb noch nicht gefilterte Traces. Der Service-Export wurde
zurückgenommen, der Collector gezielt neu erstellt und die Filterwirkung vor
der erneut ausdrücklich freigegebenen Aktivierung synthetisch geprüft.
Bereits gespeicherte ältere Traces werden dadurch nicht rückwirkend bereinigt.

Reproduzierbare Dashboard-Quelle im Metrics-Owner-Repo (seit 0.13.0):
`grafana/tools/build-shardpost-operator-dashboard.py` und
`grafana/dashboards/shardpost-operator.dashboard.json`, Dashboard-UID
`shardpost-mail-operator`
([Grafana](https://metrics.roessner-net.de/d/shardpost-mail-operator/shardpost-c2b7-mail-operator),
seit 18.09.2026; das alte `imapcephpoc-mail-operator` ist gesichert und entfernt). Die Abfragen decken alte und neue Labels ab
(`namespace` `imap|shardpost`, `service.name` `imapcephpoc|shardpost`).
Kubernetes-Owner-Vertrag: `shardpost/telemetry-patch.yaml` und
`reports/2026-09-15-imapcephpoc-observability.md`.


## Strukturierte Sitzungslogs ab 0.11.3

`slog` schreibt JSON nach stdout; Kubernetes verwendet `LOG_LEVEL=info`.
`connection_open`, `auth_success`/`auth_failed`, `tls_established` und
`connection_close` machen den Verbindungslebenszyklus sichtbar. `session_id`
verbindet die Ereignisse; `remote` ist die effektive PROXY-Client-Adresse.
Auch Director-Healthchecks erzeugen Verbindungslogs. Logs werden nicht durch
die OTLP-Samplingrate gefiltert.

Debug ergänzt bekannte Befehlsnamen und Laufzeiten ohne Argumente oder
Mailinhalte. Für die Fehlersuche mit einem Client gibt es ab 0.18.0 den
befristeten Protokoll-Trace pro Konto (`event=protocol_trace`, siehe
[Admin-API](admin-api.md#protokoll-trace-eines-kontos)). Details, Einschränkungen und Loki-Beispiel stehen in der
[Konfigurationsdokumentation](configuration.md), der datierte Nachweis im
[Logging-Bericht](reports/2026-09-16-logging.md).
