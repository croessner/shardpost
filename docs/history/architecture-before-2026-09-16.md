> Historischer Stand vor der Dokumentationsüberarbeitung vom 16.09.2026.
> Keine aktuelle Betriebsanleitung; Aussagen und Messwerte können überholt sein.
> Einstieg: [aktuelle Dokumentation](../README.md).

# Architektur: Mailstore direkt auf RADOS

## Grundidee

Ceph ist im Kern ein Objektspeicher (RADOS). CephFS und RGW sind Schichten
darüber, die ihre eigenen Metadatenpfade (MDS bzw. Bucket-Index-Shards)
mitbringen. Ein Mailserver hat ein viel einfacheres Zugriffsmuster als ein
POSIX-Dateisystem: Nachrichten sind unveränderlich, ein Postfach ist eine
geordnete Liste von UIDs mit ein paar Flags. Das lässt sich 1:1 auf
RADOS-Primitive abbilden, ohne MDS und ohne HTTP-Gateway dazwischen.

Der Ansatz entspricht dem, was Dovecot mit `obox`/`rbox` (librmb) macht und
was RGW intern für seinen Bucket-Index tut.

## Datenmodell

Pro Benutzer ein **RADOS-Namespace** (im Pool, nicht pro Benutzer ein Pool).
Namespaces sind ein Prefix im Objektschlüssel, kosten nichts und isolieren
Benutzer sauber: der IOContext wird einmal auf den Namespace gepinnt.

```
Pool imap-index (repliziert, size 3)              Pool imap-data (EC 2+1)
namespace <user>                                  namespace <user>
  mailboxes    Registry, omap:                      msg/<guid>/0   Chunk 0, xattr refs=N
                 m:<name>  -> {id, uidvalidity,      msg/<guid>/1   Chunk 1 (4 MiB)
                               subscribed, specialuse}  msg/<guid>/n   ...
                 q:bytes, q:msgs  Quota-Zähler (atomar reserviert)
                 d:<id>     -> {started, entries}  Zustell-Intents
  scrub        omap: o:<guid> -> Objektversion   Waisen-Kandidaten (2 Läufe)
  mbox/<id>    Shard 0 eines Postfachs, omap:
                 h:uidnext, h:uidvalidity, h:modseq, h:keywords,
                 h:shardsize, h:logstart
                 c:<modseq> -> {u:[uids], x:bool}   Änderungslog
                 v:<modseq> -> [uids]   Vanished-Log für QRESYNC
                 m:<uid>    -> {guid,size,chunks,flags,idate,sdate,modseq,env,refs,preview}
  mbox/<id>/n  Shard n: nur m:<uid>-Einträge, Shard = (uid-1)/shardsize
  fts/<shard>  omap: <feld>:<term> -> Posting-Liste (Doc-IDs, Delta-Varint)
  ftsd/<shard> omap: <trigramm> -> Terme          Wörterbuch für Substrings
  ftsmap       omap: g:<guid> -> id, i:<id> -> guid, n  Doc-ID-Zuordnung
```

* **Sharding.** Ein Postfach wächst in neue Objekte statt in eine
  ever-growing omap. Die Shardgröße steht im Header, damit kein Prozess
  mit anderer Konfiguration den Index falsch liest. Shard 0 wird atomar
  per CAS geschrieben, andere Shards zweiphasig (erst Header mit modseq,
  UID und Änderungslog, dann der Eintrag). Ein Crash dazwischen ist eine
  UID-Lücke plus ein verwaister Blob, beides Fälle für den Scrubber.
* **Änderungslog.** Jeder Write hinterlässt `c:<modseq>` mit den
  betroffenen UIDs. Eine Session liest vor jedem Kommando nur den Header;
  hat sich HIGHESTMODSEQ bewegt, holt sie das Log ab ihrem Stand und die
  Einträge der genannten UIDs. Der komplette Reload bleibt der Ausnahme
  vorbehalten, dass `h:logstart` den Stand nicht mehr deckt.
* **Stabile Mailbox-IDs.** Der Name ist nur ein Schlüssel in der Registry,
  das Index-Objekt hängt an der ID. RENAME, auch eines ganzen Teilbaums,
  ist ein einziger CAS-Write auf das Registry-Objekt und fasst keine
  Nachricht an. Die ID ist gleichzeitig die MAILBOXID für OBJECTID.
* **MODSEQ.** Jeder Index-Write erhöht `h:modseq` im selben atomaren
  Schreibvorgang und stempelt die geänderten Einträge. Damit sind
  CONDSTORE und QRESYNC im Datenmodell fertig; das Vanished-Log hält die
  expungierten UIDs pro modseq (begrenzt auf die letzten 10000 Einträge).
* **Envelope-Cache.** Der Meta-Record enthält den IMAP-Envelope, ein
  200-Zeichen-Preview und das Speicherdatum. Listenansichten, SORT und
  THREAD kommen ohne Blob-Zugriff aus.
* **Quota.** Zähler auf dem Registry-Objekt, geprüft vor Zustellung und
  COPY, korrigiert nach EXPUNGE. Sie liegen auf einem anderen Objekt als
  der Postfachindex und sind deshalb eventual consistent; ein Scrubber
  kann sie aus den Meta-Records neu berechnen.

* **Index in omap.** omap ist das Key/Value-Areal, das jeder OSD pro Objekt
  in RocksDB hält. Es ist sortiert, seitenweise lesbar (`startAfter`,
  Prefix-Filter) und Änderungen an einem Objekt sind atomar. Die 10-stellige,
  nullgefüllte UID sorgt dafür, dass Bytefolge == UID-Reihenfolge ist.
* **Blobs unveränderlich, in Chunks.** Eine Mail wird in 4-MiB-Chunks
  (`msg/<guid>/<n>`) einmal mit `write_full` geschrieben, parallel über
  mehrere OSDs/PGs; Lesen ebenso parallel. Damit funktioniert ein
  Erasure-Coded-Pool ohne `ec_overwrites`, große Mails skalieren in der
  Latenz nicht linear, und der Speicher-Overhead für die Masse der Daten
  sinkt von 3x auf 1,5x (bei 2+1; produktiv eher 4+2 oder 8+3).
* **Referenzzählung statt Kopie.** Chunk 0 trägt im xattr `refs` die Zahl
  der Index-Einträge, die auf den Blob zeigen. COPY erhöht den Zähler und
  legt einen Index-Eintrag an, MOVE verschiebt nur den Index-Eintrag,
  EXPUNGE senkt den Zähler und löscht die Chunks erst bei 0. Payload wird
  nie kopiert. Löschen ist damit ein omap-Delete plus ein Objekt-Delete
  pro Chunk, beides O(1) und über alle OSDs verteilt.
* **Zwei Pools, weil omap nur auf replizierten Pools existiert.** Genau so
  trennt auch RGW `buckets.index` (repliziert) und `buckets.data` (EC).

## Konsistenz ohne Lock-Service

Alle Writes an ein RADOS-Objekt werden vom Primary-OSD serialisiert und
jedes Objekt trägt eine Version. Der Server nutzt das als
Compare-and-Swap:

1. Header lesen (`h:uidnext`) und die Objektversion merken.
2. WriteOp mit `assert_version(version)` + `omap_set(h:uidnext, m:<uid>)`.
3. Schlägt die Assertion fehl (jemand anderes hat zwischenzeitlich
   geschrieben), neu lesen und wiederholen.

Damit können beliebig viele stateless Pods gleichzeitig in dasselbe Postfach
liefern, ohne Redis, ohne Datenbank, ohne Locking-Daemon. Das ist derselbe
Mechanismus, den RGW für seine Bucket-Index-Shards verwendet.

Die Einlieferung schreibt erst den Blob, dann den Indexeintrag. Ein Crash
dazwischen hinterlässt höchstens einen verwaisten Blob (per Scrubber
einsammelbar), niemals eine UID ohne Daten.

## Volltextsuche: "Flatcurve auf RADOS"

Dovecots flatcurve-Plugin hält pro Postfach eine Xapian-Datenbank auf einem
Dateisystem. Das lässt sich nicht auf RADOS legen, wohl aber das, was
flatcurve für IMAP leistet: SEARCH über TEXT, BODY, SUBJECT, FROM, TO
ohne jede Mail zu lesen. Der POC hält dafür einen **invertierten Index in
omap**:

* Pro Benutzer N Shard-Objekte `fts/<shard>` im Index-Pool. Ein Term
  (`s:invoice`, `f:alice`, `b:sushi`, `h:…` für alle Header) landet per
  `fnv1a(term) mod N` in genau einem Shard. Damit bleibt die omap eines
  Objekts klein und die Last verteilt sich über PGs/OSDs.
* Wert je Term ist die Posting-Liste: die Dokument-IDs aller Mails mit
  diesem Term, sortiert und als Delta-Varint kodiert (rund ein Byte pro
  Eintrag statt 16 Byte GUID; Paket `internal/postings`). Die ID wird beim
  Indizieren einmal pro Blob auf dem Objekt `ftsmap` vergeben und beim
  Entfernen wieder freigegeben. Listen im alten Format (rohe GUIDs) werden
  weiter gelesen und beim ersten Schreiben konvertiert; das Format ist an
  der Länge unterscheidbar (rohe Listen sind Vielfache von 16 Byte, die
  neue Kodierung nie). Suche = `omap_get_vals_by_keys` über die Terme,
  Schnittmenge der Listen (boolesches AND), Auflösung der IDs zu GUIDs in
  einem gebündelten Read, dann Abgleich mit dem Postfach-Snapshot.
* **Indiziert wird per GUID, nicht per (Postfach, UID).** COPY und MOVE
  fassen den Index deshalb nie an; eine Mail wird genau dann aus dem Index
  entfernt, wenn ihr letzter Verweis verschwindet (refs=0).
* Updates sind dieselben Compare-and-Swap-Writes wie am Postfachindex:
  Terme lesen, Listen ergänzen, mit `assert_version` schreiben. Beliebig
  viele Pods indizieren gleichzeitig.
* **Stemming.** Jeder Token wird zusätzlich als Snowball-Stem (Englisch
  und Deutsch, ohne Sprachdetektion) indiziert; die Suchanfrage wird
  genauso expandiert. `SUBJECT invoicing` findet "invoices",
  `BODY rechnungen` findet "Rechnung".
* **Substrings über ein Trigramm-Wörterbuch.** Flatcurve beantwortet
  Wildcards über Xapians Term-Datenbank. Das Gegenstück hier sind N
  Objekte `ftsd/<shard>`, deren omap jedes Trigramm auf die Liste der
  Terme abbildet, die es enthalten (`voi -> b:invoic, s:invoices, …`).
  Eine Anfrage `SUBJECT voic` liest die Listen von `voi` und `oic`
  (ein gebündelter Read pro betroffenem Shard, parallel), schneidet sie,
  filtert exakt per `strings.Contains` auf das Feld und holt dann nur die
  Postings der Treffer-Terme. **Keine Mail wird für eine Suche gelesen.**
  Das Wörterbuch wächst mit dem Vokabular des Benutzers, nicht mit der
  Mailanzahl, und schrumpft, wenn der letzte Verweis eines Terms fällt.
  Anfragen unter drei Zeichen fallen auf einen Prefix-Scan der sortierten
  omap zurück (`FROM li` findet "lic…", nicht "alice").
* **Parallelität.** Alle Shard-Writes einer Zustellung (Postings und
  Wörterbuch) laufen als unabhängige CAS-Operationen gleichzeitig; ein
  Konflikt kostet nur den Retry des einen Shards. 40 gleichzeitige
  Zustellungen in dasselbe Postfach liefen im Test ohne Fehler durch.
* Bewusst kein Xapian: keine Phrasen, kein Ranking, keine
  Substring-Treffer über Tokengrenzen hinweg (`ce at` in "invoice
  attached"). Nested NOT/OR mit Textkriterien fallen auf Lesen der Mail
  zurück.

Für Produktion käme dazu: N-Gramme statt Tokenizer für CJK, Shard-Anzahl
abhängig von der Postfachgröße, ein per-GUID-Termliste-Objekt, wenn
Re-Tokenisieren beim Löschen zu teuer wird, und asynchrones
Indizieren (Queue im omap) statt synchron im Zustellpfad.

## Was das Review erzwungen hat

Ein externer Code-Review (Codex, `docs/review-round1.md`) hat zehn
Befunde mit Datenverlust-Potenzial gefunden. Die Antworten darauf sind
jetzt Teil des Designs:

* **Versionierte Reads auf privaten Kontexten.** librados liefert die
  Objektversion nur kontextweit (`get_last_version`). Mit parallelen
  Goroutinen auf einem IOContext konnte ein CAS die Version einer fremden
  Operation erwischen. Jeder versionierte Read läuft jetzt auf einem
  Kontext aus einem kleinen Pool, den er bis zur Versionsabfrage exklusiv
  besitzt (`verpool.go`).
* **Alles im CAS lesen.** STORE liest Existenz, Flags und MODSEQ der
  Einträge innerhalb der Header-Transaktion, EXPUNGE entfernt nur, was
  noch existiert, und gibt nur dafür Referenzen frei.
* **Pending-Änderungen für Shards.** Ein Write auf einen höheren Shard
  ist zweiphasig: der Header-CAS vergibt die MODSEQ und kündigt die UIDs
  als `pending` an, die Entscheidung (Flags, UNCHANGEDSINCE, Existenz)
  fällt danach als Read-Modify-Write unter der Version des Shard-Objekts
  selbst, der Commit trägt nach, was tatsächlich geändert oder entfernt
  wurde. Damit serialisieren parallele STOREs auf derselben Nachricht,
  ein verspäteter STORE legt keinen expungierten Eintrag neu an, und ein
  EXPUNGE gibt nur Referenzen frei, die er selbst entfernt hat. Leser
  bleiben vor einem pending-Eintrag stehen (höchstens 30 s); eine Session
  mit frischem Snapshot setzt ihren Stand vor die älteste pending-Änderung
  zurück. Scheitert der Commit nach dem Shard-Write, bleibt der Eintrag
  gültig, die Ankündigung läuft aus.
* **Refcount-Null ist endgültig.** Der letzte EXPUNGE schreibt `refs=0`
  vor dem Löschen; ein COPY gegen null scheitert mit "message is gone".
  MOVE nimmt wie COPY erst eine eigene Referenz für das Ziel und gibt die
  Referenzen der Quelle erst frei, wenn deren Einträge tatsächlich
  entfernt wurden; ein EXPUNGE der Quelle zwischen den beiden
  Index-Schritten kann dem Ziel den Blob nicht wegnehmen.
* **MODSEQ pro Nachricht ist monoton.** Die MODSEQ wird im Header-CAS
  vergeben, der Shard-Write kann aber nach dem eines späteren STORE
  landen; der Eintrag behält dann die höhere MODSEQ. Weil Leser vor einer
  pending-Änderung stehen bleiben, sieht CONDSTORE keine Lücke.
* **Snapshot-Grenze vor dem Snapshot.** SELECT liest erst Header und
  pending-Änderungen, dann den Bestand; die Session startet vor der
  ältesten pending-Änderung. Umgekehrt könnte ein Commit zwischen beiden
  Reads eine Nachricht dauerhaft verbergen.
* **Scrubber bricht bei unvollständigem Scan ab.** Konnte ein Index
  nicht vollständig gelesen werden, wird nichts gelöscht und nichts
  korrigiert. Refcount-Korrekturen sind CAS gegen die beobachtete Version.
  MOVE-Recovery entfernt die Quelle erst, wenn jeder Eintrag im Ziel
  bestätigt ist.
* **Speicherbudget.** Nachrichtenpuffer (Zustellung, APPEND, FETCH mit
  Body, POP3 RETR) laufen gegen ein prozessweites Budget
  (`MEMORY_BUDGET_BYTES`, Standard 256 MiB); darüber wird höchstens 10 s
  gewartet, dann scheitert das Kommando temporär. Die Wartezeit ist
  begrenzt, weil ein Kommando Teilreservierungen hält (alle Literale
  eines Kommandos, die Schritte einer LMTP-Nachricht) und zwei solche
  Warter sich sonst gegenseitig blockieren.
* **Parser-Grenzen.** Zeilenlänge, Listen-Verschachtelung, Argumentzahl,
  Literale pro Kommando (256) und Kommando-Labels für Metriken sind
  begrenzt.
* **Quota wird reserviert, nicht geprüft.** Limit-Prüfung und Erhöhung
  der Zähler sind ein CAS auf dem Registry-Objekt; parallele Zustellungen
  und COPY-Batches sehen die Reservierungen der anderen und können das
  Limit nicht gemeinsam überschreiten. Scheitert die Operation danach,
  wird die Reservierung zurückgegeben.
* **Zustell-Intents.** Sieve mit mehreren `fileinto` und MULTIAPPEND
  bestehen aus mehreren Index-Updates. Vor dem ersten wird `d:<id>` auf
  dem Registry-Objekt angelegt; jede Nachricht wird mit Postfach-ID und
  vorab gewählter GUID eingetragen, **bevor** sie gespeichert wird, am
  Ende wird der Intent entfernt. Erst wenn das gelungen ist, sieht der
  Client ein OK; scheitert es, wird zurückgerollt und temporär abgelehnt
  (sonst könnte der Scrubber bestätigte Mail löschen). Ein Intent älter
  als zehn Minuten ist ein abgestürzter Prozess: der Scrubber entfernt
  die Nachrichten anhand von Postfach-ID und GUID (nie per UID, die nach
  DELETE/CREATE eine andere Nachricht bezeichnen könnte); der Client hat
  kein OK gesehen und wiederholt.
* **MODSEQ-Kollision wird nachgestempelt.** Schreibt ein STORE seinen
  Eintrag nach einem parallelen STORE mit höherer MODSEQ, trüge der
  Eintrag dieselbe MODSEQ wie der frühere Zustand; CHANGEDSINCE würde die
  spätere Änderung übersehen. Der Eintrag wird deshalb mit einer danach
  vergebenen MODSEQ erneut geschrieben.
* **Quota-Drift erst beim zweiten Blick.** Reservierungen laufender
  Zustellungen sind im Index noch nicht sichtbar; der Scrubber korrigiert
  den Zähler nur, wenn zwei Läufe denselben Wert zählten.
* **Unregistrierte Index-Objekte.** Ein Prozess, der zwischen dem
  Anlegen eines `mbox/<id>`-Objekts und dem Registry-Eintrag stirbt,
  hinterlässt ein Objekt, das nichts erreicht (die ID ist zufällig). Der
  Scrubber entfernt solche Objekte nach der Karenzzeit. Gefunden über
  einen OOM-Kill unter Last: `EnsureMailbox` legte pro LMTP-Zustellung
  erst ein Objekt an und löschte es wieder, wenn der Name schon vergeben
  war; jetzt kommt der (gecachte) Lookup zuerst.
* **Waisen erst beim zweiten Blick.** Scan und Reparatur des Scrubbers
  haben keinen gemeinsamen Sperrpunkt. Ein unreferenzierter Blob wird
  beim ersten Lauf nur mit seiner Objektversion vorgemerkt (`scrub`-Objekt)
  und erst gelöscht, wenn ein weiterer Lauf ihn unverändert und weiter
  unreferenziert vorfindet.
* **PendingFloor ohne festes Fenster.** Der Beginn des Log-Scans wird
  durch Sondieren einzelner Einträge in verdoppelten Abständen gefunden
  (log2 Reads bis zum ersten Eintrag älter als 30 s); ein sehr aktives
  Postfach ist damit genauso abgedeckt wie ein ruhiges.
* **VANISHED bei Loglücke.** Ohne known-UIDs meldet der Server alle
  fehlenden UIDs unterhalb UIDNEXT als `VANISHED (EARLIER)`, eine
  Obermenge, die UIDs enthalten kann, die der Client nie sah; für den
  Client ist das folgenlos, ein vollständiger UID-Vergleich entfällt.

## Scrubber

Jede nicht-atomare Stelle des Designs (Blob vor Index, Zwei-Phasen-Write
auf Shards, MOVE über zwei Objekte, Quota auf einem anderen Objekt) ist so
gewählt, dass ein Crash nur harmlosen Abfall hinterlässt. Der Scrubber
läuft pro Benutzer-Namespace, ist damit beliebig parallelisierbar, und
repariert genau diese Fälle mit einer Karenzzeit auf der Objekt-mtime,
damit laufende Zustellungen unberührt bleiben. Er ist ein Subkommando des
Server-Binaries und läuft als CronJob mit denselben Credentials.

## Benachrichtigungen ohne Message-Bus

RADOS hat Watch/Notify eingebaut. Der Store sendet nach jedem Index-Write
ein Notify auf das Postfachobjekt, eine IMAP-Session in IDLE hält einen
Watch darauf. Eine Zustellung auf Pod B erreicht die IDLE-Session auf Pod A
cluster-weit in rund 25 ms, ohne Kafka, Redis oder Director. Notifies sind
best effort; ein 60-Sekunden-Ticker deckt verlorene ab.

## Warum das für Millionen Benutzer trägt

* **Front-Ends sind stateless.** Jeder Pod kann jeden Benutzer bedienen;
  Skalierung ist `replicas: N`. Kein Director, kein Sticky-Routing nötig
  (es hilft nur dem Cache).
* **Kein zentraler Engpass.** Es gibt keine "Mailbox-Datenbank"; die
  Metadaten jedes Postfachs liegen auf dem Objekt des Postfachs und werden
  per CRUSH über alle OSDs verteilt. Millionen Postfächer bedeuten Millionen
  kleine Indexobjekte, gleichmäßig verteilt, nicht eine Tabelle mit
  Milliarden Zeilen.
* **Namespaces statt Pools.** Ceph verträgt hunderte Pools, nicht Millionen;
  Namespaces sind unbegrenzt.
* **EC für die Masse.** Mail ist zu >95 % Blob-Bytes, die nie geändert
  werden. Das ist der Idealfall für Erasure Coding.
* **Keine MDS-Schicht.** CephFS würde jeden Postfachzugriff über den MDS
  leiten, dessen Metadaten-Cache und Single-Active-Design (bzw. Subtree-
  Partitionierung) bei Millionen kleiner Dateien der bekannte Engpass sind.
* **Kein RGW-Hop.** S3 würde jeden Fetch über HTTP+Gateway leiten und
  den Index in RGW-Shards halten, deren Listing-Semantik für IMAP
  (UID-Bereiche, Flags) nicht passt.

## Was für Produktion dazukäme (keine Architekturänderung)

* **Große Postfächer:** ein Indexobjekt pro Postfach reicht bis etwa
  10^5–10^6 Einträge (omap-Größe). Darüber Sharding des Index nach
  UID-Bereichen, exakt wie RGW `bucket_index_max_shards`.
* **Cache:** Header/Index-Snapshots pro Session im Pod cachen; Watch/Notify
  auf dem Postfachobjekt (RADOS hat das eingebaut) für IDLE statt Polling.
* **Scrubber/GC** für verwaiste Blobs (Crash zwischen Blob- und
  Index-Write, Crash zwischen den zwei Index-Writes eines MOVE), Quota im
  Header-omap.
* **Verschlüsselung** pro Benutzer auf Blob-Ebene; msgr2 secure mode für
  den Transport (im Cluster bereits vorausgesetzt: aes256k-Keys).
* **Eigene Objektklasse (cls)** für "uidnext++ und Eintrag setzen" in einer
  serverseitigen Operation statt CAS-Retry; das ist der Weg, den RGW geht
  (`cls_rgw`). Erst nötig, wenn viele parallele Zusteller pro Postfach die
  Retry-Rate hochtreiben.
* **Auth** gegen einen echten Backend-Dienst (hier statisch `test:test`).

## Protokollschicht

`internal/imapproto` ist eine eigene IMAP-Implementierung: Tokenizer für
Atome, Quoted Strings, Literale (synchronisierend, LITERAL+, LITERAL-,
BINARY `~{n}`) und Listen; ein Dispatcher mit Zustandsautomat
(nicht authentifiziert, authentifiziert, selektiert); ein Encoder für
Envelope, BodyStructure und Literale. Die Schicht spricht ausschließlich
`model.User`, eine cgo-freie Schnittstelle, die `cephstore.User` erfüllt.
Dadurch läuft die gesamte Protokolllogik in `go test` gegen ein
In-Memory-Backend, ohne librados, und dieselbe Schicht läuft im Cluster
gegen RADOS.

Die Sitzung hält nur einen Snapshot des Postfachindex
(Sequenznummer = Position). Vor jedem Kommando wird HIGHESTMODSEQ gelesen;
nur bei Änderung wird der Index neu geladen und die Differenz als EXISTS,
EXPUNGE bzw. VANISHED und FETCH FLAGS gemeldet. Das ist die Brücke zwischen
dem MODSEQ im Datenmodell und CONDSTORE/QRESYNC im Protokoll.

## Authentifizierung und Director

Die Front-Ends kennen nur `auth.Authenticator`: Anmeldeversuch hinein,
kanonisches Konto heraus. Die Nauthilus-Implementierung spricht bevorzugt
gRPC (`AuthService.Authenticate` / `LookupIdentity`) mit Basic- oder
Bearer-Metadaten und bildet `AUTH_DECISION_FAIL` auf eine harte
Fehlanmeldung, `TEMPFAIL` und Transportfehler auf eine temporäre ab. Der
Master-User-Wrapper erlaubt dem nauthilus-director `<konto>*director` mit
Master-Passwort; das Zielkonto wird trotzdem per `LookupIdentity`
aufgelöst, damit Aliase und Normalisierung greifen. LMTP löst jeden
RCPT über `LookupIdentity` auf, weil der Director die Envelope-Adresse
unverändert weiterreicht.

## Zugriffsrechte und geteilte Postfächer

* ACLs (RFC 4314) stehen im Registry-Eintrag des Postfachs beim Besitzer
  (`MailboxRec.ACL`, Kennung → Rechte; der Besitzer hat implizit alle).
  Jeder Zugriff prüft gegen diesen Eintrag; ein Postfach ohne Rechte für
  das Konto existiert für dieses Konto nicht (NONEXISTENT statt NOPERM).
* Berechtigte finden fremde Postfächer über Verweise im eigenen Namensraum
  (`sh:<besitzer>/<name>` auf dem Registry-Objekt), geschrieben von SETACL
  auf Besitzerseite, bei RENAME umgehängt, bei DELETE und Entzug
  entfernt. Der Verweis ist ein Hinweis für LIST, nie eine Berechtigung;
  LIST prüft jeden Verweis gegen die ACL und räumt veraltete weg.
  Zugriffe laufen über ein zweites User-Objekt für den Namensraum des
  Besitzers, das die Session bis zum Logout hält.
* Blobs sind namensraumgebunden: COPY/MOVE in ein fremdes Postfach liest
  und speichert die Nachricht neu (Quota des Ziels), innerhalb eines
  Namensraums bleibt es das Referenz-Linking.
* Flags in geteilten Postfächern sind geteilt; `\Seen` pro Benutzer wäre
  ein eigener Index-Eintrag je Konto und ist nicht umgesetzt.
* METADATA (RFC 5464) liegt auf einem Objekt `metadata` je Namensraum,
  Schlüssel nach Postfach-ID (überlebt RENAME) und, für `/private/*`, nach
  zugreifendem Konto. Server-Einträge `/shared/*` liegen im reservierten
  Namensraum `~server` und sind für Konten nur lesbar.
* Rechte gelten auch für Flags: ein ersetzendes STORE prüft je Nachricht,
  welche Flags hinzukommen oder wegfallen; APPEND und COPY lassen Flags
  weg, für die das Recht fehlt (RFC 4314 §4). Fehlende oder nicht
  freigegebene fremde Postfächer antworten einheitlich NONEXISTENT, damit
  ihre Existenz nicht erkennbar wird (TRYCREATE nur im eigenen
  Namensraum). COPY/MOVE über Namensräume hinweg laufen unter einem
  Zustell-Intent und werden bei Fehlern vollständig zurückgenommen.
* NOTIFY (RFC 5465) nutzt dieselben RADOS-Watches wie IDLE: ein Pusher je
  Session hält beim Schreiben die Kommando-Sperre der Verbindung, die die
  Kommandoschleife nur während des Wartens auf Eingabe (und in IDLE)
  freigibt. So mischen sich Benachrichtigungen nie in die Antwort eines
  laufenden Kommandos. Beobachtet werden höchstens 64 Postfächer.

## Sieve

Skripte liegen als omap des Objekts `sieve` im Benutzer-Namespace, das
aktive Skript im xattr. Die Zustellung führt das aktive Skript aus und
legt die Mail in die resultierenden Postfächer (mit Flags) ab; jede
Ablage ist ein normaler `Deliver`. Ein Skriptfehler zur Laufzeit führt zu
INBOX, nie zu Verlust.

## Betrieb im Cluster

* Der Server läuft im Namespace `imap` (PSS `restricted`, non-root, read-only
  root, keine Capabilities). Ports 1143/2424 im Container, 143/24 am Service.
* cephx-Identität `client.imap` mit minimalen Caps (`mon r`, `osd rwx` auf
  genau die zwei Pools; `x` für die Lock-Objektklasse des POP3-Maildrop). Rook legt den Key als Secret in `rook-ceph` ab; eine
  Role dort erlaubt dem ServiceAccount ausschließlich `get` auf dieses eine
  Secret und die Mon-ConfigMap. Der Server holt beides beim Start über die
  Kubernetes-API, es wird nichts manuell kopiert und Key-Rotation über Rook
  funktioniert (Pod-Neustart).
* Das Image linkt gegen librados 20.2.4 (Tentacle), passend zum Cluster;
  aes256k-Keys setzen einen Tentacle-Client mit msgr2 voraus.
* Rotiert Rook den Key (`keyGeneration`), pollt der Server das Secret und
  baut eine neue Verbindung auf; laufende Sessions behalten die alte, bis
  sie enden (Referenzzählung auf der Verbindung).
* Beim Rollout meldet `/readyz` 503, Sessions bekommen `BYE`, der Pod
  wartet `DRAIN_SECONDS` (Deployment: `terminationGracePeriodSeconds` 40).
