# Betrieb und Releases

[Dokumentationsindex](../README.md) · [Konfiguration](../configuration.md)

## Abhängigkeiten und Verantwortungsgrenzen

Das aktive Profil ist `deploy/overlays/croessner-nauthilus`; Namespace und
Deployment heißen seit 0.13.0 `shardpost` und `shardpost-server`. Das Profil erwartet Rook-
Pools und cephx-Zugang, cert-manager/TLS, Nauthilus-Backchannel-Zugang,
Director-Backend-Zugang und die konfigurierten Lern-/Relay-Dienste.

Die Secrets werden durch die zuständigen Infrastrukturverfahren bereitgestellt.
Der Server liest Rook-Zugang mit begrenztem RBAC. Eine Umbenennung der
Anwendung verändert Pools, Zertifikatsnamen und Director-Routen nur zusammen
mit einer Datenkopie; siehe [Namensmigration](../decisions/project-name.md).

Das Basisoverlay verwendet statische Testzugänge. `make deploy` wählt das
`croessner-nauthilus`-Betriebsprofil. Auch damit das beabsichtigte Ziel vor
einer Anwendung prüfen. `make undeploy` löscht auch Rook-Ressourcen
und darf nicht als Rollback verwendet werden.

## Lesende Erstdiagnose

```bash
kubectl --context croessner -n shardpost get pods -l app.kubernetes.io/name=shardpost-server
kubectl --context croessner -n shardpost rollout status deployment/shardpost-server --timeout=30s
kubectl --context croessner -n shardpost logs -l app.kubernetes.io/name=shardpost-server --since=15m --tail=500
kubectl --context croessner -n shardpost get deployment shardpost-server \
  -o jsonpath='{.spec.template.spec.containers[0].image}'
```

Keine ganzen Secrets oder Environment-Dumps für Berichte ausgeben. Client-IP,
Benutzer und Mailbox können in Betriebslogs personenbezogen sein. Für die
Weitergabe nur die benötigten Ereignisse und gegebenenfalls pseudonymisierte
Korrelationsdaten verwenden.

| Symptom | Zuerst unterscheiden |
|---|---|
| Keine Clientverbindung im Log | Level, richtiger Pod/Zeitraum, Director-Routing, neue gegenüber schon laufender Sitzung |
| Nur Director-/Pod-IP | PROXY-Ausgabe, vertrauenswürdige CIDRs und tatsächlicher Zugangspfad |
| Auth fehlgeschlagen | `auth_failed` gegenüber temporärem Backend-/OIDC-Fehler |
| Ready, aber Mailfunktion gestört | Readiness prüft Ceph, nicht den gesamten Auth-/Sieve-Pfad |
| IDLE aktualisiert spät | Watch/Notify, Journal-/MODSEQ-Synchronisierung, Polling |
| Lernen verzögert | Prepared/Commit-Beleg, Worker-Lease, Budget, Retry, Dead, externer Helper |
| Speichern scheitert | Quota, Ceph-Latenz/Fehler, Intents, RADOS- und Backendmetriken |

Bei `info` werden neue Verbindungen, Anmeldung, TLS-Aufbau und Schließen
sichtbar. Regelmäßige Director-Healthchecks erzeugen ebenfalls Lebenszykluslogs.
`debug` ergänzt Befehlsnamen und Laufzeiten; ein Rollout ist für Leveländerungen
nötig. `remote` zeigt den effektiven Client, `session_id` verbindet Ereignisse.

## Releaseablauf

1. Ausgangsimage samt Digest, Konfiguration, Replikas und offene Betriebs-
   auffälligkeiten erfassen. Rückweg für Image und gegebenenfalls ConfigMap sichern.
2. Geänderte Pfade, passende Race-/Unit-Tests und den vollständigen Linux-
   Build prüfen. Bei Datenmodelländerungen zusätzlich echte Ceph-Abbruchtests.
3. Neues unveränderliches Versionstag bauen. Runtime-`config check` ausführen.
   Veröffentlichungsziel und Rollout müssen zum autorisierten Umfang gehören.
4. Image veröffentlichen und Registry-Digest nachlesen. Das Deployment auf
   `tag@sha256:...` festlegen; das Release-Overlay mitführen.
5. Image und beabsichtigte Einstellungen in einem kontrollierten Update ändern.
   Bei konkurrierenden Änderungen abbrechen bzw. neu vergleichen. Nicht blind
   ein großes Overlay über einen unbekannten Live-Stand anwenden.
6. Rolling Update abwarten; Ready, Neustarts und tatsächliche `imageID` jeder
   Replika prüfen. Der Scrub-CronJob soll zum freigegebenen Serverstand passen.
7. Den betroffenen Benutzerpfad testen. Bei gemeinsamem Director-/Auth-Pfad
   beide vorgesehenen Testkonten und relevanten Protokolle prüfen. SMTP-
   Testzustellungen sind ein eigener Seiteneffekt.
8. Logs/Metriken und gegebenenfalls Traces prüfen, temporäre Ressourcen entfernen,
   Ergebnis und verbleibende Lücken dokumentieren.

Beim Logging-Release wurde nur Image und `LOG_LEVEL` am Pod-Spec verändert;
ConfigMap, Pools und Director blieben unverändert. Das ist ein Beispiel für
einen begrenzten Release, kein universeller Migrationsablauf.

## Rollback und Datenkompatibilität

Bei einer reinen Code-/Logging-Änderung lässt sich das vorherige Digest mit
den gesicherten Einstellungen wiederherstellen. Ein blindes `rollout undo`
ist nur geeignet, wenn die ausgewählte Revision tatsächlich den gewünschten
vollständigen Zustand enthält. CronJob und deklaratives Overlay gehören
zum Rückweg, damit eine spätere Anwendung nicht wieder vorwärts rollt.

Bei Änderungen an persistenten Formaten, Programmnamen gespeicherter Jobs,
Chunkgröße oder Indexzuordnung reicht ein altes Binary nicht als Rückweg.
Vorher eine Kompatibilitätsstrategie für alte und neue Writer festlegen.
Pools und Namespaces beim Rebranding nicht umbenennen oder neu anlegen.

PROXY-Rollback benötigt Reihenfolge: erst Director-Ausgabe deaktivieren und
deren Rollout abschließen, dann Receiver-Vertrauen entfernen. Andernfalls
kann der Header als Protokollbefehl beim Server landen.

## Scrub, Sicherung und Beobachtbarkeit

Scrub kann Daten verändern. Für eine Untersuchung zuerst Dry-run und Umfang
festlegen. Er repariert definierte Inkonsistenzen, rekonstruiert aber keine
verlorenen Mailinhalte. Ein Backup-/Restore-Nachweis muss beide Pools und
zugehörige dauerhafte Zustände berücksichtigen.

Prometheus, Loki und Tempo haben unterschiedliche Aussagen. Eine fehlende
Serie ist nicht Null; ein gesampelter Trace ist nicht jede Verbindung.
Der Server setzt teilweise Benutzer-/Mailbox-/Peer-Attribute in Spans;
die dokumentierte Redaktion liegt im externen Collector. Diese darf beim
Umbau der Telemetrie nicht als automatisch serverseitig gegeben angenommen
werden. Siehe [Observability-Vertrag](../observability.md).
