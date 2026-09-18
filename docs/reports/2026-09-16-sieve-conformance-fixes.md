# Sieve-Korrekturen nach 0.11.0

Status: **am 16.09.2026 vollständig abgeschlossen und produktiv abgenommen:
0.11.2, beide Replikas bereit, vier neue Lernaufträge bestätigt.** Bearbeitung ausdrücklich nacheinander, ohne Subagenten.
Die ausgeschlossene historische IMAP-Adressdarstellung wird nicht geändert.

## Behobene Ursachen

- CPU-/Laufzeitbudget: reguläre Skripte erhalten standardmäßig fünf Sekunden
  verstrichene Zeit. `sieve_max_cpu_time` kann ein positives Zeitintervall
  vorgeben; ein kürzerer Kontext des Aufrufers gewinnt (IMAPSieve: 500 ms in
  der aktiven Konfiguration). Schleifen, Includes und Regex-Eingabereader
  prüfen den Abbruch. Keine abgekoppelte Rechengoroutine läuft danach weiter.
- MIME: höchstens vier verschachtelte `foreverypart`-Schleifen, auch über
  Includes. Der MIME-Baum ist auf 1000 Teile, Tiefe 20 und 256 KiB Header je
  Teil begrenzt. Die Zerlegung prüft den Kontext und verwendet Body-Slices;
  eine überschrittene Grenze bricht den Lauf ab.
- Parser: überflüssige Kommas, mehrere Address-Part-Tags, unbekannte
  Envelope-Felder, unzulässige numerische Vergleichsarten, fehlende
  `foreverypart`-Abhängigkeit und ungültige Redirect-Adressen werden
  abgewiesen. Variable Redirect-Ziele werden zusätzlich zur Laufzeit geprüft.
  Zulässige Leerzeichen in gültigen Adressen bleiben erhalten.
- Vergleiche: Nicht-ASCII-Literale werden beim Byte-Regex korrekt kodiert;
  dadurch funktionieren unter anderem Vergleiche mit Auslassungszeichen.
- Vacation: `-request` wird am lokalen Adressteil erkannt. Der produktive
  Adapter antwortet niemals auf einen leeren SMTP-Absender. Antwortheader
  behalten passende Anzeigenamen und begrenzte References; MIME-kodierte
  Betreffzeilen werden vor der Kürzung dekodiert. Konfigurierte eigene
  Adressen und Empfängerprüfungen werden berücksichtigt. Der Standard für
  den Antwort-Envelope bleibt der Nullsender.
- Testumgebung: Flags und Testzustand werden korrekt kopiert; Fehler-Keep
  verwendet das Original; SMTP-Schleifenprüfungen berücksichtigen Benutzer-
  und Zustelladresse. Mehrere Skripte teilen Flags/Nachrichtenänderungen,
  führen neue Aktionen genau einmal aus und führen Ergebnisse zusammen.
  Ein indizierter Aktionstest prüft tatsächlich nur den gewählten Index.
- Gespeicherte Skripte behalten Konfiguration und Aktionslimit.

## Prüfungen und Grenzen

Die feste Quellkopie liegt unter `/private/tmp/imap-sieve-fixes-verified`.
Linux-Sieve-Gesamtlauf mit Race-Detector: **139 bestanden, 0 fehlgeschlagen,
9 ausdrücklich ausgenommen, 0 ungeprüft von 148 Dateien**. Der frühere
CPU-Test besteht jetzt nach rund zwei Sekunden. Tests für Regex-Abbruch,
MIME-Grenzen und Flags-Isolation laufen zusätzlich. Der vollständige
Serverlauf `go test -p 1 -race -tags tentacle ./... -count=1` besteht
ebenfalls, einschließlich der neuen Produktionsadapter-Prüfungen.

Die neun Ausnahmen sind keine bestandenen Tests:

| Dateien | Grund / Ersatznachweis |
|---|---|
| 3 extlists | Kein Listen-Backend, Erweiterung nicht angeboten. |
| 2 Dovecot environment | Optionales Dovecot-Plugin nicht angeboten. |
| 1 Dovecot report | Optionales Dovecot-Plugin nicht angeboten. |
| compile/errors.svtest | Dovecot sammelt mehrere Fehler und prüft eigene Fehlertexte. Unser Compiler meldet den ersten Fehler. Der wieder aktivierte `TestCompileErrors` prüft alle dort aufgeführten ungültigen Skripte auf tatsächliche Ablehnung; die Dovecot-Diagnosetext-Konformität wird nicht behauptet. |
| failures/fuzz1.svtest | Historische Crash-Datei verlangt eine nie erstellte Mailbox und nie gesetzte Flags. Fehlender-Mailbox-Pfad und echte Flags-Kopien werden separat geprüft. |
| failures/fuzz4.svtest | Historische Crash-Datei enthält ungültige Notify-Syntax; deren Ablehnung wird separat geprüft. |

Weitere `test_error :count`-Prüfungen des vorhandenen Adapters vergleichen
Fehlerfreiheit gegen Fehlerzustand, nicht die exakte Anzahl aller Dovecot-
Diagnosen. Die Zahl 139 bedeutet daher bestandene Dateien im dokumentierten
Adapter, keine uneingeschränkte Dovecot- oder RFC-Vollkonformität.
Die Pigeonhole-Originaldateien wurden nicht verändert.

Ein macOS-Lauf zeigte zusätzlich lokale Zeitzonen-/Shell-Unterschiede.
Maßgeblich ist die Linux-Prüfung mit UTC wie bei der bisherigen Abnahme.

Protokolle:
- `/private/tmp/sieve-linux-final.log`
- `/private/tmp/sieve-server-final.log`
- `/private/tmp/sieve-cpu-fixed.log`


## Veröffentlichung und Cluster

Docker-Hub-Tag: `docker.io/chrroessner/imapcephpoc:0.11.1`.
Digest: `sha256:a59e05f489e8ee6cc8030c468e77f2f5e039c408ee1c9d0adad07dfdb61343bf`.
Registry-Readback stimmt mit dem getesteten lokalen Image überein.
Image-Build mit `go vet` und `config check` bestanden; Benutzer `167:167`.
Eine erste automatische Push-Prüfung verlangte konkretere Ziel-/Payload-
Nachweise. Repository, angemeldeter Benutzer und Vergleich mit 0.11.0 wurden
verifiziert; der anschließende Push wurde freigegeben und ausgeführt. Im
Laufzeit-Image änderte sich von den Programmdateien ausschließlich das
Serverbinary, nicht imapload oder learn.py. Keine Quell-/Secret-Verzeichnisse
wurden hinzugefügt.

Isolierter Kandidat mit unveränderter Produktionskonfiguration: beide Konten
bestanden. CPU-Fälle einschließlich IMAP-Antwort nach 0,667/0,645 Sekunden,
MIME-Fälle nach 0,203/0,168 Sekunden; Nachricht jeweils bytegleich erhalten,
Folgeskript erfolgreich. Vier absichtlich ausgelöste Skriptfehler und vier
vorbereitete Lernaufträge, keine weiteren Warn-/Fehlermeldungen. Die
Lernzustellung am Kandidaten wurde wegen gleichzeitig aktiver alter Worker
nicht dem Kandidaten allein zugerechnet.

Produktiv geändert wurden ausschließlich die Image-Referenzen von
Deployment `imap-server` und CronJob `imap-scrub`. JSON-Patch-Vorbedingungen
prüften den bisherigen 0.11.0-Digest. Beide neuen Pods sind bereit und ihre
Image-IDs entsprechen dem Registry-Digest. ConfigMap und übrige Konfiguration
bleiben unverändert. Temporärer Kandidat, Kandidaten-Netzrichtlinie und der
eigene lokale Tunnel wurden entfernt.

Rollback: das gesicherte 0.11.0-Image
`docker.io/chrroessner/imapcephpoc:0.11.0@sha256:75b1361e550f6eef7ec4d11b1deb3497ef9ab6c2ee9e9666f91c733812af4318`
in Server-Deployment und Scrub-CronJob zurücksetzen. Die Konfiguration wurde
nicht verändert. Ausgangsobjekte (0600):
`/private/tmp/sieve-fixes-deployment-before.json` und
`/private/tmp/sieve-fixes-cronjob-before.json`.

Weitere Belege:
- `/private/tmp/sieve-release-build.log`
- `/private/tmp/sieve-release-push.log`
- `/private/tmp/sieve-release-manifest.json`
- `/private/tmp/sieve-candidate-native.log`
- `/private/tmp/sieve-production-native.log`


## Zusätzlich gefundener Helperfehler und 0.11.2

Die erste öffentliche Lernabnahme unter 0.11.1 ergab drei bestätigte von vier
Aufträgen. Ein Ham-Testauftrag erhielt fünf temporäre Fehler und wurde als
Dead gespeichert. Es gab weder einen Queue-Lesefehler noch einen neuen
Interpreterfehler: Der direkte, begrenzte Nachweis am gesicherten Testinhalt
ergab **HTTP 204 ohne Body**. Der Helper behandelte nur 208 als Erfolg ohne
JSON und stufte die 204-Antwort fälschlich als JSON-Fehler/Retry ein.

`learn.py` akzeptiert jetzt ausschließlich 204 und 208 ohne JSON; ein leeres
200, fehlerhaftes JSON, Ablehnungen, Redirects und 5xx werden weiterhin nach
dem bisherigen Vertrag behandelt. Die vollständigen HTTP-/spamc-Vertragstests
bestehen im Linux-Image und gegen den tatsächlich installierten Helper.
Am isolierten 0.11.2-Kandidaten bestätigte der Helper den identischen zuvor
fehlgeschlagenen Testinhalt mit Exitcode 0.

**Finaler Release: 0.11.2**
`docker.io/chrroessner/imapcephpoc:0.11.2@sha256:8e51fecb8d7539c4f4654d5718799fe8bfb7140f9e878443a8e9e725d5abea3e`.
Das Image wurde vom unveränderlichen 0.11.1-Digest abgeleitet und ersetzt
nur `/usr/local/lib/imapcephpoc/programs/learn.py`. Der normale Dockerfile
verwendet dieselbe korrigierte Quelle für zukünftige vollständige Builds.

- Serverbinary in 0.11.1 und 0.11.2:
  `sha256:4e36e9c2c78deda62a7e7b7fa723abbb3b3ddd1770129e1ace65582376de4458`
- Installierter Helper und korrigierte Quelle:
  `sha256:25f2544b107b52ca4ac73b316d5c57b1875d6e98bb8976647952dbf2b2def920`
- Registry-Manifest stimmt mit dem lokalen finalen Image überein.
- `/private/tmp/Dockerfile.sieve-helper-fix`,
  `/private/tmp/sieve-helper-build.log`,
  `/private/tmp/sieve-helper-push.log`,
  `/private/tmp/sieve-helper-manifest.json` dokumentieren den ergänzenden Build.

Der initiale fehlgeschlagene Testauftrag wurde nach Metadaten-/Payload-
Gleichheitsprüfung genau einmal wieder freigegeben: Dead=false, Attempts=0,
Next=jetzt. Der unveränderte Payload und die Zustellnachweise blieben erhalten.
Der produktive 0.11.2-Worker bestätigte exakt eine erfolgreiche Ham-Ausführung
und eine Zustellung und entfernte den Auftrag anschließend regulär aus der
Queue. Es wurde kein Erfolg manuell gesetzt und kein Auftrag manuell gelöscht.
Die Sicherung verbleibt lokal mit Modus 0600.


## Endgültige öffentliche Abnahme 0.11.2

Beide Konten bestanden über den öffentlichen TLS-/Director-Zugang die native
IMAPSieve-Abnahme einschließlich APPEND, FETCH/Seen, MOVE und Shardgrenze 20.
CPU-Fälle: 0,694 und 0,652 Sekunden einschließlich IMAP-Antwort; MIME-Fälle:
0,195 und 0,133 Sekunden. Originalnachrichten jeweils bytegleich erhalten,
keine Deleted-Markierung durch das fehlgeschlagene Skript, unmittelbar
anschließend erfolgreiches normales Folgeskript.

Frischer Zählervergleich nach der gesonderten Wiederaufnahme:

| Ergebnis | Delta |
|---|---:|
| Vorbereitete Lernaufträge | 4 |
| Vom Worker bestätigte Zustellungen | 4 |
| Helper spam / ok | 2 |
| Helper ham / ok | 2 |
| Retry / Dead / tempfail | 0 |
| Absichtlich ausgelöste Sieve-Abbrüche | 4 |
| Sonstige Warn-/Fehlermeldungen | 0 |

Die Lernqueue ist leer. Beide produktiven Pods
`imap-server-dd5cf558d-6fx6s` und `imap-server-dd5cf558d-n9hpk` sind bereit,
haben null Neustarts und verwenden den finalen 0.11.2-Digest. Deployment und
Scrub-CronJob sind auf diesen Digest festgelegt. Ein Vergleich des gesamten
Pod-Spec gegen die Ausgangssicherung ergab ausschließlich den Imagewechsel;
ConfigMap `imap-server-config-dk4c766t5f` bleibt dieselbe.
Beide Kandidatenläufe sind bereinigt, der eigene Portforward ist beendet.

Endbelege:
- `/private/tmp/sieve-final-native.log`
- `/private/tmp/sieve-final-metrics-before.json`
- `/private/tmp/sieve-final-metrics-after.json`
- `/private/tmp/sieve-final-postflight.json`
- `/private/tmp/sieve-recovery-metrics-before.json`
- `/private/tmp/sieve-recovery-metrics-after.json`

Der Arbeitsordner besitzt kein Git-Repository; es wurden daher keine
Git-Commits oder Git-Tags erzeugt. Quellcode, Vendor-Fork, Release-Overlay und
Dokumentation enthalten den fertigen Stand. Die ausgeschlossene historische
IMAP-Adressdarstellung wurde nicht bearbeitet.
