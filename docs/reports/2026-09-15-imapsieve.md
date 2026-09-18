# Viper-Konfiguration und IMAPSieve

## Ergebnis

Am 15.09.2026 produktiv ausgerollt: **0.10.1**, zwei gesunde Serverreplikas.
Image: `docker.io/chrroessner/imapcephpoc:0.10.1@sha256:f9bdea21100a3bcd76a1e8de40fec52f4e70109b2e6770247307c79e8a25574b`.

- Typisierte Viper-Konfiguration mit validator/v10, strikter YAML-/Env-Konvertierung, Secret-Dateien und `config check`. Bisherige Env-Namen bleiben Overrides.
- IMAPSieve für APPEND, COPY/MOVE und echte Flagänderungen einschließlich FETCH/Seen. Zuordnung über METADATA, begrenzte Ausführung, ACL-Prüfung und getrennte Zustellaktivierung.
- Konfigurierbare Rspamd-Regeln mit dauerhafter Ceph-Lernqueue, unabhängigen Payloads, atomaren Commitnachweisen, Retry und erhaltenen Fehleraufträgen.
- YAML: `deploy/overlays/croessner-nauthilus/server.yaml`; bestehende Secret-Verweise erhalten. Aktive ConfigMap: `imap/imap-server-config-8dhg8g7h7k`.
- ManageSieve-Capability: `sieve://mail.roessner-net.de:4190`, über den bestehenden Director für beide Testaccounts geprüft.
- Rspamd: separater Lernzugang `192.168.0.2:11336`, Worker-Quelladressen, eigener Token, exakt POST /learnspam und /learnham, Header-Allowlist und 4-MiB-Limit.

## Abnahme

| Prüfung | Ergebnis |
| --- | --- |
| Gesamtes Serverprojekt unter Linux: `go test -race -tags tentacle ./...` | bestanden |
| Docker-Build einschließlich `go vet` | bestanden |
| Kubernetes-YAML mit gebautem Image: `config check` | bestanden mit isolierten Test-Secrets |
| OTLP-HTTP-Integrationstest mit tatsächlichem Export | bestanden, einschließlich Basispfad |
| Neue Sieve-IMAPSieve-Fixture | bestanden |
| Pigeonhole-Konformität | 117 bestanden, 31 fehlgeschlagen von 148; bestehende Abdeckungslücken |
| Separater Fork: `go test ./...` | TestTestAddress scheitert an fehlender `test-address.svtest`; übrige getestete Pakete bestanden |
| Mailstack Stack-/Secret-/Render-/Runtime-Verträge | bestanden |
| HAProxy 3.2.19 Syntax und isolierte Zugriffstests | bestanden; Body unverändert, Token entfernt |
| Native Kandidatenabnahme mit beiden Testaccounts | APPEND, FETCH/Seen, MOVE nach Junk und zurück bestanden; 4 Lernaufträge bestätigt |
| Native öffentliche Abnahme über Director, beide Testaccounts | bestanden |
| Öffentliche Abnahme über Ceph-Shardgrenze, beide Testaccounts | bestanden; Ziel-UID 21 bei Shardgröße 20 |
| Lernqueue regulärer Server nach öffentlichen Abnahmen | 8 vorbereitet, 8 bestätigt; zusätzlich zuvor 4 im Kandidaten bestätigt |
| Bestehende Rspamd-Adminports von Node1 | 11334 und 11335 jeweils HTTP 200 |
| Unerlaubte Zugriffe auf Lernport | Adminpfad, falscher Token und Query jeweils HTTP 403 |
| Postflight | beide Server gesund mit identischem Digest; Primary/Shadow Rspamd gesund; 0 IMAPSieve- und 0 OTLP-Exportfehler im Prüfzeitraum |

Der native Test `hack/imapsieve-live-test.py` legt eindeutige Testordner und ein unaktiviertes Skript an und entfernt sie danach. Keine SMTP-Nachrichten wurden versandt. `IMAP_TEST_SHARD_BOUNDARY=20` prüft höhere Indexshards.

Die vollständige imaptest-Suite wurde zweimal gegen den Kandidaten gestartet. Beide Läufe brachen an `NO [UNAVAILABLE]` durch den bereits dokumentierten Nauthilus-Tempfail ab. Die korrelierten Testanfragen zeigten `authn=true`, `authz=true` und `director_routing:ok`, danach `authenticated=tempfail`. Diese Läufe sind **nicht bestanden**. Zusätzlich bleibt der bekannte `fetch-envelope`-Unterschied bei veralteten Source-Routes sichtbar. Es wurden keine Authentifizierungsprüfungen abgeschwächt.

## Review- und Fixrunden

Mehrere unabhängige Runden bis zum Abschluss ohne weitere P1/P2-Befunde. Korrigiert wurden unter anderem strikte Konvertierung, leere Skripte, dauerhafte Zielkopien vor Deleted, mehrdeutige Speicherfehler, Queue-Fairness, DELETE/Link-Konkurrenz, Sperrverluste und SETACTIVE-Trennung.

Die Kandidatenabnahme fand zusätzlich den fehlenden Signalpfad des HTTP-Tracing-Exporters. Version 0.10.1 ergänzt `/v1/traces` zur OTLP-Basis-URL; ein echter Exporttest schützt das Verhalten. 0.10.0 bleibt ein bereits veröffentlichtes, nicht produktiv eingesetztes Vorgängerimage.

Die ConfigMap-Namespace-Angabe ist ausschließlich am Generator gesetzt. Der unabhängige vollständige Rendercheck bestätigt alle 17 Ressourcen einschließlich Ceph/RBAC in `rook-ceph`, ServiceAccount-Zuordnung, Selektoren und Secret-/Zertifikatsreferenzen. Produktiv angewandt wurden nur Server-Deployment, ConfigMap und Scrub-CronJob; Ceph/RBAC wurden nicht verändert.

## Rollout und Grenzen

Der Benutzer hat Docker-Hub-Push und vollständigen Rollout trotz des ungenutzten Backup-Wartungsfensters ausdrücklich freigegeben. Der Proxy wurde gezielt neu erstellt; Rspamd und andere Maildienste wurden nicht neu gestartet. Temporäre Kandidaten-, Testjob- und überzählige ConfigMap-Ressourcen sowie lokale Kopien des Lerntokens wurden entfernt.

Extprograms bleiben zurückgestellt. IMAP-fileinto benötigt eine vorhandene Zielmailbox. Fehleraufträge bleiben für Betreiberbearbeitung gespeichert. Transport ist mindestens einmalig, keine verteilte Exactly-once-Garantie. Die oben genannten Konformitäts- und Nauthilus-Testlücken bleiben offen.

## Rollback

Deployment-Ausgangszustand: `/private/tmp/imap-server-before.json`, vorherige Version 0.9.0; zusätzlich bestehende Kubernetes-Deployment-Historie. Quell-Ausgangskopie: `/private/tmp/imapcephpoc-before-imapsieve.tgz`.

Proxy-Sicherung auf dem Mailhost: ursprüngliche Quelle unter `/srv/docker/mailstack/temp/imapsieve-rollback-20260915T184447/source.yaml`; ursprüngliche gerenderte Compose-Datei und Render-Env unter `/srv/docker/mailstack/temp/imapsieve-rollback-20260915T184744/`. Nur den Proxy gezielt mit wiederhergestellter Compose-Konfiguration neu erstellen. Keine Mail-, Rspamd- oder Archivdaten löschen.

Das Workspace-Verzeichnis besitzt kein `.git`; es wurde kein Commit/Tag erzeugt. Das Kubernetes-Overlay referenziert das veröffentlichte Image per Digest.
