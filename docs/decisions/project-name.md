# ShardPost: Namensentscheidung und Migration

Status: **ShardPost gewählt und Refactoring/Rollout freigegeben**, 16.09.2026.
Erstes Release unter dem neuen Namen: **0.12.0**. Seit **0.13.0** (18.09.2026)
tragen auch alle Betriebs- und externen Bezeichner den Namen.

ShardPost bezeichnet die verteilte Mailbox- und Verarbeitungsschicht über
mehrere Protokolle. Der frühere Name `imapcephpoc` ist nur noch ein historischer
bzw. ausdrücklich kompatibler Bezeichner. Die Umbenennung ist keine Migration
der Nachrichten oder ihrer Speicherformate.

## Umgestellte Produktbezeichner

| Bereich | Neuer Name |
|---|---|
| Produkt / Protokoll-Identität | ShardPost |
| Go-Modul und eigene Imports | `shardpost` / `shardpost/internal/...` |
| Serverquelle / Binary | `cmd/shardpost` / `/usr/local/bin/shardpost` |
| Lastgenerator | `cmd/shardpost-load` / `/usr/local/bin/shardpost-load` |
| Serverimage | `docker.io/chrroessner/shardpost:0.12.0` |
| Toolsimage | `docker.io/chrroessner/shardpost-tools:0.12.0` |
| Konfigurationsauswahl | `SHARDPOST_CONFIG` oder `--config` |
| Konfigurationsmount im Betriebsprofil | `/etc/shardpost/server.yaml` |
| Externe Programme | `/usr/local/lib/shardpost/programs` |
| OTel-Standard für neue Installationen | `service.name=shardpost`, Instrumentierungsscope `shardpost` |

README, Entwicklerhandbuch, Architekturdiagramme, Buildanweisungen,
Testskripte und Release-Manifeste verwenden die neuen Namen. Historische
Nachweise behalten damalige Images, Digests und URLs. Das lokale
Arbeitsverzeichnis heißt `shardpost`.

## Übergangskompatibilität

Die Images 0.12.x enthielten Symlinks `imapcephpoc -> shardpost`,
`imapload -> shardpost-load` und
`/usr/local/lib/imapcephpoc -> /usr/local/lib/shardpost`. Sie sind seit dem
18.09.2026 entfernt: alle Manifeste und die aktive ConfigMap verwenden
`/usr/local/bin/shardpost` und `/usr/local/lib/shardpost/programs`. Ein
Rollback auf eine ConfigMap aus der Zeit vor 0.12.0 muss deshalb zusammen mit
dem damaligen Image erfolgen (wie im Abschnitt „Rollout und Rückweg“ ohnehin
vorgesehen). Der Arbeitsordner heißt seit demselben Tag ebenfalls
`shardpost`.

Konfigurationspfad-Priorität: explizites `--config`, danach ein gesetztes
`SHARDPOST_CONFIG`, sonst `IMAP_CONFIG`. Ein bewusst leer gesetztes
`SHARDPOST_CONFIG` bedeutet Defaults ohne Dateipfad und verhindert den
Rückfall auf die alte Variable. Protokollbezogene Einstellungen wie
`IMAP_ADDR`, `POP3_ADDR` und `LMTP_ADDR` behalten ihre sachlich passenden Namen.

Bis 0.12.x blieben die Kubernetes-Namen (`imap`, `imap-server`, `imap-scrub`,
Service `imap`), RADOS-Pools, Secrets, Zertifikate und Director-/Nauthilus-
Verträge erhalten. Mit 0.13.0 (18.09.2026) wurden sie mitgezogen: Namespace
`shardpost`, Deployment `shardpost-server`, Service `shardpost`, Pools
`shardpost-index`/`shardpost-data` (Kopie mit `hack/pool-copy.py`,
objektweise verifiziert), cephx-Client `shardpost`, Director-Shard
`shardpost`, OIDC-Client `shardpost-service`, LDAP-Health-Konto
`mail_health_shardpost` und die SOPS-Datei `shardpost.sops.yaml` der
Testkonten. Unverändert bleiben nur die Logins der Testkonten und die
Kontonamespaces in RADOS. Ablauf und Nachweise stehen im Bericht
[2026-09-18-shardpost-0.13.0](../reports/2026-09-18-shardpost-0.13.0.md).

Die Metrikfamilien `imapceph_*` und `imap_*` bleiben als bestehende
Monitoring-API stabil. `telemetry.service_name` ist seit 0.13.0 auch im
Betriebsprofil `shardpost`; `OTEL_SERVICE_NAME` kann ihn überschreiben. Der Produktname in den Betriebslogs lautet `shardpost`.

## Rollout und Rückweg

Neue Images werden mit Digests gepinnt. Für das Serverdeployment werden
Image, Konfigurationsmount und Env-Auswahl gemeinsam umgestellt. Die neue
ConfigMap unterscheidet sich fachlich nur durch Helperpfad und expliziten
Telemetrie-Kompatibilitätsnamen. Der Scrub-CronJob und das Toolsdeployment
wechseln auf ihre jeweiligen neuen Images.

Die bisherige ConfigMap und die vorherigen Manifeste bleiben für den Rollback
verfügbar. Ein Rückweg stellt gemeinsam altes Image, alten Mount und alte
Env-Auswahl wieder her; allein das alte Binary vor einer neuen Konfiguration
reicht wegen des neuen Feldes `telemetry.service_name` nicht aus. Poolnamen,
Objektformate und Lernjob-Actions wurden nicht verändert.

Prüfung: vollständiger Linux-Build/Race-Suite, alte/neue CLI-Konfiguration,
Protokollidentität, unveränderte Monitoring-Verträge und reale Anmeldung
beider Testkonten über IMAP und ManageSieve. Der konkrete produktive
Nachweis steht im [Releasebericht](../reports/2026-09-16-shardpost.md).
