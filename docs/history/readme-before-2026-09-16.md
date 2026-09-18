> Historischer Stand vor der Dokumentationsüberarbeitung vom 16.09.2026.
> Keine aktuelle Betriebsanleitung; Aussagen und Messwerte können überholt sein.
> Einstieg: [aktuelle Dokumentation](../README.md).

# imapcephpoc – IMAP/LMTP direkt auf RADOS

Proof of Concept: ein Mailserver (LMTP-Einlieferung, IMAP4rev1/rev2), der
Nachrichten **direkt als RADOS-Objekte in Ceph** ablegt. Kein CephFS, kein RGW,
kein Dateisystem, keine Datenbank. Ziel ist herauszufinden, wie ein eigener
Server aussehen müsste, der Ceph optimal nutzt und horizontal auf ein
Millionenpublikum skaliert. Die Architektur ist in [ARCHITECTURE.md](../../ARCHITECTURE.md)
beschrieben, die Ausbaustufen bis zum Produktivsystem in
[ROADMAP.md](../../ROADMAP.md).

Testbenutzer: `test` / `test` (Umgebungsvariable `USERS`, statisches Backend).
Produktiv: `AUTH_BACKEND=nauthilus` mit `NAUTHILUS_GRPC_ADDR` und den
Backchannel-Credentials, siehe [cmd/imapcephpoc/auth.go](../../cmd/shardpost/auth.go).

## Aufbau

```
cmd/imapcephpoc        main: Konfiguration, Listener, Health-Endpoints
internal/cephstore     RADOS-Store (librados via go-ceph): Index in omap, Blobs in 4-MiB-Chunks
                       mit Referenzzählung, Volltextindex in omap-Shards
internal/fts           Tokenizer, Snowball-Stemming, Trigramme (reines Go, kein Ceph)
internal/imapproto     eigene IMAP-Protokollschicht (Parser, Dispatcher, Encoder) mit
                       CONDSTORE, QRESYNC, QUOTA, SORT, THREAD, OBJECTID, PREVIEW;
                       cgo-frei, lokal gegen In-Memory-Backend getestet
internal/pop3proto     POP3-Front-End auf derselben Schnittstelle
internal/sieve         Sieve-Ausführung (go-sieve) und ManageSieve-Server
internal/auth          Authenticator: statisch, Nauthilus (gRPC/HTTP, OIDC client_credentials), Master-User, SASL-Parser
internal/postings      Posting-Listen-Kodierung (Delta-Varint, cgo-frei)
internal/telemetry     OpenTelemetry-Setup (OTLP)
cmd/imapload           Lastgenerator (LMTP + IMAP-Sessions, Perzentile)
internal/model         gemeinsame Datentypen und die Backend-Schnittstelle (cgo-frei)
internal/memstore      In-Memory-Backend für die Protokolltests
internal/metrics       Prometheus-Metriken
internal/mailmeta      Envelope/Preview-Extraktion für den Index-Cache
internal/lmtpd         LMTP auf Basis von go-smtp
internal/rookcreds     holt cephx-Key + Mon-Adressen aus Rook (Kubernetes-API, RBAC)
deploy/                Kustomize: Namespace imap, Rook-CRs (Pools, CephClient), Deployment, Test-Pod
Dockerfile             Build gegen librados 20.2.4 (Tentacle), Runtime CentOS Stream 9 + librados2
Dockerfile.tools       Debian + swaks + curl für Tests im Cluster
hack/test.sh           End-to-End-Test (swaks -> LMTP, curl -> IMAP)
hack/test-extended.sh  Chunking (10 MiB), SEARCH über den Index, COPY/MOVE/EXPUNGE
hack/test-search.sh    Stemming, Substrings, 40 parallele Zustellungen
hack/test-full.sh      Hierarchie, RENAME, Subscriptions, EXAMINE, Keywords,
                       SEARCHRES, ENVELOPE-Cache, IDLE via Watch/Notify, TLS
hack/test-proto.sh     CONDSTORE, QRESYNC, QUOTA, SORT, THREAD, OBJECTID, MULTIAPPEND
hack/test-ops.sh       Sharding, inkrementeller Poll, POP3, COMPRESS, Charset, Scrubber, Metriken
hack/test-stage5.sh    ManageSieve, Sieve-Zustellung, XOAUTH2/OAUTHBEARER, Master-User, Drain, Key-Rotation
hack/loadtest-job.yaml Lasttest als Job im Cluster
```

## Build und Deploy

```bash
make build build-tools   # Images bauen (linux/amd64, cgo gegen librados)
make push                # nach docker.io/chrroessner/… pushen
make deploy              # kubectl --context croessner apply -k deploy/overlays/croessner
make status
make test                # swaks/curl im mailtest-Pod
go test ./internal/...   # Protokollschicht und Tokenizer lokal, ohne Ceph
```

Beim Deploy entstehen in `rook-ceph` zwei Pools (`imap-index` repliziert,
`imap-data` erasure coded 2+1), ein `CephClient` namens `imap` und eine Role,
die dem ServiceAccount `imap/imap-server` genau das Secret
`rook-ceph-client-imap` und die ConfigMap `rook-ceph-mon-endpoints` lesbar
macht. Der Server holt den Key beim Start selbst; nichts wird manuell kopiert.

`make undeploy` entfernt alles wieder, inklusive der Pools (Rook löscht die
Pools, wenn die CRs verschwinden).

## Nauthilus und nauthilus-director

Das Overlay `deploy/overlays/croessner-nauthilus` schaltet die Anmeldung
auf Nauthilus um (Komponente `deploy/components/nauthilus`): gRPC gegen
`nauthilus-service.auth.svc.cluster.local:8444` (doppelgaenger-Sidecar,
TLS-Name `login.authserv.me`, System-CAs), Backchannel-Auth per OIDC
`client_credentials` (`imappoc-service`, Token-URL
`https://login.authserv.me/oidc/token`, Scopes `nauthilus:authenticate`
und `nauthilus:lookup_identity`), Master-User `nauthilus-director` für die
Backend-Logins des Directors, `INSECURE_AUTH=false`. Die Bearer-Replays des
Directors (XOAUTH2/OAUTHBEARER) gehen mit dem SASL-Methodennamen im
Passwortfeld an Nauthilus.

Die beiden Secrets, die das Overlay erwartet, entstehen in den anderen
Repositories und werden nie von Hand angelegt:

| Secret (Namespace imap) | Keys | Quelle |
|---|---|---|
| `imap-nauthilus-oidc` | client-id, client-secret, token-url | nauthilus-tests/kubernetes: `make handover-imappoc` |
| `imap-director-backend` | master-password | kubernetes/director: `make sync-imappoc-secrets` |

Der Server-Cert der POC-Listener wird von einer namespace-lokalen CA
(`imap-poc-ca`, cert-manager) ausgestellt; ihr Zertifikat liegt im
Director-Repo als `certificates/imap-poc-ca.crt` und wird dort als
`tls.ca_file` der Backends `imappoc-imap/-lmtp/-sieve` (Shard `imappoc`)
verwendet. Nauthilus pinnt `rbdpoc1@exampleserver.de` und
`rbdpoc2@exampleserver.de` per `director.lua` dauerhaft auf diesen Shard.

Stand 2026-09-15: die Umstellung ist erfolgt, das Overlay
`croessner-nauthilus` ist der Live-Stand. `hack/acceptance-nauthilus.sh`
prüft Login, LMTP-Zustellung und die Negativfälle mit den LDAP-Konten
(Passwörter nur aus der SOPS-Datei des rns-server-Repos, im Speicher).

Reihenfolge der Umstellung (jeder Schritt ein produktiver Rollout, den der
jeweilige Owner freigibt):

1. nauthilus-tests/kubernetes: `make apply-prod`, dann `make handover-imappoc`
   (Details: `docs/imappoc-handover.md` dort).
2. LDAP: die beiden Konten und `mail_health_imappoc` unter `ou=health`.
3. kubernetes/director: `make -C director sync-imappoc-secrets`, dann
   `make -C director deploy` (Details: `director/imappoc.md` dort).
4. Hier: `kubectl --context croessner apply -k deploy/overlays/croessner-nauthilus`.
   Die Test-Skripte unter `hack/` setzen die statischen Konten voraus und
   gelten dann nicht mehr; der Abnahmetest läuft über den Director
   (`mail.roessner-net.de`, Skript in `director/imappoc.md`).

Speicher: librados hält allein rund 150 MiB RSS, dazu kommt das
Nachrichtenbudget (`MEMORY_BUDGET_BYTES`, 256 MiB). Das Deployment setzt
deshalb 1 GiB Limit und `GOMEMLIMIT=700MiB`; unter dem Lasttest (20
Benutzer, 25 Zustellungen/s) liegt der Pod bei etwa 230 MiB.

## Manuell testen

```bash
kubectl --context croessner -n imap exec -it deploy/mailtest -- bash
swaks --protocol LMTP --server imap:24 --from a@example.org --to test@example.org
curl --url 'imap://imap/INBOX?ALL' --user test:test          # SEARCH ALL -> UIDs
curl --url 'imap://imap/INBOX;UID=1' --user test:test        # Nachricht abrufen
curl --url 'imap://imap/' --user test:test -X 'STATUS INBOX (MESSAGES UNSEEN)'
curl --url 'imap://imap/INBOX' --user test:test -X 'SEARCH SUBJECT invoice FROM alice'   # aus dem RADOS-Index
curl --url 'imap://imap/INBOX' --user test:test -X 'UID MOVE 2 Archive'
curl --url 'imap://imap/INBOX' --user test:test -X 'UID STORE 3 +FLAGS (\Deleted)'
curl --url 'imap://imap/INBOX' --user test:test -X 'EXPUNGE'
```

Umgebungsvariablen: `CEPH_CHUNK_SIZE` (Bytes, Standard 4 MiB), `FTS_SHARDS`
(Shard-Objekte pro Benutzer, Standard 16), `USERS`, `QUOTA_BYTES`/`QUOTA_MSGS`
(0 = unbegrenzt), `TLS_CERT`/`TLS_KEY` (aktiviert STARTTLS und IMAPS auf
`IMAPS_ADDR`), `INSECURE_AUTH` (LOGIN ohne TLS erlauben, POC-Standard true).

Weitere: `MBOX_SHARD_SIZE` (UIDs pro Index-Shard, gilt für neue Postfächer,
Standard 100000; das POC-Overlay setzt 20), `MAX_CONNS_PER_IP`,
`MAX_CONNS_PER_USER`, `POP3_MARK_SEEN`, `SCRUB_GRACE_SECONDS`,
`SCRUB_DRY_RUN`, `SCRUB_USERS`.

Im Cluster stellt cert-manager ein selbstsigniertes Zertifikat aus; Service-Ports
sind 143 (IMAP + STARTTLS), 993 (IMAPS), 110 (POP3 + STLS), 995 (POP3S),
4190 (ManageSieve + STARTTLS) und 24 (LMTP + STARTTLS). Metriken liegen auf Port 8080 unter `/metrics`.
Die [Observability-Dokumentation](../../docs/observability.md) beschreibt OTLP-Aktivierung,
Messwertbedeutung und die Abnahme von Prometheus, Tempo und Operator-Dashboard. Der
Scrubber läuft nächtlich als CronJob `imap-scrub` und manuell mit

```bash
kubectl --context croessner -n imap create job scrub-now --from=cronjob/imap-scrub
```

Direkt in Ceph nachsehen (Namespace = Benutzer):

```bash
kubectl --context croessner -n rook-ceph exec deploy/rook-ceph-tools -- rados -p imap-index -N test ls
kubectl --context croessner -n rook-ceph exec deploy/rook-ceph-tools -- rados -p imap-index -N test listomapvals mbox/INBOX
kubectl --context croessner -n rook-ceph exec deploy/rook-ceph-tools -- rados -p imap-data  -N test ls
```

## Quota, Löschen, LMTP-Details

* **Quota je Konto:** Standard aus `QUOTA_BYTES`/`QUOTA_MSGS`; ein Wert
  pro Konto kommt aus dem Attribut, das Nauthilus mit dem Login liefert
  (`QUOTA_ATTRIBUTE`, Standard `rnsMSQuota`, Bytes mit optionalem
  K/M/G/T-Suffix, 0 = unbegrenzt) und wird bei jedem Login und jeder
  Zustellung übernommen. `GETQUOTAROOT INBOX` zeigt den wirksamen Wert;
  SETQUOTA durch Benutzer ist gesperrt (`QUOTA_SELF_SERVICE`).
* **Löschen:** EXPUNGE entfernt den Index-Eintrag und gibt die
  Blob-Referenz frei; mit der letzten Referenz werden die Chunks sofort
  gelöscht (kein Papierkorb im Store, keine Aufbewahrung). Waisen aus
  Abstürzen räumt der Scrubber nach zwei Läufen weg.
* **LMTP:** SMTPUTF8 und 8BITMIME werden angekündigt, jede Zustellung
  bekommt einen eigenen `Received:`-Header (Absender-Adresse, dieser Host
  aus `LMTP_HOSTNAME`, Empfänger, Zeit).

## Zugriffsrechte, Annotationen, NOTIFY, CATENATE, Sieve-Versand

* **ACL und Shared Folders (RFC 4314):** SETACL/DELETEACL/GETACL/LISTRIGHTS/
  MYRIGHTS. Ein Postfach eines anderen Benutzers erscheint als
  `Other Users/<konto>/<postfach>` (NAMESPACE meldet den Präfix). Rechte
  liegen im Registry-Eintrag des Besitzers, Berechtigte bekommen einen
  Verweis in ihrem Namensraum; COPY/MOVE zwischen Benutzern kopiert die
  Daten (Blobs sind namensraumgebunden). Flags sind geteilt, nicht pro
  Benutzer.
* **METADATA (RFC 5464):** GETMETADATA/SETMETADATA mit DEPTH und MAXSIZE.
  Mailbox-Einträge liegen beim Besitzer, `/private/*` je zugreifendem
  Konto; `/shared/*` auf Serverebene ist global und für Konten nur lesbar.
* **NOTIFY (RFC 5465):** SELECTED, SELECTED-DELAYED, INBOXES, PERSONAL,
  SUBSCRIBED, SUBTREE, MAILBOXES mit MessageNew (inkl. Fetch-Attributen),
  MessageExpunge, FlagChange; höchstens 64 beobachtete Postfächer,
  Ereignisse auch während IDLE. MailboxName, SubscriptionChange und
  AnnotationChange werden mit BADEVENT abgelehnt.
* **CATENATE (RFC 4469):** APPEND aus TEXT-Teilen und IMAP-URLs
  (`imap:///Postfach;UIDVALIDITY=n/;UID=n[/;SECTION=x]`), Rechte werden
  geprüft, Fehler als BADURL.
* **Sieve redirect und vacation:** brauchen `SMTP_RELAY_ADDR` (optional
  `SMTP_RELAY_USER/PASSWORD`, `SMTP_RELAY_TLS`, `SMTP_RELAY_HELO`). Redirect
  behält den Envelope-Absender, meidet Schleifen (kein Redirect an sich
  selbst oder von automatischer Mail, höchstens vier Ziele) und ist bei
  Relay-Fehler ein temporärer LMTP-Fehler. Vacation folgt RFC 5230 (keine
  Antwort an Listen, automatische Mail, den Nullabsender oder wenn das
  Konto nicht adressiert ist), einmal pro Absender und Zeitraum (Merker
  auf dem Sieve-Objekt), Antwort mit Nullabsender und Auto-Submitted.
  go-sieve liegt dafür als Fork mit Vacation-Erweiterung unter
  `third_party/go-sieve` (replace-Direktive).

## Weitere Erweiterungen und Konformität

* IMAP: `WITHIN` (SEARCH YOUNGER/OLDER), `REPLACE` (RFC 8508, auch mit
  CATENATE), `LIST-MYRIGHTS` (RFC 8440), `UNAUTHENTICATE` (RFC 8437).
* Sieve (im Fork `third_party/go-sieve`): `date`/`currentdate`
  (RFC 5260), `duplicate` (RFC 7352, Merker auf dem Sieve-Objekt),
  `editheader` (RFC 5293, Received/Return-Path/Auto-Submitted/Delivered-To
  geschützt; die gespeicherte Nachricht ist die editierte), `mailbox`
  (`fileinto :create`, `mailboxexists`), `ihave`, `include` (`:personal`
  aus dem eigenen Skriptspeicher, `:global` aus `SIEVE_GLOBAL_DIR`,
  `global`/`return`), `index`, `regex`, `spamtest`/`spamtestplus`/
  `virustest` (Header und Skala über `SIEVE_SETTINGS`, Standard
  `X-Spam-Score` mit Maximum 15), `mboxmetadata`/`servermetadata` (liest
  die IMAP-METADATA-Einträge), `special-use` (`fileinto :specialuse`,
  `specialuse_exists`), `enotify` mit `mailto:` (Versand über den Relay,
  keine Benachrichtigung über automatische Mails), `foreverypart`/`break`,
  `mime` (`:type`, `:subtype`, `:contenttype`, `:param`, `:anychild`) und
  `extracttext`, konfigurierbares `imapsieve` und `vnd.dovecot.execute`/`pipe`/`filter` über eine Programm-Allowlist (siehe unten). Nicht umgesetzt: `extlists`, `replace`/
  `enclose`, Dovecot-eigene `vnd.dovecot.*`. Einstellungen mit den
  Pigeonhole-Schlüsseln (`sieve_editheader_header/...`, `sieve_notify_*`,
  `sieve_redirect_envelope_from`, `sieve_vacation_default_subject*`) in
  `SIEVE_SETTINGS` als `key=value;key=value`.
* Konformität: `hack/conformance.md` beschreibt die Läufe mit Dovecots
  imaptest (Job `hack/imaptest-job.yaml`, Image `chrroessner/imaptest-poc`)
  und der Pigeonhole-Testsuite (`SIEVE_CONFORMANCE=1 go test ./tests` im
  Fork, Checkout unter `third_party/go-sieve/pigeonhole`). Stand 0.8.7:
  imaptest 71 von 72 Gruppen (offen nur die Source-Route im ENVELOPE),
  Pigeonhole-Stand 0.11.2: 139 Dateien bestanden, 0 fehlgeschlagen,
  9 begründet ausgenommen. CPU-/MIME-Grenzen sind geprüft; optionale
  Erweiterungen und Diagnose-/Crash-Fixtures sind in `hack/conformance.md`
  ausdrücklich abgegrenzt.
* Header-Adressen (`internal/mailmeta/address.go`): eigener RFC-5322-Parser
  für ENVELOPE, SORT und SEARCH, mit Gruppen, Kommentaren und der
  veralteten Syntax; Sender und Reply-To fallen auf From zurück.
* `\Recent`: die erste Sitzung, die eine Nachricht sieht (SELECT oder
  NOOP/IDLE), nimmt sie; STATUS RECENT zählt danach 0.
* APPEND mit `Content-Transfer-Encoding: binary` (literal8) wird beim
  Speichern nach base64 umkodiert (RFC 3516 erlaubt das), damit auch
  Clients ohne BINARY die Nachricht lesen können.

## Was der POC bewusst nicht kann

TLS, SASL-Mechanismen außer LOGIN, RENAME, Quota, Sieve, Phrasensuche und
Substrings über Tokengrenzen hinweg, Garbage Collector für verwaiste Blobs. Nichts davon ist eine Architekturfrage,
siehe ARCHITECTURE.md.

## Konfigurationsdatei und IMAPSieve

Der Server unterstützt YAML über `--config` oder `IMAP_CONFIG`, validiert mit Viper/validator. Bisherige Env-Variablen bleiben Overrides. Details: [Serverkonfiguration](../../docs/configuration.md) und [IMAPSieve/Rspamd](../../docs/imapsieve.md).
