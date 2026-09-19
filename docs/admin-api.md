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

Rollen: `diagnose` (Version, Serverstatus und lesende Diagnose), `deliver` (Zustellung mit Quota-Durchsetzung),
`deliver:noenforce` (zusätzlich für `enforce_quota=false`), `import`
(Kontoimport), `trace` (Protokoll-Trace eines Kontos, siehe unten),
`provision` (Postfächer anlegen, umbenennen, abonnieren; ACLs ändern),
`repair` (Wartungsjobs, die reparieren; Lernjobs wiederholen), `delete`
(Postfach, Nachrichten und Konto endgültig löschen), `content`
(Nachrichteninhalte lesen: Suche, Kopfzeilen, Export, Sieve-Skripte) und
`support` (Flags, Kopieren/Verschieben, Sieve-Skripte ändern und
aktivieren). `content` ist bewusst von `diagnose` getrennt: Diagnose sieht
Metadaten, aber keine Inhalte.

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
    - identity: support.example.org
      roles: [diagnose, trace]
      accounts: ["*@example.org"]      # optional: Konten-Scope
```

`clients[].accounts` begrenzt die Konten, die ein mTLS-Client ansprechen
darf (`path.Match`-Muster auf das kleingeschriebene Konto; leer = alle).
Der Scope gilt für jede kontobezogene Operation: Diagnose, Trace, Import,
Zustellung (auf das aufgelöste Konto), bei `account show` zusätzlich für das
kanonische Konto eines Alias und bei `acl explain` für den erklärten
Principal. Clients mit Scope dürfen Konten nur kleingeschrieben ansprechen
(RADOS-Namespaces unterscheiden Groß- und Kleinschreibung). Außerhalb des
Scopes antwortet die API `403 forbidden`.
Aufrufer am Unix-Socket sind nicht begrenzt.

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

## Lesende Diagnose (Stufe 8a)

Alle Operationen lesen nur (Rolle `diagnose`). Ein Test führt jede davon
gegen einen Store aus, der bei jeder Schreiboperation fehlschlägt.

```sh
shardpostctl server status                   # Version, Pod, Start, Config-Fingerprint, Store
shardpostctl account show user@example.org   # Nauthilus-Auflösung, Store, Quota, Importsperre, Trace
shardpostctl mailbox list [-limit 100] [-after NAME] [-all] user@example.org
shardpostctl mailbox show user@example.org INBOX            # Name oder Mailbox-ID
shardpostctl acl list user@example.org Shared
shardpostctl acl explain user@example.org Shared kollege@example.org
shardpostctl quota show user@example.org
shardpostctl shared list kollege@example.org
```

Jeder Befehl kennt `--json`; Flags dürfen auch hinter den Argumenten stehen.
Exitcodes wie bei `deliver`: 67 unbekanntes Konto/Postfach, 64 Aufruf (auch
Alias statt kanonischem Konto), 77 fehlende Rolle oder Scope.

| API | Inhalt | Kosten |
| --- | --- | --- |
| `GET /admin/v1/server` | Version, Instanz (Pod), Startzeit, `config_fingerprint`, Store-Ping | ein Ping |
| `GET /admin/v1/accounts/{account}` | Nauthilus-Lookup (`canonical`, `alias` mit kanonischem Konto, `unknown`, `unavailable`), Store vorhanden, Anzahl Postfächer, Quota, Importsperre, Trace | Registry, Quota |
| `GET …/mailboxes?after=&limit=&name=` | Postfächer in Namensreihenfolge: ID, UIDVALIDITY, UIDNEXT, HIGHESTMODSEQ, Shard-Größe, Abo, Special-Use, Zahl der ACL-Einträge; `next` für die Folgeseite | ein Registry-Ausschnitt, ein Header je Postfach |
| `GET …/mailboxes/{id}` | zusätzlich Nachrichten, ungelesen, gelöscht markiert, Größe, Shards, Log-Beginn (QRESYNC-Horizont), Keywords | liest alle Index-Shards des Postfachs |
| `GET …/mailboxes/{id}/acl` | explizite Einträge und ACL-Generation (der Eigentümer hat implizit alle Rechte) | Registry |
| `GET …/mailboxes/{id}/acl/explain?principal=` | Rechte des Principals (dieselbe Funktion wie IMAP), Zeiger in seiner Registry und Befunde im Klartext | zwei Registries |
| `GET …/quota` | Belegung und Grenze (0 = unbegrenzt) | Quota-Zähler |
| `GET …/shared` | Freigabe-Zeiger des Kontos (`Other Users/…`) | Registry |

`account show` akzeptiert auch Aliase und nennt dann das kanonische Konto;
die Store-Felder beschreiben dann den (meist leeren) Namespace unter dem
angegebenen Namen. Alle anderen Operationen verlangen das kanonische Konto,
weil die Daten darunter liegen. Postfächer werden in der URL über ihre
stabile ID adressiert. `shardpostctl` löst Namen selbst auf (`?name=`, ein
Registry-Schlüssel); die Auflösung einer ID liest die ganze Registry des
Kontos. Eine Seite endet nach `limit` gelesenen Registry-Einträgen, auch
unlesbaren; ein defekter Eintrag verschiebt oder beendet die Liste nicht.

`acl explain` beantwortet „Warum sieht X die Freigabe nicht?“ mit Befunden:

- Alias statt kanonischem Konto; ein ACL-Eintrag unter dem Alias greift nie.
- Keine Rechte, also existiert das Postfach für den Principal nicht (`NONEXISTENT`).
- Rechte ohne `l`: nicht gelistet, aber per Name ansprechbar.
- Rechte nur über `anyone`: Dafür werden keine Zeiger geschrieben, LIST
  zeigt das Postfach nicht.
- Fehlender Zeiger (LIST zeigt das Postfach nicht; ein SETACL für den
  Principal schreibt ihn) oder Zeiger ohne Recht `l` (LIST blendet aus, der
  Zeiger bleibt bis zum Löschen des Postfachs oder bis ein SETACL ihn
  zurückzieht). Die Generation des Zeigers ist nur Information: SETACLs für
  andere Identifier erhöhen die Generation des Postfachs, ohne ihn zu
  berühren.

`config_fingerprint` ist ein SHA-256 über die wirksame Konfiguration ohne
die Werte der mit `secret:"true"` markierten Felder; ein Test verlangt diese
Markierung für jedes neue Feld, dessen Name nach Passwort, Secret oder Token
aussieht. Gleiche Fingerprints bedeuten gleiche Einstellungen der Pods.

Den Jobstatus liefern die Wartungsjobs (Stufe 8c, unten).

## Provisionierung und Berechtigungen (Stufe 8b)

Rolle `provision`. Quota-Grenzen setzt die Admin-API nicht: maßgeblich ist
LDAP (`rnsMSQuota`), übernommen bei der Anmeldung (Entscheidung 19.09.2026).

```sh
shardpostctl mailbox create [-special-use '\Archive'] user@example.org "Projekte/2026"
shardpostctl mailbox rename [-if-match '"m-…"'] user@example.org "Projekte/2026" "Archiv/2026"
shardpostctl mailbox subscribe|unsubscribe user@example.org "Archiv/2026"
shardpostctl acl set [-mode replace|add|remove] [-if-match '"acl-3"'] user@example.org Shared kollege@example.org lr
shardpostctl acl delete user@example.org Shared kollege@example.org
```

| API | Semantik |
| --- | --- |
| `POST …/mailboxes` `{name, special_use}` | 201 angelegt. Existiert das Postfach schon mit gleichem Special-Use: 200 (Wiederholung nach verlorener Antwort), sonst 409. Special-Use wird wie bei IMAP CREATE in Standardschreibweise gespeichert (`\All`, `\Archive`, `\Drafts`, `\Flagged`, `\Important`, `\Junk`, `\Sent`, `\Trash`), andere Attribute lehnt die API ab. |
| `PATCH …/mailboxes/{id}` `{name?, subscribed?}` | Umbenennen samt Unterordnern (INBOX nicht, belegtes Ziel 409), Abo setzen; gleicher Name ist ein No-op. |
| `PUT …/mailboxes/{id}/acl/{identifier}` `{rights, mode}` | `replace` setzt, `add`/`remove` ändern relativ; der Server rechnet und schreibt in einem versionierten Schritt, parallele Änderungen gehen nicht verloren. |
| `DELETE …/mailboxes/{id}/acl/{identifier}` | Eintrag entfernen. |

- **Versionen:** `GET …/mailboxes/{id}` und Änderungen liefern das
  Postfach-ETag (`"m-…"`), `GET …/acl` und ACL-Änderungen das ACL-ETag
  (`"acl-<Generation>"`). Mit `If-Match` wird eine Änderung bedingt;
  passt die Version nicht, antwortet die API `412 precondition_failed`.
  Die ACL-Version wird im selben Schreibschritt geprüft. Beim Postfach
  (Umbenennen, Abo) wird sie unmittelbar vor der Änderung geprüft; das
  Postfach-ETag umfasst auch die ACL, eine ACL-Änderung macht es also
  ebenfalls ungültig. Ein ungültig geformtes `If-Match` ergibt 400.
- **Identifier** sind kanonische Konten (Nauthilus) oder `anyone`: Aliase
  werden mit dem kanonischen Namen abgelehnt, unbekannte Konten mit 404,
  der Eigentümer mit 400. Der Freigabe-Zeiger des Empfängers folgt wie bei
  IMAP SETACL (für `anyone` gibt es keinen); eine Änderung ohne Wirkung
  schreibt keinen. Scheitert nur das Schreiben des Zeigers, gilt die
  Änderung trotzdem (maßgeblich ist die ACL; Warnung im Log, `acl explain`
  zeigt den fehlenden Zeiger, ein `replace` mit denselben Rechten schreibt
  ihn nach).
- **Namen** speichert der Server so, wie sie ankommen (UTF-8). IMAP-Clients
  mit `UTF8=ACCEPT` sehen dieselben Namen; ältere Clients mit modified
  UTF-7 senden `&AOQ-…`-Formen und sehen Admin-angelegte Umlautnamen
  nicht unter derselben Bezeichnung.
- **Idempotenz** ergibt sich aus der Semantik: absolute Werte, Postfächer
  über ihre stabile ID. Eine wiederholte Anfrage nach verlorener Antwort
  liefert denselben Zustand. Relative ACL-Änderungen dafür mit `If-Match`
  schicken.
- Laufende IMAP-Sitzungen sehen die Änderungen wie die eines anderen
  Clients: Rechte gelten ab dem nächsten Befehl, neue oder umbenannte
  Ordner beim nächsten LIST.
- Exitcodes von `shardpostctl`: 65 bei Konflikt oder veralteter Version
  (409/412), sonst wie bei der Diagnose.

## Wartungsjobs (Stufe 8c)

Langläufer und gefährliche Änderungen laufen als dauerhafte Jobs. Jeder
Server führt einen Runner; Jobs liegen im gemeinsamen Admin-Speicher
(Namespace `~admin` im Indexpool, ein Objekt je Job und Plan) und überleben
Neustarts.

```sh
shardpostctl check -wait user@example.org                  # Prüfung ohne Reparatur (Rolle diagnose)
shardpostctl plan scrub -classes refs,orphans user@example.org
shardpostctl plan quota_recalc user@example.org
shardpostctl plan acl_reconcile user@example.org
shardpostctl plan fts_rebuild user@example.org
shardpostctl plan -mailbox "Archiv/2019" mailbox_delete user@example.org
shardpostctl plan purge user@example.org
shardpostctl apply -wait <plan-id>
shardpostctl job list -account user@example.org
shardpostctl job show|wait|cancel <job-id>
```

**Plan → Apply.** `plan` rechnet trocken und zeigt Zielmenge und Wirkung
(Befunde, Zählerdifferenz, fehlende Zeiger, betroffene Ordner, Nachrichten
und Bytes). `apply` startet den Plan als Job, einmal und vor Ablauf
(`jobs.plan_ttl_seconds`). Beim Löschen eines Postfachs und bei purge
vergleicht der Job vor dem ersten Schreibzugriff den Stand (IDs,
UIDVALIDITY, UIDNEXT, HIGHESTMODSEQ, bei purge auch die Sieve-Skripte) mit
dem Plan; weicht er ab, endet der Job ohne Änderung mit „the account changed
since the plan“. Ein Abgleich der Freigaben wendet nur Reparaturen an, die
geplant und noch nötig sind.

| Art | Rolle | Kontosperre | Wirkung |
| --- | --- | --- | --- |
| `index_check` (ohne Plan) | `diagnose` | nein | Scrubber ohne Reparatur, Befunde als Ergebnis |
| `scrub` | `repair` | nein | Scrubber mit gewählten Klassen: `deliveries`, `moves`, `retired`, `uncovered`, `entries`, `refs`, `orphans`, `unknown`, `quota` (ohne Angabe alle) |
| `quota_recalc` | `repair` | ja | Belegung aus dem Index neu zählen und setzen |
| `acl_reconcile` | `repair` | nein | fehlende Zeiger bei Empfängern schreiben, eigene Zeiger ohne Rechte entfernen |
| `fts_rebuild` | `repair` | ja | Volltextindex löschen und aus allen Nachrichten neu aufbauen |
| `mailbox_delete` | `delete` | ja | Postfach samt Unterordnern löschen (INBOX nicht) |
| `expunge` | `delete` | nein | Nachrichten eines Postfachs endgültig entfernen: `uids` oder `deleted: true` (alle mit `\Deleted`) |
| `purge` | `delete` | ja | Freigaben in beide Richtungen entziehen, Lernjobs und alle Objekte des Kontos löschen |

- **Kontosperre** (Wartungsmodus, Entscheidung 19.09.2026): Der Job hält die
  Sperre, die auch der Import nutzt. IMAP und POP3 antworten solange
  „Mailbox maintenance in progress“, LMTP mit `451 4.2.1`. Bereits offene
  IMAP/POP3-Sitzungen prüfen die Sperre höchstens alle 5 Sekunden beim
  Befehlsstart und trennen dann mit `BYE [UNAVAILABLE]` bzw.
  `-ERR [SYS/TEMP]`. Der Job wartet nach dem Sperren `jobs.settle_seconds`
  (Standard 10), bevor er schreibt. Eine Sitzung, die in dieser Zeit
  keinen Befehl schickt (etwa ein IDLE-Client), schreibt auch nichts.
  Andere Konto-Jobs, Importe und der periodische Scrub bleiben draußen.
  Ist das Konto beim Start schon gesperrt (Import, anderer Job), wartet der
  Job in `running` mit der Phase „waiting for the account lock“ und
  versucht es je Lease-Periode erneut, höchstens `jobs.plan_ttl_seconds`
  nach dem Anlegen. Danach endet er `failed`. Verliert ein Job die Sperre,
  endet er mit „maintenance lock lost“.
- **Übernahme:** Ein Job gehört dem Server, der ihn übernommen hat (Lease mit
  Generation, `jobs.lease_seconds`). Vor jedem Schreibschritt prüft der Job
  seine Lease (Fencing). Stirbt der Server, übernimmt ein anderer nach
  Ablauf der Lease und setzt am letzten Checkpoint fort. Beim geordneten
  Herunterfahren gibt der Server laufende Jobs sofort ab. `fts_rebuild`
  hat keinen Checkpoint und beginnt nach einer Übernahme von vorn.
  Leases vergleichen Uhrzeiten verschiedener Server, deshalb müssen die
  Uhren per NTP synchron laufen. Eine Abweichung weit unter
  `jobs.lease_seconds` ist unkritisch.
- **Abbrechen** (`job cancel`): Ein wartender Job endet sofort, ein laufender
  am nächsten sicheren Punkt. Bereits Getanes wird nicht zurückgenommen.
- **Zustände:** `queued`, `running`, `succeeded`, `partially_failed`,
  `failed`, `cancel_requested`, `cancelled`. `apply -wait` und `job wait`
  enden mit 0 (Erfolg), 1 (Teilfehler) oder 69 (Fehler, abgebrochen).
- **Idempotenz:** `apply -idempotency-key K` bzw. der Header
  `Idempotency-Key` liefert bei Wiederholung denselben Job (200). Mit einer
  anderen Anfrage wird der Schlüssel abgelehnt (409). Schlüssel gelten je
  Aufrufer.
- **Scope:** Pläne, die andere Konten ändern (Freigabe-Abgleich, purge),
  brauchen diese im Scope des Clients, beim Anlegen und erneut beim
  `apply`. Clients mit Scope sehen Jobs nur mit Kontoangabe. Jobs und
  Pläne außerhalb ihres Scopes gelten für sie als nicht vorhanden (404).
- Ohne Jobsystem (`jobs.enabled: false`) antworten die Job-Endpunkte mit
  503 `temporary`.
- **purge** entfernt nur die Daten im Store. Die Identität (LDAP) gehört
  ihrem Owner. Meldet sich das Konto danach wieder an, legt ShardPost eine
  leere INBOX an.
- Beendete Jobs und Pläne bleiben `jobs.retention_days` lang abrufbar.

## Supportwerkzeuge (Stufe 8d)

Werkzeuge für den Support an einzelnen Nachrichten, Sieve-Skripten und der
Lernqueue.

```sh
shardpostctl messages alice@example.org INBOX                       # Metadaten (diagnose)
shardpostctl search -subject Rechnung -since 2026-09-01 alice@example.org   # (content)
shardpostctl message show alice@example.org INBOX 42                # Umschlag + Kopfzeilen (content)
shardpostctl message export alice@example.org INBOX 42 > 42.eml     # (content)
shardpostctl flags -add '\Seen' -remove '$Junk' alice@example.org INBOX 42,43   # (support)
shardpostctl move alice@example.org Junk 42 INBOX                   # copy|move (support)
shardpostctl plan -mailbox Trash -deleted expunge alice@example.org # dann apply (delete)
shardpostctl sieve list|show alice@example.org [name]
shardpostctl sieve put -activate alice@example.org main < main.sieve  # (support)
shardpostctl sieve check -message INBOX:1789839043:42 alice@example.org
shardpostctl learning list -state dead
shardpostctl learning retry -confirm-repeat <id>                   # (repair)
```

- **Stabile Ziele:** Nachrichten werden über Postfach-ID, UIDVALIDITY und
  UID adressiert. Passt die UIDVALIDITY nicht (Postfach neu angelegt),
  antwortet die API mit 412. Ohne `-uidvalidity` nimmt shardpostctl die
  aktuelle. Für strikte Identität den Wert aus einer vorherigen Liste
  mitgeben. Nicht mehr vorhandene UIDs meldet die Antwort als `missing`.
  Kopieren/Verschieben liefert die neue UID je Nachricht (wie UIDPLUS).
- **Expunge** läuft als geplanter Job. Der Plan hält UID und GUID jeder
  Nachricht fest. Beim Anwenden wird nur entfernt, was noch dieselbe GUID
  trägt, bei `deleted` nur, was noch `\Deleted` ist. Eine geänderte
  UIDVALIDITY bricht ab.
- **Suche** ist begrenzt: höchstens `admin.search_scan_limit` Nachrichten
  je Anfrage und 200 Treffer je Seite. `next` setzt fort (in
  Postfach-ID-Reihenfolge). `from`, `to` (auch Cc) und `subject` sind
  Teilstrings im Umschlag, `text` nutzt den Volltextindex. Fehlt der Index
  (nie aufgebaut oder während `fts_rebuild`), findet `text` nichts.
- **Expunge-Pläne** umfassen höchstens 40 000 Nachrichten. Größere Mengen
  in mehreren Plänen erledigen (`-uids`).
- **Datenschutz:** Das Audit-Log nennt bei Suchen nur die Namen der
  Kriterien, nie ihre Werte. Bei Nachrichten und Skripten nennt es die
  Identität und Größe, nie den Inhalt.
- **Sieve** wird vor dem Speichern kompiliert, mit den Includes des Kontos.
  Ein fehlerhaftes Skript wird nicht gespeichert (400). Versionen laufen
  über ETags (`If-Match`, `If-None-Match: *` nur anlegen). Das aktive
  Skript lässt sich nicht löschen.
- **Sieve-Prüfung ohne Nebenwirkungen:** `sieve check` kompiliert und führt
  das Skript auf Wunsch über eine gespeicherte Nachricht (Rolle `content`)
  oder einen mitgeschickten Text (`diagnose`) aus. Dabei wird nichts
  ausgeführt: kein Redirect, keine Abwesenheitsnotiz, keine
  Benachrichtigung, kein externes Programm (`execute`/`filter` gelten als
  mit Exit 0 gelaufen, ohne zu laufen), kein Duplikat- oder
  Abwesenheitseintrag, kein Ablegen. Die Antwort listet, was geschähe, und
  unter `suppressed` jede unterdrückte Nebenwirkung. Mit einem
  mitgeschickten Skript und Text (Rolle `diagnose`) sind die
  Store-Tests (`mailboxexists`, `mboxmetadata`, `specialuse_exists`)
  aktiv. So erfährt `diagnose` nicht mehr als über `mailbox list`. Das
  ist eine bewusste Entscheidung.
- **Lernqueue:** `learning list|show` zeigt Jobs mit Zustand (`pending`,
  `dead`, `delivered`), Programm, Versuchen und nächstem Termin. `retry`
  nimmt nur tote Jobs wieder auf. Lernprogramme sind nicht idempotent:
  Hat das Programm die Nachricht womöglich schon bekommen (Versuche > 0),
  ist `-confirm-repeat` nötig. Ein Job, dessen Nachricht nie festgeschrieben
  wurde (kein Beleg), lässt sich nicht wiederholen.
- Clients mit Konten-Scope sehen Lernjobs nur mit Kontoangabe. Jobs
  anderer Konten gelten für sie als nicht vorhanden (404).

## Protokoll-Trace eines Kontos

Zur Fehlersuche mit einem bestimmten Client lässt sich für ein Konto
befristet ein Mitschnitt des IMAP-Protokolls einschalten (Rolle `trace`):

```sh
shardpostctl trace on -for 30m user@example.org   # ohne -for: trace.default_duration_seconds
shardpostctl trace status user@example.org
shardpostctl trace off user@example.org
```

API: `GET`/`PUT`/`DELETE /admin/v1/accounts/{account}/trace`, `PUT` mit
optionalem `{"duration_seconds": n}`. Der Schalter liegt als Registry-Key
`trace` im Ceph-Konto und gilt daher für alle Server des Clusters, egal
welcher Pod die Anfrage bekommt. Er muss auf das kanonische Konto gesetzt
werden (ein Alias wird wie beim Import mit dem kanonischen Namen
abgelehnt) und läuft spätestens nach `trace.max_duration_seconds` ab.

Wirkung:

- Neue IMAP-Sitzungen des Kontos protokollieren ab der erfolgreichen
  Anmeldung jede Zeile beider Richtungen als `event=protocol_trace` mit
  `dir` (`C` Client→Server, `S` Server→Client), `line`, `session_id` und
  `user`. Zeilen über `trace.max_line_bytes` werden gekürzt (`cut_bytes`).
- Literale (Nachrichteninhalte, APPEND-Daten, abgerufene Abschnitte)
  erscheinen nur als `<literal N bytes>`, der Text von `PREVIEW` als
  `"<masked>"`. Client-Zeilen und -Literale meldet der IMAP-Parser, also
  genau das, was der Server verarbeitet; Pipelining, abgelehnte Literale
  und Lesepuffer verschieben die Sicht nicht. Server-Zeilen stammen aus
  dem gesendeten Datenstrom.
- Anmeldedaten erscheinen nie: der Mitschnitt beginnt nach der Anmeldung
  und endet bei `UNAUTHENTICATE`. Eine Client-Zeile mit `LOGIN` oder
  `AUTHENTICATE` im angemeldeten Zustand wird durch
  `<authentication command, trace stopped>` ersetzt und hält den Trace
  bis zur nächsten Anmeldung an (auch folgende Literale oder SASL-Zeilen
  erscheinen nicht).
- Bereits laufende Sitzungen werden erst bei der nächsten Anmeldung
  erfasst. Der Ablauf wirkt sofort. Ausschalten wirkt in laufenden Sitzungen
  beim nächsten Befehl, sobald `trace.recheck_seconds` vergangen sind; eine
  Sitzung in IDLE wird bis zum Ende von IDLE oder bis zum Ablauf weiter
  mitgeschnitten. Wird der Trace vor dem Ablauf verlängert, setzt eine
  bereits mitgeschnittene Sitzung ihn nach dem alten Ende fort.
- `protocol_trace_start` und `protocol_trace_stop` (mit `reason`) markieren
  Beginn und Ende; das Audit-Log der Admin-API enthält, wer den Trace wann
  und bis wann eingeschaltet hat.

Der Trace enthält Betreffzeilen, Adressen, Dateinamen (ENVELOPE,
BODYSTRUCTURE), Suchbegriffe und Ordnernamen des Kontos. Er
ist deshalb eine eigene Rolle, befristet und nur für die Fehlersuche
gedacht. Auslesen, zum Beispiel:

```sh
kubectl -n shardpost logs -l app.kubernetes.io/name=shardpost-server -c server --since=30m \
  | jq -r 'select(.event=="protocol_trace" and .user=="user@example.org") | "\(.time) \(.session_id[0:8]) \(.dir): \(.line)"'
```

## Betrieb im Cluster (mTLS)

Die Komponente `deploy/components/admin-mtls` (im Overlay
`croessner-nauthilus` aktiv) stellt den mTLS-Listener bereit:

- Eigene CA `shardpost-admin-ca` (cert-manager, gebootstrappt über
  `shardpost-selfsigned`). Sie ist bewusst getrennt von `shardpost-ca`, die
  Serverzertifikate für IMAP/LMTP ausstellt; nur Zertifikate dieser CA
  werden als Admin-Clients akzeptiert.
- Serverzertifikat `shardpost-admin-tls` für den Service
  `shardpost-admin:8443` (Pod-Port `admin`), als Verzeichnis nach
  `/secrets/admin-tls` gemountet (ohne `subPath`, Rotation ohne Neustart).
- Clientzertifikat `shardpost-admin-operator` (CN = Identität) mit der
  Rolle `diagnose`. Weitere Clients bekommen je ein eigenes `Certificate`
  mit `usages: [client auth]` und einen Eintrag in `admin.clients`.
- NetworkPolicy `shardpost-admin-ingress`: Port 8443 nur für Pods im
  Namespace `shardpost` mit Label
  `shardpost.roessner-net.de/admin-client: "true"`.

Aufruf aus einem solchen Pod mit eingehängtem Client-Secret:

```sh
shardpostctl -url https://shardpost-admin.shardpost.svc:8443 \
  -cert /op/tls.crt -key /op/tls.key -ca /op/ca.crt version
```

Abgenommen am 2026-09-18: Operator `version` 200, `deliver` ohne Rolle 403,
Client der fremden CA und ohne Zertifikat im Handshake abgewiesen, Pod
ohne Label von der NetworkPolicy blockiert; Audit-Zeilen mit
`transport=mtls` und der Identität.

## Kontoimport mit erhaltenen IMAP-Identitäten

Kern von Teilpaket 8e ([Stufe 8](development/admin-stage.md)). Ein Konto
wird so übernommen, dass IMAP-Clients nach der Umschaltung nur inkrementell
synchronisieren: UIDVALIDITY, UIDs mit ihren Lücken, UIDNEXT, Flags und
Keywords, INTERNALDATE/SAVEDATE und MODSEQ je Nachricht bleiben erhalten,
HIGHESTMODSEQ wird mindestens der Quellwert. imapsync kann das nicht, weil
IMAP-APPEND die UIDs immer neu vergibt.

### Semantik

- **Sperre:** `POST /admin/v1/accounts/{account}/import` (Rolle `import`)
  nimmt die kontoweite Import-Sperre für eine Sitzung (Header
  `X-Import-Session` bei allen weiteren Aufrufen; jeder Aufruf verlängert
  sie, `admin.import.lock_ttl_seconds`). Solange sie besteht, lehnen IMAP,
  POP3, ManageSieve, LMTP (`451 4.2.1`) und die lokale Zustellung das Konto
  vorübergehend ab. Nur das kanonische Konto der Userdb wird angenommen.
- **Ordner:** `PUT .../import/mailboxes` legt den Ordner mit der
  UIDVALIDITY der Quelle an (Dovecot-Mailbox-GUID als stabile ID, wenn
  verfügbar) oder übernimmt einen vorhandenen mit gleicher UIDVALIDITY. Ein
  leerer Ordner mit anderer UIDVALIDITY (etwa ein INBOX vom ersten Login)
  wird ersetzt, ein nicht leerer ist ein Konflikt (`409 conflict`). Hat die
  Quelle einen Ordner umbenannt, folgt der Import über die stabile ID.
  Neue Ordner bekommen `admin.import.shard_size` als Shardgröße.
- **Nachrichten:** `POST .../mailboxes/{id}/messages` mit
  `multipart/mixed`; jeder Teil ist `message/rfc822` mit der Identität als
  JSON im Teil-Header `X-Import-Meta` (`uid`, `modseq`, `flags`,
  `internal_date`, `save_date`, `guid`, `pop3_uidl`). UIDs aufsteigend. Eine
  UID mit gleich großer Nachricht wird übersprungen (Wiederaufnahme), mit
  anderer ist sie ein Konflikt. Quota wird gezählt, nicht durchgesetzt.
  Gleiche Quell-GUID in mehreren Ordnern teilt sich einen Blob.
- **Deltas:** `POST .../changes` setzt Flags mit dem Quell-MODSEQ und
  entfernt UIDs, die die Quelle nicht mehr hat. `GET .../messages`
  liefert das Inventar seitenweise (`after`, `limit`).
- **Abschluss je Ordner:** `POST .../finalize` setzt UIDNEXT und
  HIGHESTMODSEQ auf mindestens die Quellwerte, die \Recent-Marke auf
  UIDNEXT und den Beginn des Änderungslogs hinter HIGHESTMODSEQ. Ein Client,
  der mit einem älteren Quellstand per QRESYNC/CONDSTORE wiederkommt, bekommt
  so die vollständige Antwort (alle neueren Einträge, VANISHED für alle
  Lücken), einer mit aktuellem Stand nichts.
- **Sieve und ACL:** `PUT .../import/sieve` ersetzt Skripte und aktives
  Skript; alle Skripte werden vorher wie bei ManageSieve übersetzt
  (`422 rejected`, wenn eines nicht geht; dann ändert sich nichts).
  `PUT .../mailboxes/{id}/acl` ersetzt die expliziten ACL-Einträge.
- **POP3:** Mit `-pop3-uidl-format` (Dovecot-Syntax 2.3 `%08Xu%08Xv` oder
  2.4 `%{uid | hex(8)}`) wird je INBOX-Nachricht die UIDL der Quelle
  gespeichert; POP3 `UIDL` gibt sie statt der GUID aus. COPY/MOVE übernehmen
  sie nicht (sie muss je Maildrop eindeutig bleiben).

### Migrator `shardpostctl migrate`

Liest die Quelle per IMAP (Master-User, `UID FETCH ... MODSEQ X-GUID
SAVEDATE`) und ManageSieve (mit PROXY-Kopf, wo der Load-Balancer ihn
verlangt), vergleicht mit dem Inventar des Ziels und schickt nur die
Unterschiede. Jeder Lauf ist wiederholbar und prüft am Ende je Ordner, dass
UIDs, Größen und Flags dem gelesenen Quellstand entsprechen (`verified`).
Ziel-Ordner, die die Quelle nicht mehr hat, werden entfernt
(`-no-prune` schaltet das ab; INBOX bleibt immer).

Im Cluster läuft er im Sidecar `import` des Server-Pods (Komponente
`deploy/components/import-sidecar`) gegen den Admin-Socket. Quelle und
Sieve-Endpunkt stehen als `SHARDPOST_MIGRATE_*` in der Komponente, das
Master-Passwort im Secret `shardpost-migrate-source` (Schlüssel
`password`):

```sh
kubectl -n shardpost create secret generic shardpost-migrate-source \
  --from-literal=password="$(kubectl -n director get secret director-backend-master -o jsonpath='{.data.password}' | base64 -d)"
kubectl -n shardpost exec deploy/shardpost-server -c import -- \
  shardpostctl -timeout 10m migrate user@example.org
```

Für lange Läufe losgelöst mit Protokoll unter `/work`:

```sh
kubectl -n shardpost exec deploy/shardpost-server -c import -- sh -c \
  'nohup shardpostctl -timeout 10m migrate --json user@example.org >/work/user.json 2>/work/user.log &'
```

Exitcodes: 0 alles verifiziert, 1 Teile fehlgeschlagen (erneut laufen
lassen), 64 Aufruf, 69 Lauf abgebrochen.

### Ablauf einer Migration mit dem Director

1. Erstkopie im laufenden Betrieb (Konto noch auf mailstack):
   `migrate` beliebig oft, bis ein Lauf kaum noch Änderungen meldet.
2. Director: `users hold`, `users kick`.
3. Letzter `migrate`-Lauf (Delta, Sieve, ACL); alle Ordner `verified`.
4. Pin im Director/`director.lua` auf `shardpost`,
   `users move --to-shard shardpost --strategy kick_existing`,
   `hold clear`.
5. Kontrolle mit einem Client: gleiche UIDVALIDITY, kein Voll-Download.

Solange mailstack nach der Umschaltung unverändert bleibt, ist der Rückweg
ein Zurück-Routen; danach eingegangene Änderungen müssten zurückkopiert
werden (dann mit Voll-Resync auf dem Rückweg).
