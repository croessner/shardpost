# ShardPost 0.12.0

Status: **ShardPost 0.12.0 veröffentlicht, ausgerollt und produktiv geprüft.**

Der Benutzer hat ShardPost als Produktnamen gewählt und Refactoring,
Imageveröffentlichung sowie produktiven Rollout freigegeben. Der vorherige
produktive Stand war imapcephpoc 0.11.3.

Umgestellt: eigenes Go-Modul und Imports, Server/Lastgenerator, Imageziele,
Protokoll-Identitäten, Config-Variable und -Mount, Helperpfad, Buildanweisungen,
Dokumentation und Diagramme. Kompatibilitätsaufrufe und bestehende Daten-/
Monitoring-/Routingverträge sind in der [Namensentscheidung](../decisions/project-name.md)
aufgeführt. Es findet keine RADOS-Datenmigration statt.

Nachweise werden nach dem Rollout ergänzt. Ausgangssicherungen:
`/private/tmp/shardpost-*-before.json` und
`/private/tmp/shardpost-source-before.tar.gz`.

## Lokale Prüfung

- Vollständiger Linux-Build mit `go vet` und `go test -p 1 -race -tags tentacle ./...` bestanden.
- Zusätzliche gezielte Race-Tests aller betroffenen cgo-freien Pakete bestanden.
- Neues `SHARDPOST_CONFIG`, alter `IMAP_CONFIG`-Rückfall und CLI-Vorrang getestet.
- Runtime-`config check` für neue und alte Binary-Aufrufe bestanden.
- IMAP ID liefert ShardPost; Runtime-Helper unter neuem Pfad ausgeführt,
  alter/neuer Helperinhalt identisch, unveränderte sichere Eigentümer-/Dateimodi.
- Toolsimage: swaks und curl lauffähig.
- Neue ConfigMap gegen Live-Inhalt verglichen: ausschließlich Helperpfad und
  expliziter alter OTel-Servicebezeichner unterscheiden sich.
- Beide Ziel-Tags vor Veröffentlichung nicht vorhanden.
- Diagramme neu gerendert; keine gebrochenen lokalen Markdown-Verweise.

Nachweise: `/private/tmp/shardpost-linux-tests.log`,
`/private/tmp/shardpost-build.log`, `/private/tmp/shardpost-config-next.json`.

## Veröffentlichung und produktiver Nachweis

- Server: `docker.io/chrroessner/shardpost:0.12.0@sha256:acb70171a0272f0a7e47a6da2b6d61d73b3348522f76f1a2cc69072e7c07e2e7`.
- Tools: `docker.io/chrroessner/shardpost-tools:0.12.0@sha256:45949884ff8fb6ca53ea2db173f07866147905e8ab341d6777b060fffe650049`.
- Neue ConfigMap: `imap-server-config-4f7c9bk44f`; vorherige Map bleibt erhalten.
- Beide Serverreplikas Ready, null Neustarts, tatsächliche imageIDs entsprechen dem neuen Digest.
- `shardpost config check` gegen die tatsächlich gemountete Konfiguration auf beiden Pods erfolgreich.
- Beide Metrik-Endpunkte über das neue Toolsdeployment abgefragt; bestehende Metrikfamilien vorhanden.
- Toolsdeployment erfolgreich gerollt; Scrub-CronJob auf denselben Serverdigest gestellt.
- Beide Testkonten öffentlich über IMAP und ManageSieve geprüft: TLS, Login,
  ShardPost-Produktidentität, NOOP und Logout erfolgreich.
- Vollständige Sitzungslogs einschließlich öffentlicher PROXY-Client-IP und
  `service=shardpost`. Im Abnahmefenster 202 Info-Zeilen, keine Debug-/Warn-/Error-Zeilen.
- Vollständiger Pod-Spec-Vergleich: Server nur Image, Config-Env, Mount und
  ConfigMap-Verweis geändert. Tools und CronJob ausschließlich Image geändert.
- Pools, Namespaces, Zugangsdaten, TLS, Netzwerkregeln und gespeicherte
  Lernjob-Actions unverändert. Keine Testmail oder externe Zustellung erzeugt.

Die automatische Freigabeprüfung verlangte eine zusätzliche Bestätigung der
beiden konkreten Docker-Hub-Ziele; diese wurde erteilt, danach beide Uploads
und Registry-Readbacks erfolgreich abgeschlossen.

## Rückweg

Vorheriges Serverimage:
`docker.io/chrroessner/imapcephpoc:0.11.3@sha256:b32270400f58ccb976af35d879fde7f25a78a4a53ad3bb83dab5519027d9f982`.
Die Sicherungen `shardpost-deployment-rollout-before.json`,
`shardpost-cronjob-rollout-before.json` und `shardpost-tools-rollout-before.json`
unter `/private/tmp` enthalten den unmittelbar vor dem jeweiligen Patch
geprüften Stand. Beim Server müssen Image, frühere ConfigMap, Mount und
IMAP_CONFIG gemeinsam zurückgestellt werden. Datenmigration ist nicht nötig.
Die deklarativen Release-Manifeste müssen bei einem Rückweg ebenfalls angepasst werden.

## Grenzen

Der Umbau hat die vollständige Server-Race-Suite, aber nicht erneut die ganze
historische Pigeonhole-/imaptest- oder Lastsuite ausgeführt. Deren dokumentierte
Ausnahmen bleiben bestehen. Der Collector und die Grafana-Dashboards wurden
nicht umbenannt; ihr Service-/Metrikvertrag bleibt ausdrücklich kompatibel.
Der Workspace-Ordner behält seinen bisherigen Pfad. Ohne Git-Metadaten sind
keine Git-Commits oder Tags erzeugt worden.
