# RADOS-Datenmodell und Konsistenz

[Architektur](../../ARCHITECTURE.md) · [Entwicklerhandbuch](../development/README.md)

## Physisches Modell

![Objekte und Pools](../diagrams/storage.svg)

Der kanonische Kontoname wählt einen RADOS-Namespace in gemeinsam genutzten
Pools. Namespaces organisieren Daten; sie sind hier keine eigenständigen
Ceph-Zugangsdaten pro Benutzer. Die Zugriffskontrolle muss vorher im
Protokoll-/Auth-/ACL-Pfad stimmen. Das Server-Ceph-Konto kann mehrere
Benutzernamespaces erreichen.

| Objekt/Struktur | Bedeutung | Primärquelle |
|---|---|---|
| `mailboxes` | Registry aus Namen und stabilen Mailbox-IDs; Quota und Zustell-Intents | [user.go](../../internal/cephstore/user.go) |
| `mbox/<id>` | Header, UID-Zähler, MODSEQ, Änderungs-/Vanished-Log und erster UID-Shard | [user.go](../../internal/cephstore/user.go) |
| `mbox/<id>/<n>` | Weitere Nachrichteneinträge | [user.go](../../internal/cephstore/user.go) |
| `msg/<guid>/<n>` | Unveränderliche Payload-Chunks; Chunk 0 trägt `refs` | [blob.go](../../internal/cephstore/blob.go) |
| `fts/<shard>`, `ftsd/<shard>`, `ftsmap` | Postinglisten, Trigrammwörterbuch, GUID-/Doc-ID-Zuordnung | [fts.go](../../internal/cephstore/fts.go) |
| Skript-/Metadatenobjekte | Sieve-Texte, aktives Skript, private/geteilte Metadaten | [scripts.go](../../internal/cephstore/scripts.go), [metadata.go](../../internal/cephstore/metadata.go) |
| `scrub` | Kandidaten und Reparaturzustand | [scrub.go](../../internal/cephstore/scrub.go) |
| `~learning` / `learning/<job-id>` im Datenpool | Unabhängige Nachricht plus Job-xattr | [learning.go](../../internal/cephstore/learning.go) |

Der Indexpool ist repliziert, weil seine omap-Strukturen das verlangen.
Der Datenpool ist im vorhandenen Manifest EC 2+1. Das ist eine konkrete
Betriebskonfiguration, keine allgemeine Empfehlung für jede Clustergröße.
Nachrichten bestehen standardmäßig aus 4-MiB-Chunks; die Anzahl ist Teil
der Metadaten. `readBlob` verwendet die konfigurierte Chunkgröße zur
Berechnung der Offsets. Sie darf bei bestehenden Daten nicht beiläufig
geändert werden. Auch die FTS-Shardzahl beeinflusst die Objektzuordnung.

Die Mailbox-Shardgröße ist dagegen im Mailboxheader gespeichert und wird bei
der Anlage festgelegt. Für eine UID gilt `shard = (uid - 1) / shardSize`.
Stabile Mailbox-IDs entkoppeln RENAME von den Payload-Objekten; Namen sind
Registry-Schlüssel, keine verlässlichen physischen Objektpfade.

## Identitäten nicht verwechseln

| Begriff | Gültigkeitsbereich | Bedeutung |
|---|---|---|
| Konto | Auth-/Store-Vertrag | Kanonischer Namespace-Eigentümer |
| Mailbox-ID | Registry eines Kontos | Stabile Identität unabhängig vom Anzeigenamen |
| UIDVALIDITY + UID | Eine Mailbox | Identität einer Nachricht aus Sicht des IMAP-Clients |
| GUID | Blob und seine Indexverweise | Referenz auf gemeinsame Nachrichtendaten |
| MODSEQ | Mailbox-Änderungsverlauf | Ordnungswert für Synchronisierung und bedingte Änderungen |
| FTS-Doc-ID | Suchindex eines Kontos | Kompakte Postinglisten-Identität, keine IMAP-UID |
| session_id | Eine Netzwerk-/Backend-Sitzung | Log-Korrelation, keine persistente Mailidentität |

## Atomare Grenze

RADOS atomisiert Operationen **innerhalb eines Objekts**. Der Writer liest
Zustand samt Version und schreibt mit `AssertVersion`; bei einem Konflikt
wird neu gelesen und erneut entschieden. Die CAS-Helfer und der
[Versionskontext-Pool](../../internal/cephstore/verpool.go) verhindern, dass
eine nebenläufige Operation eine fremde gelesene Version verwendet.

Zwischen Registry, Mailboxheader, höherem Shard, Blob, FTS und externem
Programm existiert keine gemeinsame Transaktion. Deshalb muss jede Änderung
festlegen, welcher Zustand bei einem Abbruch sichtbar bleibt und welcher
Reparaturpfad ihn wieder auflösen kann.

## Schreiben einer Nachricht

`DeliverGUID` prüft GUID und Größe, löst die Mailbox auf und reserviert Quota.
Danach schreibt es Blob-Chunks, FTS und schließlich den Mailboxeintrag.
Der erfolgreiche Indexeintrag soll nie auf noch nicht geschriebene Nutzdaten
verweisen. Fehler vor dem Verknüpfen lösen die vorgesehenen Aufräum- und
Quota-Rückgabepfade aus; ein Prozessabbruch kann Restzustand hinterlassen.

![Commit eines höheren Shards](../diagrams/shard-commit.svg)

Shard 0 kann Header und Eintrag in einem CAS verändern. Für höhere Shards
kündigt der Header die Änderung zunächst als Pending an. Anschließend wird
der Eintrag geschrieben und das Journal abgeschlossen. Leser dürfen ihren
MODSEQ-Wasserstand nicht über unvollständige Änderungen hinweg bewegen.
`PendingFloor` und die Synchronisierung der Protokollschicht beachten dies.

Wenn der Eintrag bereits dauerhaft ist, aber das abschließende Pending-
Commit scheitert, darf der Aufrufer den Blob nicht wie bei einem frühen
Fehler löschen: Dann würde ein sichtbarer Eintrag ohne Daten zurückbleiben.
Genau diese Unterscheidung ist im `link`-Pfad dokumentiert.

## COPY, MOVE und EXPUNGE

COPY legt einen zusätzlichen Indexverweis an und erhöht die Blob-Referenz.
Der Payload muss innerhalb dieses Store-Pfades nicht erneut geschrieben
werden. MOVE ist nicht lediglich ein atomarer Zeigerwechsel: Der Code legt
einen Move-Intent an, erhöht vorübergehend Referenzen, verknüpft das Ziel,
entfernt die Quelle und gibt deren Referenz frei. Ein Abbruch lässt den
Intent für die Reparatur bestehen. Nach erfolgreichem MOVE ist die Netto-
Referenzzahl unverändert.

EXPUNGE entfernt Mailboxeinträge und gibt Blob-Referenzen frei. Die letzte
Referenz führt zur Löschung der Chunks. Das ist keine Backup- oder
Aufbewahrungsfunktion. `\Deleted`, Papierkorb-Mailbox und physisches Löschen
sind verschiedene Zustände.

Bei mehreren lokalen Zustellaktionen schützt ein Delivery-Intent den
Zusammenhang. Seine Einträge nennen die stabile Mailbox-ID und die GUID,
bevor die Nachricht geschrieben wird. Verwaiste Intents werden nach der
vorgesehenen Altersgrenze repariert bzw. zurückgerollt. Intents garantieren
keine Exactly-once-Zustellung gegenüber einem externen Client, dessen
Erfolgsantwort auf dem Netzwerk verloren gehen kann.

## Benachrichtigungen und Suche

Watch/Notify ist ein Wecksignal. Maßgeblich bleiben Mailboxheader,
Änderungsjournal und Einträge. Ein voller Snapshot wird nötig, wenn das
Journal den angeforderten Zeitraum nicht mehr abdeckt. Watches müssen vor
dem Schließen ihrer Store-Sicht beendet werden.

Der Volltextindex arbeitet pro GUID. Feldterme und DE/EN-Stemming werden in
Postinglisten abgelegt; ein Trigrammwörterbuch unterstützt Teilwortsuchen.
Nicht jede SEARCH-Kombination lässt sich vollständig mit diesem Index
beantworten. Die Protokollschicht kann Nachrichtendaten nachlesen; die
historische Aussage „keine Mail wird für eine Suche gelesen“ ist zu pauschal.
Siehe [search.go](../../internal/imapproto/search.go) und
[Tokenizer](../../internal/fts/tokenize.go).

## Reparatur und Grenzen

Der Scrubber prüft unter anderem Referenzen, Quota, verwaiste Blobs,
fehlende Blobs und abgebrochene Intents. Altersgrenzen, Objektversionen und
mehrstufige Kandidatenbehandlung schützen laufende Operationen. Ein Dry-run
ist die Diagnoseoption vor beabsichtigten Reparaturen.

Scrub ist keine vollständige Datensicherung und kein Ersatz für Restore-
Tests. Ein fehlender Blob kann nicht aus seinem Indexeintrag rekonstruiert
werden. Backup/Restore über beide Pools sowie eine abgestimmte Datenmigration
müssen als eigene Betriebsverfahren qualifiziert werden.

Für Store-Änderungen immer Erfolgs-, Konflikt- und Abbruchpfade beschreiben:
Was ist dauerhaft? Was sieht ein Leser? Wer räumt auf? Ein erfolgreicher
In-Memory-Protokolltest beantwortet diese Fragen für RADOS nicht.
