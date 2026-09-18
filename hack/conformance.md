# Konformitätsläufe

## IMAP: Dovecot imaptest

Image `docker.io/chrroessner/shardpost-imaptest` (bis 0.12.x `imaptest-poc`) = `dovecot/imaptest` plus die
Test-Skripte aus `github.com/dovecot/imaptest` (`src/tests`, 112 Dateien)
und die Beispiel-Mailbox `dovecot-crlf`. Läuft als Job im Namespace `shardpost`
gegen `shardpost:993` (TLS, eigene CA) mit einem Testkonto aus dem Secret
`imaptest-creds` (Keys `user`, `pass`):

```bash
imaptest host=shardpost port=993 ssl=any-cert user=... pass=... mbox=/mbox test=/tests
```

Die Job-Vorlage liegt in `hack/imaptest-job.yaml` (restricted
PodSecurity, Secret `imaptest-creds`); Ergebnis und Bewertung stehen unten.

## Sieve: Pigeonhole-Testsuite

Der go-sieve-Fork bringt den Dovecot-Testsuite-Dialekt (`vnd.dovecot.testsuite`)
mit. Mit einem Checkout der Pigeonhole-Tests unter
`third_party/go-sieve/pigeonhole/tests` (Symlink `tests/pigeonhole`):

```bash
cd third_party/go-sieve
SIEVE_CONFORMANCE=1 GOFLAGS=-mod=mod go test ./tests -run TestPigeonholeConformance -v | grep "SIEVE CONFORMANCE\|  FAIL"
```

## Ergebnisse 0.11.2 (16.09.2026)

Das Serverbinary in 0.11.2 ist identisch zum vollständig geprüften 0.11.1.
Der zusätzliche Helperfix für HTTP 204 besteht eigene HTTP-/spamc-Vertragstests
auch gegen den installierten Helper im finalen Image.
Die sequenzielle Linux-Abnahme aus einer festen Quellkopie besteht:
`go test -p 1 -race ./...` im Sieve-Fork und
`go test -p 1 -race -tags tentacle ./...` im Serverprojekt.

Pigeonhole: **139 bestanden, 0 fehlgeschlagen, 9 ausdrücklich ausgenommen,
0 ungeprüft von 148 Dateien**. Alle zehn extprograms-Dateien und der bisher
hängende CPU-Limit-Test bestehen. Die neun Ausnahmen umfassen sechs Dateien
für nicht angebotene Erweiterungen, die Dovecot-spezifische Diagnoseprüfung
und zwei ungültige historische Crash-Szenarien. Compiler-Ablehnung,
Fehlerpfade und Flags-Isolation werden zusätzlich separat geprüft.

Der Adapter vergleicht bei Fehleranzahlen weiterhin Fehlerfreiheit gegen
Fehlerzustand, nicht die exakte Anzahl der Dovecot-Diagnosen. Details und
Evidenz stehen im [Korrekturbericht](../docs/reports/2026-09-16-sieve-conformance-fixes.md).
Ausnahmen zählen nie als bestanden. Der vollständige Sieve-Lauf benötigt
keinen Ausschluss des CPU-Tests mehr. Plattformreferenz: Linux mit `TZ=UTC`;
die unveränderten extprograms-Testprogramme erwarten Linux-Shellverhalten.

Öffentliche Abnahme 0.11.2: beide Konten bestanden einschließlich
CPU-/MIME-Abbruch mit bytegleichem Original und erfolgreichem Folgeskript.
Vier neue Lernaufträge bestätigt (2 Spam, 2 Ham), keine neuen Retries/Dead-Jobs,
Lernqueue leer. Beide Produktionspods bereit, keine Neustarts.

## Ergebnisse 0.11.0 (16.09.2026)

Linux-Fork-Build und reguläre Fork-Tests bestehen. Die bisher fehlende
Adress-Fixture wird direkt aus der vorhandenen `.svtest.in`-Quelldatei gelesen;
die unveränderten, dokumentierten Ausnahmen im Adresstest bleiben bestehen.

Pigeonhole: **127 bestanden, 20 fehlgeschlagen, 1 separat geprüfte
CPU-Limit-Datei von 148**. Alle zehn extprograms-Dateien bestehen.
Die 20 übrigen Fehler betreffen weiterhin nicht implementierte Erweiterungen
und vorhandene allgemeine Abweichungen; es ist keine vollständige
Sieve-Konformitätsfreigabe. Die Rohprotokolle und die genaue Bewertung stehen
im [Abnahmebericht](../docs/reports/2026-09-16-extprograms.md).

Der CPU-Limit-Test wurde separat nach 20 Sekunden äußerer Zeitgrenze
abgebrochen und ist nicht bestanden.
Für den übrigen Lauf:

```bash
SIEVE_CONFORMANCE=1 GOFLAGS=-mod=mod go test ./tests \
  -run TestPigeonholeConformance \
  -skip 'TestPigeonholeConformance/execute/errors-cpu-limit.svtest' -v
```

Die Zusammenfassung zählt herausgefilterte Dateien ausdrücklich als nicht
ausgeführt. Sie werden nicht als bestanden gezählt.

## Ergebnisse (15.09.2026, Image 0.8.3)

Sieve (Image 0.9.0, Fork-Stand 15.09.2026): **117 von 148 Dateien** bestehen
(0.8.3: 50). Umgesetzt wurden dafür ihave, include (mit global/return,
:once/:optional, Zyklus- und Tiefenlimits), index, regex, spamtest/
spamtestplus/virustest (Header und Schwellen konfigurierbar), mboxmetadata/
servermetadata, special-use, enotify mit mailto, foreverypart/break, mime
(:type/:subtype/:contenttype/:param, :anychild) und extracttext; außerdem
Testsuite-Umgebung (Mailstore, SMTP-Ausgang, Mailbox-Anlage, Metadaten,
Konfigurationsschalter, Dateien der Pigeonhole-Beispiele). Die 31 offenen
Dateien: extprograms/vnd.dovecot.* (Dovecot-eigene Plugins: execute,
filter, pipe, report, environment, 12 Dateien), extlists (kein
Listen-Backend, 3), multiscript/test_multiscript (4), Fuzz-Fixtures mit
absichtlich kaputten Headern (2), Parser-Recovery und Fehlerzählung
(compile/errors, recover, mime/errors, editheader/errors: unser Loader
meldet den ersten Fehler, Pigeonhole zählt alle), CPU-Limit, lokale
Zeitzone (date/zones), zwei Vacation-Details (Subject-Kürzung mit
Auslassungszeichen, Reply-Regeln in Sonderfällen) und ein
Redirect-Loop-Fall im SMTP-Test.

## IMAP-Ergebnis 0.11.0 (16.09.2026)

Die vollständige Suite am Release-Kandidaten ergab 72 Gruppen: **1 fehlgeschlagen,
11 wegen fehlender Capabilities übersprungen**. Basisprotokoll: 1/460
Kommandos fehlgeschlagen; Erweiterungen: 0/261. Einziger Fehler bleibt
`fetch-envelope` bei veralteten Source-Routes. Keine Nauthilus-UNAVAILABLE-
oder Initialisierungsfehler in diesem Lauf. Die öffentliche native Abnahme
mit beiden Konten sowie vier Rspamd-Lernbestätigungen bestand nach dem Rollout.

## IMAP-Ergebnis (15.09.2026, Image 0.8.7; 0.8.8 nur Bugfix, siehe docs/review-round6.md)

Verlauf: 0.8.3 26 von 72 Gruppen rot, 0.8.4 9 rot, 0.8.6/0.8.7 **1 rot**
(72 Gruppen, 11 wegen fehlender Capabilities übersprungen; Basisprotokoll
1 von 441 Kommandos, Erweiterungen 0 von 261; dazu je Lauf 0 bis 2
Gruppen, deren LOGIN an einem Nauthilus-Tempfail scheiterte, siehe Hinweis).

Behoben zwischen 0.8.4 und 0.8.6: STATUS RECENT (die erste Sitzung, die
eine Nachricht sieht, nimmt ihr \Recent), ESEARCH MIN/MAX mit MODSEQ der
gelieferten Teilmenge, RFC-5322-Adressparser für ENVELOPE (Gruppen,
Kommentare, Sender/Reply-To-Default auf From), SEARCH FROM/TO/CC/BCC und
HEADER <adresse> auch gegen die geparsten Adressen, SORT nach Gruppen-
namen, THREAD=ORDEREDSUBJECT mit der ältesten Nachricht als Wurzel,
MOVE in die selektierte Mailbox (EXPUNGE vor EXISTS), LIST CHILDINFO nur
für Kinder, die kein Pattern selbst listet, Abo überlebt DELETE
(\NonExistent), FLAGS/PERMANENTFLAGS vor jedem FETCH mit neuem Keyword,
APPEND mit Content-Transfer-Encoding: binary wird nach base64 umkodiert.

Offen: `fetch-envelope` erwartet für `<@route:user@domain>` die
Source-Route im ENVELOPE (`(NIL "@route" "user" "domain")`); der Parser
verwirft die veraltete Route. Bewusst nicht umgesetzt (go-imap kennt das
Feld nicht, kein Client braucht es).

Hinweis: Ein Lauf kann zusätzlich an einer `NO [UNAVAILABLE]`-Antwort
scheitern (imaptest meldet dann "initialization failed"); das ist kein
Serverfehler, sondern ein sporadischer Tempfail von Nauthilus nach
erfolgreicher Authentifizierung (siehe Abschlussbericht vom 15.09.2026).
