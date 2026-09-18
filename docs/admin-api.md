# Admin-API, shardpostctl und lokale Zustellung (LDA)

Vorgezogener Kern von [Stufe 8](development/admin-stage.md): versionierter
OpenAPI-Vertrag, Admin-Listener mit Identität und Rollen, Audit-Log und die
lokale Zustellung. Jobs, Plan/Apply und die Admin-Ansichten folgen in den
Teilpaketen 8a bis 8c.

## Vertrag und Generierung

- Vertrag: [`api/admin/openapi.yaml`](../api/admin/openapi.yaml), Pfad `/admin/v1`.
- Generiert mit `oapi-codegen` v2.8.0 (Modelle, `net/http`-Server-Interface,
  Client) nach `internal/adminapi/api.gen.go`; Konfiguration in
  `api/admin/oapi-codegen.yaml`. Handgeschriebene Client-Helfer für
  Socket und mTLS liegen in `internal/adminapi/client.go`.
- `make generate` erzeugt den Code neu, `make check-generate` schlägt fehl,
  wenn der eingecheckte Code nicht zum Vertrag passt. Der Generator läuft
  im Modulmodus (Netzzugriff), die Laufzeitbibliothek
  `github.com/oapi-codegen/runtime` ist vendort.

## Listener und Identität

Beide Listener sind ohne Konfiguration aus und von Mail- und Metrikports
getrennt. Wirkungslose Einstellungen werden abgelehnt: `socket` und
`socket_roles` nur gemeinsam, `addr` nur mit `tls_cert`, `tls_key`,
`client_ca` und mindestens einem Eintrag in `clients`, TLS-Einstellungen
nicht ohne `addr`.

| Listener | Identität | Rollen |
| --- | --- | --- |
| Unix-Socket (`admin.socket`, Modus 0600) | Peer-UID per `SO_PEERCRED`; nur die UID des Serverprozesses wird zugelassen | `admin.socket_roles` |
| TCP mit mTLS (`admin.addr`) | verifiziertes Clientzertifikat; Subject-CN oder DNS-/URI-SAN (ohne Groß-/Kleinschreibung) | `admin.clients[].roles` |

Der Socket ist nur innerhalb des Pods erreichbar, also dieselbe
Vertrauensgrenze wie `extprograms`. Netzzugehörigkeit allein berechtigt nie.
Für mTLS werden `admin.tls_cert`, `admin.tls_key` und `admin.client_ca`
bei Dateiänderung neu gelesen (höchstens alle 10 s geprüft). Damit greift
eine Rotation durch cert-manager ohne Neustart. Eine defekte neue Version
wird geloggt, bis dahin bleibt die alte in Gebrauch. Clientzertifikate
brauchen `extKeyUsage clientAuth`.

Rollen: `diagnose` (Version), `deliver` (Zustellung mit Quota-Durchsetzung),
`deliver:noenforce` (zusätzlich für `enforce_quota=false`).

```yaml
admin:
  socket: /run/shardpost/admin.sock
  socket_roles: [diagnose, deliver, "deliver:noenforce"]
  addr: ":9443"                     # optional
  tls_cert: /admin-tls/tls.crt
  tls_key: /admin-tls/tls.key
  client_ca: /admin-tls/ca.crt
  clients:
    - identity: ops.shardpost.internal
      roles: [diagnose]
```

Umgebung: `ADMIN_SOCKET`, `ADMIN_ADDR`, `ADMIN_TLS_CERT`, `ADMIN_TLS_KEY`,
`ADMIN_CLIENT_CA`; Rollen und Clients nur per YAML.

Jede Anfrage erzeugt eine Logzeile `admin request` (`event=admin_request`)
mit Principal, Transport, Methode, Pfad, Status, Fehlercode, Dauer und den
fachlichen Feldern der Operation (bei der Zustellung Empfänger, Absender,
Konto, Optionen, Anzahl gespeicherter Kopien), aber ohne Nachrichteninhalt
oder Secrets. Abgelehnte mTLS-Clients erscheinen mit der vorgelegten
Zertifikatsidentität, abgelehnte Socket-Aufrufer mit ihrer UID.
Metrik: `shardpost_admin_requests_total{operation,code}`.

## Lokale Zustellung

`POST /admin/v1/deliveries` stellt eine Nachricht (`message/rfc822`) für einen
Empfänger zu, wie `dovecot-lda`. Die Empfängerauflösung läuft wie bei LMTP
(Aliase, kanonisches Konto, Userdb-Attribute). Sieve läuft standardmäßig,
inklusive Redirect, Vacation und Notify. LMTP und LDA teilen dieselbe
Zustellung (`internal/delivery`). Vor die Nachricht setzt der Server
`Received: from <transport> <principal> by <lmtp_hostname> with LDA`.
Empfänger und Absender dürfen keine Steuer- oder Leerzeichen und keine
weiteren spitzen Klammern enthalten. Auflösung (10 s) und Zustellung
(5 min) laufen vom HTTP-Request entkoppelt: Bricht der Client ab, wird
die Zustellung trotzdem vollständig ausgeführt.

| Parameter | Bedeutung |
| --- | --- |
| `recipient` | Empfänger (Adresse oder Konto), Pflicht |
| `from` | Envelope-Absender, leer = Null-Absender |
| `mailbox` | direkt in dieses vorhandene Postfach, ohne Sieve |
| `sieve=false` | ohne Benutzerskript nach INBOX |
| `enforce_quota=false` | auch über dem Limit speichern (Nutzung wird gezählt); Rolle `deliver:noenforce` |

Fehlercodes: `bad_request` 400, `unauthenticated` 401, `forbidden` 403,
`no_such_user`/`no_such_mailbox` 404, `too_large` 413, `rejected` 422
(Sieve-Reject), `over_quota` 507, `temporary` 503, `internal` 500.

## shardpostctl

Im Image unter `/usr/local/bin/shardpostctl`. Standard ist der Socket
`/run/shardpost/admin.sock` (`-socket`, `SHARDPOST_ADMIN_SOCKET`). Mit
`-url https://host:port -cert -key -ca` (oder `SHARDPOST_ADMIN_*`) geht es
über mTLS.

```sh
shardpostctl version
shardpostctl deliver -d user@example.org -f postmaster@example.org < mail.eml
shardpostctl deliver -d user@example.org --no-enforce-quota --json < warning.eml
```

Exitcodes von `deliver` (sysexits): 0 ok, 64 Aufruf, 67 unbekannter Empfänger
oder Postfach, 69 abgelehnt oder zu groß, 70 unerwartete Antwort, 73 über
Quota, 75 temporär (erneut versuchen), 77 keine Berechtigung.
