# Serverkonfiguration

Der Server lädt seine Konfiguration einmal vor dem Öffnen von Ceph, Authentifizierung und Listenern. Bibliotheken: Viper und go-playground/validator.

```sh
shardpost --config /etc/shardpost/server.yaml
shardpost --config /etc/shardpost/server.yaml config check
shardpost --config /etc/shardpost/server.yaml scrub
```

Alternativ bestimmt `SHARDPOST_CONFIG` den Dateipfad. `IMAP_CONFIG` bleibt
als Rückfall erhalten, sofern `SHARDPOST_CONFIG` nicht gesetzt ist. Ein
explizites `--config` hat Vorrang vor beiden Variablen. Ein ausdrücklich
leeres `SHARDPOST_CONFIG` deaktiviert den alten Rückfall. Priorität: eingebaute Standardwerte, explizite YAML-Datei, explizit gesetzte bisherige Env-Variablen. Es gibt keine implizite Dateisuche und keinen automatischen Reload. Kubernetes-Änderungen erfolgen per kontrolliertem Deployment-Rollout. Go-Runtime-Einstellungen wie `GOMEMLIMIT` bleiben Umgebungsvariablen; zusätzliche standardisierte OTEL-SDK-Optionen werden weiterhin vom SDK ausgewertet.

Die Abschnitte sind `server`, `ceph`, `rook`, `tls`, `nauthilus`, `smtp`, `sieve`, `scrub`, `logging`, `telemetry`, `imapsieve`, `extprograms`, `quota` und `admin`. Die vollständige Feld- und Env-Zuordnung steht in `internal/config/config.go`, `internal/config/imapsieve.go`, `internal/config/extprograms.go`, `internal/config/quota.go` und `internal/config/admin.go`; `extprograms`, die Lernregeln, Quota-Regeln und -Warnungen sowie Admin-Rollen und -Clients sind nur per YAML konfigurierbar (siehe `docs/imapsieve.md`, `docs/quota.md` und `docs/admin-api.md`).

```yaml
server:
  imap_addr: ':1143'
  imaps_addr: ':1993'
  pop3_addr: ':1110'
  pop3s_addr: ':1995'
  lmtp_addr: ':2424'
  http_addr: ':8080'
  auth_backend: nauthilus
  insecure_auth: false
  memory_budget_bytes: 268435456
ceph:
  user: shardpost
  index_pool: shardpost-index
  data_pool: shardpost-data
rook:
  namespace: rook-ceph
nauthilus:
  transport: grpc
  grpc_addr: nauthilus.auth.svc.cluster.local:9444
  oidc_client_secret_file: /run/secrets/nauthilus/client-secret
tls:
  cert: /tls/tls.crt
  key: /tls/tls.key
sieve:
  addr: ':14190'
  global_dir: /etc/sieve
telemetry:
  protocol: http/protobuf
  endpoint: http://collector:4318
  sample_ratio: 0.1
```

Unbekannte Schlüssel, falsch typisierte YAML-Werte, ungültige Env-Zahlen, ungültige Bool-Werte, widersprüchliche TLS-Paare und fehlende/leer gelesene explizite Secret-Dateien führen zu einem Startfehler. Env-Zahlen werden immer dezimal interpretiert. Secret-Dateien haben gegenüber dem dazugehörigen direkten Secret-Wert Vorrang. Fehlermeldungen drucken keine Werte.

`config check` validiert die Konfigurationsstruktur und liest referenzierte Secret-Dateien. Es ist kein Verbindungsnachweis für Ceph, Nauthilus, SMTP oder die externen Programme; Programmdateien werden beim Aufruf geprüft. Zertifikate werden beim anschließenden Serverstart geladen. Ohne Dateipfad bleiben die bisherigen Env-Deployments verwendbar.

## Client-IP hinter dem Director

`server.proxy_protocol_trusted_cidrs` steuert die Übernahme von Clientadressen
aus PROXY-Protocol v1/v2 vor dem eigentlichen Mailprotokoll/TLS. Dazu muss
auch der Director für die betreffenden Backends `haproxy.enabled: true`
setzen. Die Konfiguration gilt gemeinsam für IMAP, POP3, ManageSieve und LMTP.

Das produktive Overlay verwendet das nachgewiesene Calico-Podnetz
`172.16.0.0/12` zusammen mit `network-policy.yaml`. Mailports sind nur für
Director-Pods im Namespace `director` sowie die kontrollierten Testwerkzeuge
`mailtest`/`imaptest` im Namespace `shardpost` erreichbar. Diese Testwerkzeuge
gehören ausdrücklich zur Proxy-Vertrauensgrenze. HTTP-Health und Metriken
bleiben erreichbar. Bei Änderungen der Calico-Pools ist die CIDR-Liste
mitzuprüfen; einzelne dynamische Director-Pod-IP-Adressen werden nicht fest
hinterlegt.

Ohne PROXY-Präambel kann bei direkten serverseitig begrüßenden Verbindungen
bis zu fünf Sekunden Erkennungszeit entstehen. Der Director sendet deshalb
auch zum LMTP-Backend die Präambel. Direkte Operatorproben benötigen ein
entsprechendes Timeout oder senden selbst PROXY-Protocol.

## Strukturierte Betriebslogs

`slog` schreibt JSON nach stdout. `logging.level` (YAML) bzw. `LOG_LEVEL`
(Environment hat Vorrang) akzeptiert `debug`, `info`, `warn`, `error`; Standard
und Kubernetes-Rollout sind `info`. Ungültige Werte verhindern den Start.

- `info`: Verbindungsbeginn/-ende, Authentifizierungserfolg/-ablehnung,
  Zustellungen und Server-Lebenszyklus.
- `debug`: zusätzlich bekannte Befehlsnamen und Laufzeiten für IMAP, POP3
  und ManageSieve. `handler_error` bezeichnet einen Handlerfehler, nicht
  jeden negativen Protokollstatus. IDLE erscheint nach Befehlsende.
- `warn`: temporär gestörte Authentifizierung, Verbindungslimits und
  vorhandene Fallback-Meldungen.
- `error`: Speicherfehler, fehlgeschlagene Zustellungen und interne Fehler.

Sitzungen tragen `proto`, eine zufällige `session_id`, `remote` und `local`.
`remote` enthält die vom vertrauenswürdigen PROXY-Absender übermittelte
Client-Adresse; beim direkten Zugang die TCP-Gegenstelle. Erfolgreiche
Anmeldungen enthalten den kanonischen `user`. Die Sitzungs-ID bleibt bei
IMAP UNAUTHENTICATE gleich, die Benutzerbindung wird entfernt.
`connection_close` enthält die Sitzungsdauer in Millisekunden. Bei LMTP
beginnt die Backend-Sitzung nach LHLO. Das `tls`-Feld beschreibt TLS zum
Backend, nicht einen bereits am Director terminierten TLS-Kanal.

Auch Debug protokolliert keine Befehlsargumente, SASL-Daten, Passwörter,
Tokens, Skripttexte oder Mailinhalte. Unbekannte Befehle heißen `UNKNOWN`.
Die Logs enthalten betriebsnotwendige Client-IP-Adressen und Kontonamen;
diese Felder werden nicht als Metrik-Labels verwendet.

Beispiel für Loki (Client-IP durch die gesuchte öffentliche IP ersetzen):

```logql
{namespace="shardpost", app_kubernetes_io_name="shardpost-server"} | json | remote =~ "203\\.0\\.113\\.10:.*"
```

Für die weitere Korrelation nach `session_id` filtern. Bei `info` erzeugen
NOOP/FETCH/IDLE keine einzelnen Befehlslogs; Öffnen, Login und Schließen
bleiben sichtbar. Bereits vor dem Rollout geschlossene Verbindungen lassen
sich nachträglich nicht rekonstruieren. Leveländerungen erfordern Neustart.
