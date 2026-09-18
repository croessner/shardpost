> Historischer Ausbauplan. Die Stufen und Messwerte dokumentieren frühere
> Planungen und sind keine aktuelle Release- oder Kapazitätszusage.
> Aktueller Einstieg: [Dokumentation](docs/README.md),
> [Qualitätsstand](docs/development/quality.md), [Namensmigration](docs/decisions/project-name.md).
>
> Neue Planung vom 16.09.2026: **[Stufe 8: Betriebsverwaltung mit shardpostctl
> und OpenAPI](docs/development/admin-stage.md)**. Diese ist aktuell geplant;
> die historischen Stufen darunter bleiben als Historie erhalten.
> Darin neu (18.09.2026): identitätserhaltender Kontoimport als vorgezogener
> Kern von Teilpaket 8e, damit Migrationen keinen Voll-Resync der Clients auslösen.

# Ausbaustufen: vom POC zum produktiven Mailstore auf RADOS

Dieses Dokument beschreibt, was der POC heute kann, was er im Datenmodell
bereits vorbereitet, und in welchen Stufen daraus ein Server für ein
Millionenpublikum wird. Die Reihenfolge ist nach Nutzen pro Aufwand sortiert.
Grundlage ist das Datenmodell in [ARCHITECTURE.md](ARCHITECTURE.md).

## Stand des POC (Image 0.5.x)

**Eigene Protokollschicht** (`internal/imapproto`): Parser, Dispatcher und
Encoder sind eigener Code, go-imap dient nur noch als Bibliothek für
MIME-Sektionen, BodyStructure und Envelope. Damit sind alle Erweiterungen
erstklassig, die die Bibliothek serverseitig nicht kannte.

**Protokoll:** IMAP4rev1 und IMAP4rev2, LOGIN und AUTHENTICATE PLAIN mit
SASL-IR, STARTTLS und implizites TLS, LOGINDISABLED ohne TLS (abschaltbar),
ID, ENABLE, UTF8=ACCEPT, NAMESPACE, UNSELECT, IDLE über RADOS Watch/Notify,
LIST mit Hierarchie, \Noselect-Zwischenknoten, CHILDREN, LIST-EXTENDED mit
RECURSIVEMATCH/CHILDINFO, LIST-STATUS, SPECIAL-USE, CREATE-SPECIAL-USE,
Autoprovisionierung der Standardordner, SUBSCRIBE/LSUB, CREATE/DELETE/RENAME
inklusive Teilbaum und INBOX-Sonderfall, SELECT/EXAMINE (read-only wird
durchgesetzt), STATUS mit SIZE, HIGHESTMODSEQ und MAILBOXID, APPEND mit
MULTIAPPEND, LITERAL+ und LITERAL-, APPENDLIMIT, FETCH mit allen Sektionen,
Partials, BINARY, ENVELOPE aus dem Index-Cache, MODSEQ, EMAILID, THREADID,
PREVIEW, SAVEDATE, **CONDSTORE** (CHANGEDSINCE, UNCHANGEDSINCE, MODIFIED,
MODSEQ in SEARCH), **QRESYNC** (SELECT-Parameter, VANISHED, VANISHED
EARLIER), SEARCH mit ESEARCH, SEARCHRES, allen RFC-3501-Schlüsseln, MODSEQ,
EMAILID, THREADID und Volltext mit Stemming und Substrings, **SORT** und
**THREAD** (REFERENCES, ORDEREDSUBJECT) aus dem Envelope-Cache ohne
Blob-Zugriff, STORE mit Keywords, EXPUNGE, UID EXPUNGE, COPY, MOVE, UIDPLUS,
**QUOTA** (GETQUOTA, GETQUOTAROOT, SETQUOTA, RES-STORAGE, RES-MESSAGE),
OVERQUOTA-Codes, Idle-Timeout von 30 Minuten.

**Betrieb (Stufen 1b bis 3, umgesetzt in 0.4.x):** POP3 mit Maildrop-Lock
über `rados_lock_exclusive`, COMPRESS=DEFLATE, Charset-Konvertierung in
SEARCH, Verbindungslimits pro IP und Benutzer, Write-Timeouts, sharded
Postfachindex (Shardgröße pro Postfach im Header), Änderungslog `c:<modseq>`
mit inkrementellem Poll, Registry-Cache, Deckel für häufige Trigramme im
Wörterbuch, Scrubber als CronJob, Prometheus-Metriken auf `/metrics`.

**Stufe 5 (0.5.x):** Authentifizierung gegen Nauthilus über gRPC
(`nauthilus.auth.v1.AuthService`, HTTP-JSON als Alternative) mit
XOAUTH2/OAUTHBEARER (Token im Passwortfeld nach Director-Vertrag oder
lokale OIDC-Introspection), Master-User-Login für den nauthilus-director,
LMTP-Empfängerauflösung über `LookupIdentity`, optionales PROXY-Protokoll,
Sieve (fileinto, imap4flags, envelope, variables) im Zustellpfad plus
ManageSieve auf 4190, Intent-Log für MOVE mit Scrubber-Abgleich,
cephx-Key-Rotation ohne Neustart, Graceful Drain mit BYE, OpenTelemetry
(OTLP, per Umgebungsvariable), Lastgenerator `imapload`.

**Lasttest (2 Pods, 20 Benutzer × 3 Sessions, 25 Zustellungen/s, 60 s):**
IMAP-Kommandos p50 20 bis 100 ms, p99 unter 500 ms, LMTP p50 600 ms
(synchroner Volltextindex, siehe asynchrones Indizieren), keine Fehler.

**Review-Runden (Codex, `docs/review-round1.md` bis `review-round4.md`):**
Runde 1 fand 31 Befunde, Runde 2 prüfte die Korrekturen nach und fand 7
Folgefehler, Runde 3 fand 5 weitere (Budget-Deadlock, MODSEQ-Rücksprung,
Snapshot-Rennen bei PendingFloor, CAS-Versuchsergebnisse in STORE,
MOVE-Refcount gegen EXPUNGE), Runde 4 prüfte die Roadmap-Umsetzung und
fand 7 (Zustell-Intents: Erfolg trotz offenem Intent, Rollback per UID,
Aufzeichnung nach dem Speichern; doppelte Doc-IDs bei der
FTS-Konvertierung; VANISHED-Fallback proportional zu UIDNEXT; Löschung
bei Stat-Fehler; Token-Refresh-Fenster). Runde 5 prüfte ACL, METADATA,
CATENATE, NOTIFY und den Sieve-Versand und fand 18 (u. a. Redirect ohne
Ziel bestätigte Verlust, CATENATE ohne Budget, ersetzendes STORE und
APPEND/COPY-Flags ohne Rechteprüfung, Existenz fremder Postfächer
erkennbar, Reader-Tausch in NOTIFY, IDLE-DONE außerhalb der Sperre,
Vacation-Frist vor dem Versand verbraucht). Stand 0.8.1: alle bearbeitet
(Details in ARCHITECTURE.md, Abschnitt "Was das Review erzwungen hat").
Weiter offen und bewusst so gelassen: pending-Änderungen verfallen nach
30 s, VANISHED steht vor dem Shard-Commit im Log, fehlender letzter
Shard ist für den Scrubber nicht erkennbar, MOVE-Recovery identifiziert
Ziel-Einträge nur per GUID. Die danach noch offenen Punkte
(Quota-Reservierung, Zustell-Intents, VANISHED ohne known-UIDs,
Posting-Kompaktierung, zweistufige Waisenlöschung, PendingFloor ohne
festes Fenster) sind mit 0.7.x umgesetzt, siehe ARCHITECTURE.md. Offen
bleibt die Scrub-Enumeration über den Gesamtbestand (Stufe 2).
Nebenwirkung: SETQUOTA ist für Benutzer gesperrt
(`QUOTA_SELF_SERVICE=true` schaltet es frei).

**Datenmodell:** siehe ARCHITECTURE.md. Alles, was das Protokoll ausgibt,
kommt aus dem Index (Meta-Records, Header-omap, Vanished-Log, Registry);
ein Blob wird nur für Body-Sektionen, BODYSTRUCTURE und geschachtelte
Textsuche gelesen.

**Tests:** Die Protokollschicht ist cgo-frei und wird lokal gegen ein
In-Memory-Backend getestet (`go test ./internal/imapproto`), ohne Ceph.
Der Cluster-Test `hack/test-proto.sh` prüft dieselben Kommandos gegen RADOS.

## Stufe 1: Protokoll-Restarbeiten

* **CATENATE** (RFC 4469) und **URLAUTH**: Zusammenbau aus Teilen im
  APPEND-Pfad.
* **NOTIFY** (RFC 5465): Watches auf mehreren Postfächern gleichzeitig,
  das Watch-Modell trägt das bereits.
* **LIST-MYRIGHTS, ACL, METADATA**: siehe Stufe 4.
* **SEARCH=FUZZY**, Phrasen, CJK-N-Gramme im Volltext.
* **Pipelining-Optimierung:** Kommandos werden sequenziell verarbeitet;
  ein Lese-Vorlauf, der Literale früh anfordert, spart Roundtrips.

## Stufe 1b: POP3 (umgesetzt)

`internal/pop3proto` spricht dieselbe Schnittstelle `model.User` wie IMAP:
USER/PASS, AUTH PLAIN, STLS, CAPA, STAT, LIST, UIDL (die GUID), RETR,
TOP, DELE, RSET, QUIT, UTF8. Nur INBOX, Maildrop-Lock per RADOS-Lock auf
Shard 0 (zweite Session bekommt `-ERR [IN-USE]`), RETR setzt `\Seen`,
QUIT expungiert. Der cephx-Client braucht dafür `rwx` (Objektklassen).
Ports 110 mit STLS und 995 implizit.

## Stufe 2: Skalierung des Index (umgesetzt bis auf asynchrones Indizieren)

* **Index-Sharding** ist drin: `mbox/<id>` (Header + Shard 0) und
  `mbox/<id>/<n>`, Shard = (UID-1)/Shardgröße. Die Shardgröße wird bei
  CREATE im Header festgeschrieben (`h:shardsize`), die Konfiguration
  gilt nur für neue Postfächer. Schreiben auf Shard 0 ist ein CAS, auf
  andere Shards zweiphasig (Header, dann Eintrag); Lücken räumt der
  Scrubber auf. Das POC-Overlay setzt 20, Produktion 100000.
* **Änderungslog und Session-Cache** sind drin: `c:<modseq>` je Write,
  Poll liest nur Header, Log und die betroffenen Einträge; ein kompletter
  Reload nur, wenn `h:logstart` den Stand der Session nicht mehr deckt
  (Metrik `commands_total{command="poll",outcome="incremental|reload"}`).
* **Registry-Cache** (2 s TTL) und **Trigramm-Deckel** (2000 Terme, danach
  Marker und Auslassen in der Anfrage, Fallback auf Vokabular-Scan) sind
  drin.
* **Posting-Kompaktierung** ist drin: Dokument-IDs pro Benutzer
  (`ftsmap`), Posting-Listen als sortierte Delta-Varints
  (`internal/postings`), alte Listen werden beim ersten Schreiben
  konvertiert.
* **Noch offen:** asynchrones Indizieren über eine omap-Queue; die
  Zustell-Latenz mit synchronem Index lag im Test bei wenigen zehn
  Millisekunden, der Bedarf ist noch nicht da. Die Scrub-Enumeration
  (alle Objekte eines Namespace) wächst weiter mit dem Bestand; sie ist
  ein Batch-Lauf und lässt sich pro Benutzer parallelisieren.

## Stufe 3: Konsistenz und Betrieb (teilweise umgesetzt)

* **Scrubber** ist drin (`shardpost scrub`, CronJob `shardpost-scrub`):
  verwaiste Blobs (mit Karenzzeit), Einträge ohne Blob, Refcounts, Quota,
  Index-Objekte ohne Registry-Eintrag. Im Test hat er einen gepflanzten
  Orphan und einen falschen Refcount repariert. `SCRUB_DRY_RUN` und
  `SCRUB_USERS` steuern ihn.
* **Metriken** sind drin: RADOS-Latenzen je Operation, CAS-Retries je
  Objektart, Kommando-Latenzen und -Zähler je Protokoll, Verbindungen,
  Zustellungen, Scrub-Befunde. Pod-Annotationen für Prometheus-Scraping.
* **Limits** sind drin: Verbindungen pro IP und Benutzer, Idle-Timeout,
  Write-Timeout als Backpressure, Literal-Grenzen.
* **Intent-Log für MOVE** ist drin: `i:<id>` auf dem Quellobjekt vor dem
  Umzug, entfernt im selben CAS wie das Unlink; der Scrubber vollendet
  liegengebliebene Umzüge deterministisch.
* **Zustell-Intents** sind drin: `d:<id>` auf dem Registry-Objekt für
  Sieve-Mehrfachablage und MULTIAPPEND; der Scrubber rollt abgebrochene
  Zustellungen nach zehn Minuten zurück.
* **Atomare Quota-Reservierung** ist drin: Limit und Zähler in einem CAS,
  Rückgabe bei Fehlschlag.
* **Scrubber zweistufig** für Waisen: gelöscht wird erst, was zwei Läufe
  unverändert unreferenziert sahen.
* **Key-Rotation ohne Neustart** ist drin: das Rook-Secret wird gepollt
  (`KEY_POLL_SECONDS`), eine neue librados-Verbindung übernimmt, die alte
  wird geschlossen, sobald keine Session sie mehr nutzt. Im Test über
  `keyGeneration` am CephClient ausgelöst.
* **Graceful Drain** ist drin: Readiness geht auf rot, IDLE-Sessions
  bekommen `* BYE [UNAVAILABLE]`, `DRAIN_SECONDS` Wartezeit.
* **OpenTelemetry** ist drin: Spans pro IMAP/POP3-Kommando und LMTP-
  Zustellung, OTLP gRPC/HTTP über `OTEL_EXPORTER_OTLP_ENDPOINT`. Spans
  bis auf RADOS-Operationen hinunter fehlen noch (Kontext durch den Store).
* **Nicht im POC:** eigene Objektklasse (cls; C++ auf jedem OSD, kein
  Kandidat für einen produktiven Cluster), Multi-Cluster (kein zweiter
  Cluster).

## Stufe 4: Produktfunktionen (teilweise umgesetzt)

* **Authentifizierung: Nauthilus** ist drin (`internal/auth`), gRPC
  bevorzugt. Vertrag wie beim nauthilus-director: Basic- oder
  Bearer-Backchannel, `AUTH_DECISION_OK|FAIL|TEMPFAIL`, `Auth-Wait` wird
  honoriert (HTTP), `auth_login_attempt` pro Client-Adresse, Konto aus
  `attributes[account_field][0]`, kleingeschrieben. Temporäre Fehler werden
  als `[UNAVAILABLE]`, `-ERR [SYS/TEMP]` bzw. `451` nach außen gegeben,
  nie als Fehlanmeldung. Das POC-Deployment läuft weiter mit dem
  statischen Backend, weil die Backchannel-Credentials und ein Testkonto
  der produktiven Nauthilus-Instanz nicht in diesem Namespace liegen;
  Umschalten über `AUTH_BACKEND=nauthilus` plus `NAUTHILUS_*`.
* **XOAUTH2 / OAUTHBEARER** sind drin (IMAP AUTHENTICATE, POP3 AUTH),
  inklusive RFC-7628-Fehlerdialog. Token-Prüfung wahlweise über Nauthilus
  (Token im Passwortfeld, `method=xoauth2|oauthbearer`) oder lokal per
  `/oidc/introspect` mit anschließendem `LookupIdentity`.
* **Director-Integration:** Master-User (`<konto>*director` mit
  Master-Passwort, `MASTER_USER*`), LMTP-RCPT unverändert entgegennehmen
  und per `LookupIdentity` (`method=recipient_lookup`) auflösen, optional
  Service-Auth per `AUTH PLAIN` (`LMTP_AUTH_*`), PROXY-Protokoll v1/v2
  auf allen Listenern (`PROXY_PROTOCOL_TRUSTED_CIDRS`), ein Konto für
  Deep-Health-Checks.
* **Sieve / ManageSieve** ist drin (`internal/sieve`, foxcpp/go-sieve):
  Skripte als omap des Objekts `sieve` pro Benutzer, aktives Skript im
  xattr; fileinto, keep, discard, reject (550), imap4flags, envelope,
  variables; redirect wird protokolliert und als keep behandelt (kein MTA
  im POC). ManageSieve: AUTHENTICATE PLAIN, STARTTLS, PUT/GET/LIST/
  SETACTIVE/DELETE/RENAME/CHECKSCRIPT/HAVESPACE.
* **Lastgenerator** `imapload` (LMTP plus IMAP-Sessions, Perzentile) als
  Job `hack/loadtest-job.yaml`.
* **Umgesetzt (0.8.0):** Shared Folders und ACL (RFC 4314), METADATA
  (RFC 5464), NOTIFY (RFC 5465, ohne MailboxName/SubscriptionChange/
  AnnotationChange), CATENATE (RFC 4469), Sieve redirect und vacation mit
  Versand über einen SMTP-Relay. Offen bleiben URLAUTH, per-Benutzer-Flags
  in geteilten Postfächern und LMTP-Modus für den Relay.
* **Umgesetzt (0.8.1 bis 0.8.6):** WITHIN, REPLACE, LIST-MYRIGHTS,
  UNAUTHENTICATE; Sieve date/currentdate, duplicate, editheader, mailbox;
  Konformitätsläufe mit imaptest (71 von 72 Gruppen) und der
  Pigeonhole-Suite, siehe `hack/conformance.md`.
* **Umgesetzt (0.9.0):** Sieve ihave, include, index, regex, spamtest/
  virustest, mboxmetadata/servermetadata, special-use, enotify (mailto),
  foreverypart, mime, extracttext; Pigeonhole-Suite 117 von 148 Dateien.
  Offen: Source-Route im ENVELOPE, extlists, replace/enclose;
  imapsieve und extprograms als Stufe 5, JMAP als Stufe 6.

## Stufe 5: Sieve im IMAP-Pfad – implementiert und ausgerollt

Die Serverkonfiguration verwendet Viper und `validator/v10`. YAML-Dateien
und bestehende Environment-Overrides werden vor dem Öffnen von Diensten
geprüft. Siehe [Konfiguration](docs/configuration.md).

IMAPSieve verarbeitet APPEND, COPY, MOVE und tatsächliche Flagänderungen
(einschließlich FETCH/Seen). Skripte werden über METADATA zugeordnet und
unter den Rechten des auslösenden Benutzers ausgeführt. ManageSieve und
Zustellskripte sind getrennt validiert. Details und Grenzen stehen in
[IMAPSieve/Rspamd](docs/imapsieve.md).

Lernen verwendet explizite Special-Use-Regeln für COPY/MOVE und eine
dauerhafte Ceph-Warteschlange mit Commitnachweisen, clusterweiter Worker-Lease,
Backoff und erhaltenen Fehleraufträgen. APPEND/Importe trainieren nicht.
Ab 0.11.0 startet der Worker ausschließlich Programme aus der konfigurierten
Allowlist. Der austauschbare Python-Helper übernimmt die Rspamd-Anbindung;
der Go-Lernpfad kennt keinen Klassifizierer.

Extprograms (`vnd.dovecot.pipe`, `filter`, `execute`) sind implementiert,
mit begrenzten Aufrufen, unveränderlichen Programmnamen, festen Argumenten
und expliziter Freigabe zusätzlicher Skriptargumente. Fehlerhafte Pipes
bewahren die Nachricht; Filter- und Headeränderungen bleiben in Skriptreihenfolge.
Die Details der Review-Korrekturen, Tests und des Rollouts stehen im
[Abnahmebericht 0.11.0](docs/reports/2026-09-16-extprograms.md).

Die frühere Abnahme von 0.10.1 bleibt im
[Bericht vom 15.09.](docs/reports/2026-09-15-imapsieve.md) dokumentiert.
Offene allgemeine Konformitätslücken sind keine Vollfreigabe aller
Sieve-Erweiterungen. extlists, replace/enclose und `vnd.dovecot.report`
bleiben außerhalb dieser Stufe.

### Nachprüfung 0.11.2

Die identifizierten Sieve-/Testadapterfehler sind behoben: CPU-/MIME-Grenzen,
Parservalidierung, Vacation-/SMTP-Details, Byte-Regex und Multiscript-/Flags-
Isolation. Linux-Sieve: 139 bestanden, 0 fehlgeschlagen, 9 ausdrücklich
abgegrenzte Ausnahmen; vollständige Linux-Race-Tests des Servers bestanden.
Der zusätzlich produktiv gefundene HTTP-204-Fehler im Lernhelper ist in
0.11.2 behoben. [Korrekturen und Release-Evidenz](docs/reports/2026-09-16-sieve-conformance-fixes.md).

## Stufe 6: JMAP in vollem Umfang – geplant

Ziel für diesen Server: ein vollständiger JMAP-Server neben IMAP, nicht
ein Proxy davor. Vollständig heißt: JMAP Core (RFC 8620), JMAP Mail
(RFC 8621) komplett, dazu die Erweiterungen, die die Clients (Mozilla
Thunderbird, Ltt.rs, Meli, Cyrus-/Stalwart-kompatible Webmailer)
tatsächlich benutzen. Der Datenpfad bleibt derselbe: JMAP ist eine
zweite Protokollschicht über `model.User`, keine zweite Datenhaltung.

**Core (RFC 8620):**
* Session-Ressource `/.well-known/jmap` mit Accounts, Capabilities,
  `apiUrl`, `downloadUrl`, `uploadUrl`, `eventSourceUrl`; Zustand pro
  Datentyp aus den vorhandenen ModSeqs (Mailbox-Registry, Shard-Header,
  `h:modseq` der Mailbox).
* Authentifizierung über Nauthilus: Bearer-Token (OAuth 2.0, Introspection
  über den gRPC-Backchannel wie beim Director-Vertrag) und Basic für
  Skripte; Master-User-Login für den Director.
* API-Endpunkt mit Method-Calls, Batching, Back-References
  (`#ref`/`resultOf`), `using`-Prüfung, Fehlermodell nach RFC 8620 3.6;
  Limits (`maxSizeRequest`, `maxCallsInRequest`, `maxObjectsInGet`,
  `maxObjectsInSet`, `maxConcurrentRequests`) aus der Konfiguration.
* Blob-Upload und -Download (`Blob/copy` inklusive) direkt auf die
  RADOS-Blobs: Upload legt `msg/<guid>` an, wird bei `Email/import` oder
  `EmailSubmission/set` referenziert und sonst vom Scrubber nach Ablauf
  entfernt.
* Push: `EventSource` (Server-Sent Events) über die vorhandenen
  RADOS-Watches, `PushSubscription` mit Web-Push (RFC 8030, VAPID,
  Verschlüsselung nach RFC 8291), `StateChange` je Datentyp.
* `Core/echo`, Kompression, HTTP/2, saubere `Location`-Redirects hinter
  dem Director (JMAP läuft über HTTPS, der Director leitet per Host/Pfad
  oder Token-Claim auf den Shard).

**Mail (RFC 8621), alles:**
* `Mailbox/get`, `/changes`, `/query`, `/queryChanges`, `/set` mit
  Rollen (`role` aus Special-Use), Rechten (`myRights` aus RFC 4314) und
  Sortierung; `sortOrder` und `isSubscribed` persistent als
  Mailbox-Attribut.
* `Email/get` mit allen Properties (Header-Parsing in jeder Form:
  `header:X:asText/asAddresses/asGroupedAddresses/asMessageIds/asDate/
  asURLs/asRaw`, `bodyStructure`, `bodyValues` mit `maxBodyValueBytes`,
  `textBody`/`htmlBody`/`attachments`, `preview`), `Email/changes`,
  `Email/query` mit dem vollständigen Filter (Text, Adressen, Datum,
  Größe, Keywords, `inMailbox`/`inMailboxOtherThan`, `allInThreadHaveKeyword`
  und Verwandte) und allen Sortierschlüsseln, `collapseThreads`,
  `Email/queryChanges`, `Email/set` (Keywords, Mailboxes, Erstellen aus
  Strukturbeschreibung, Löschen), `Email/copy`, `Email/import`,
  `Email/parse`; Keywords bijektiv auf IMAP-Flags (`$seen`, `$flagged`,
  `$answered`, `$draft`, `$forwarded`, `$phishing`, `$junk`, `$notjunk`).
* `Thread/get` und `Thread/changes` aus der vorhandenen
  REFERENCES-Threadbildung; `threadId` stabil je Konversation
  (persistiert im Meta-Eintrag, nicht bei jedem Aufruf neu berechnet).
* `SearchSnippet/get` aus dem FTS-Index (Treffer-Hervorhebung).
* `Identity/get` und `/set` (Absenderidentitäten, aus LDAP über
  Nauthilus vorbelegt, Ergänzungen persistent).
* `EmailSubmission/get`, `/changes`, `/query`, `/set` mit `onSuccessUpdateEmail`
  und `onSuccessDestroyEmail`: Versand über den Relay (`SMTP_RELAY_*`),
  `envelope`, `undoStatus` mit `sendAt`-Warteschlange (persistent, im
  Benutzer-Namensraum), DSN-Auswertung (RFC 3464) für `deliveryStatus`.
* `VacationResponse/get` und `/set` als Sicht auf das Vacation-Skript
  (ein vom Server verwaltetes Sieve-Skript `jmap-vacation`, damit IMAP-
  und JMAP-Clients dieselbe Abwesenheit sehen).

**Erweiterungen, die dazugehören:**
* JMAP for Sieve Scripts (RFC 9661): `SieveScript/get`, `/set`,
  `/validate`, `/query` auf demselben Skriptspeicher wie ManageSieve.
* JMAP Quotas (RFC 9425): `Quota/get`, `/changes`, `/query` aus dem
  Kontingent des Benutzers.
* JMAP Blob Management (RFC 9404): `Blob/get`, `Blob/upload`,
  `Blob/lookup`, `Blob/copy` mit Byte-Bereichen.
* JMAP Sharing (RFC 9670) und JMAP Mail Sharing (draft): `Principal/get`,
  `ShareNotification`, Freigaben aus den ACLs, `Other Users` als weitere
  Accounts in der Session.
* JMAP for Contacts und Calendars (RFC 9610, draft-ietf-jmap-calendars)
  erst, wenn es dafür Daten gibt (nicht Mail, eigener Punkt).
* Websocket-Transport (RFC 8887) für Clients, die keinen EventSource
  wollen.

**Was dafür im Kern fehlt und vorher kommt:**
* Stabile `threadId` und `emailId` im Meta-Eintrag (heute wird die
  Thread-ID aus den References beim Aufruf abgeleitet; JMAP braucht sie
  als Änderungsstrom).
* Ein Änderungslog pro Datentyp, das `/changes` und `/queryChanges`
  über Sitzungsgrenzen hinweg beantwortet: die vorhandenen
  Shard-Änderungseinträge (`c:` mit ModSeq und UIDs) reichen für E-Mails
  und Mailboxen, für Identitäten und Submissions kommen kleine eigene
  Logs dazu; `cannotCalculateChanges`, wenn der Log abgeschnitten ist.
* Konfigurierbare Header-Parser-Bibliothek für `header:*:as*`
  (der vorhandene Adressparser ist die Basis).
* Body-Parsing mit Charset-Konvertierung und HTML-Sanitizing für
  `bodyValues` (Konverter wie bei `body :text` in Sieve).

**Prüfung:** die JMAP-Konformität mit dem `jmap-samples`/`jmap-client`-
Testkorpus, dem Stalwart-JMAP-Testsatz und einem eigenen Lauf mit
Thunderbird gegen den Director, analog zu `hack/conformance.md`.

**Was JMAP nicht ändert:** Zustellung (LMTP), Sieve-Laufzeit, Speicher-
modell. Es ist eine dritte Frontend-Schicht neben IMAP und POP3, und sie
sollte in einem eigenen Paket `internal/jmap` mit eigener Testabdeckung
entstehen.

**Director-Vertrag (Vorgabe aus `nauthilus-director/docs/FUTURE_JMAP.md`):**
Der Director kann JMAP heute nicht. Seine Design-Notiz legt fest, dass
er JMAP später ausschließlich als JMAP-zu-JMAP-Proxy führt: eigener
HTTPS-Listener, Authentifizierung über Nauthilus, Routing über dieselben
Routing-Fakten und Shard-Tags wie IMAP, Reverse-Proxy auf ein Backend,
das JMAP spricht; keine Übersetzung nach IMAP, keine Mailbox-Semantik im
Director, keine Kontenaggregation über Shards. Daraus folgen für diesen
Server als Backend feste Anforderungen, die in Stufe 6 gleich mit
umgesetzt werden:
* **Session-Ressource mit öffentlichen URLs:** der Director will im
  Standardmodus `backend_advertised` nichts umschreiben; der Server muss
  `apiUrl`, `uploadUrl`, `downloadUrl`, `eventSourceUrl` und die
  WebSocket-URL aus einer konfigurierten `JMAP_PUBLIC_BASE_URL`
  (Director-Adresse) erzeugen, nicht aus dem Pod-Namen.
* **Authentifizierung pro Request:** der Director reicht die
  HTTP-Authentifizierung durch und authentifiziert selbst bei Nauthilus;
  der Server prüft trotzdem selbst (Bearer über den gRPC-Backchannel,
  Basic über `Authenticate`), damit er auch ohne Director korrekt ist.
  Offen und mit dem Director-Owner zu klären: ob der Director dem Backend
  zusätzlich eine bereits geprüfte Identität signalisiert (Master-User-
  Analogie zu IMAP, nur über mTLS), oder ob doppelt geprüft wird. Beide
  Varianten sollen funktionieren, die zweite ist der Standard.
* **Backend-TLS mit Servernamen:** der Director verifiziert Backend-TLS
  immer; das Zertifikat aus `shardpost-ca` muss den Dienstnamen tragen,
  der im Director als `tls.server_name` steht (heute
  `shardpost.shardpost.svc.cluster.local`).
* **Health-Endpunkt ohne Anmeldung und ohne Nebenwirkungen** für die
  Backend-Prüfung des Directors (`/jmap/healthz`, analog `/readyz`).
* **Streaming durch den Proxy:** EventSource und WebSocket (RFC 8887)
  müssen hinter einem HTTP-Reverse-Proxy funktionieren (kein Buffering,
  Heartbeats, saubere Close-Codes bei Drain und Kick); Blob-Upload und
  -Download innerhalb der Request-Limits des Directors
  (`max_body_bytes`, Standard 10 MiB; größere Anhänge chunked über
  RFC 9404 `Blob/upload`).
* **Affinität:** langlebige Verbindungen (EventSource, WebSocket) erzeugen
  im Director aktive Leases; der Server muss Verbindungsabbrüche bei
  Drain und Move ohne Datenverlust verkraften (Zustand ist ohnehin im
  Store, nicht in der Verbindung).
* **Routing-Fakten:** Konten-Pinning wie heute über `director.lua`
  (`mailShard=shardpost`), Protokoll `jmap` als eigener Backend-Pool.

Solange der Director JMAP nicht kann, wird Stufe 6 direkt gegen den
Server getestet (Port-Forward oder interner Ingress mit dem POC-Zertifikat)
und erst in Stufe 7 hinter den Director gestellt.

## Stufe 7: JMAP im Director – geplant, Owner nauthilus-director

Nachgelagerte Stufe im Director-Repo, Auftrag an dessen Owner (Codex),
entlang der Phasen aus `docs/FUTURE_JMAP.md`: (1) Vertrag und Fixtures,
(2) JMAP-HTTPS-Listener mit Nauthilus-Auth, Routing-Fakten, Backend-Wahl
und Reverse-Proxy, (3) Session-Ressource im Modus `backend_advertised`,
(4) Upload, Download, EventSource mit Limits und Redaktion,
(5) WebSocket (RFC 8887) mit Session-Leases, (6) Betriebsfunktionen
(route lookup, drain, move, kick für `protocol=jmap`), (7) E2E gegen
diesen Server als Backend-Fixture. Die offenen Fragen der Notiz
(akzeptierte Auth-Schemata, Cookies, URL-Rewrite ja/nein, Lease-Timing,
Close-Codes, Shared Accounts über Shards) werden vorher gemeinsam
beantwortet; dieser Server liefert dafür das Backend-Fixture und die
Antworten aus Stufe 6. Voraussetzung: Stufe 6 ist in Betrieb.

## Was bewusst nicht kommen sollte

* **Ein Dateisystem oder eine SQL-Datenbank für Metadaten.** Beides wäre
  wieder ein zentraler Engpass. Alles, was hier als "fehlt" steht, lässt
  sich als weiterer omap-Prefix oder weiteres Objekt im Benutzer-Namespace
  ausdrücken. Das ist der Kern des Ansatzes und der Grund, warum er
  horizontal skaliert.
* **Ein Message-Bus für Benachrichtigungen.** RADOS Watch/Notify erledigt
  das cluster-weit, siehe IDLE-Messung im Test (rund 25 ms Pod zu Pod).
