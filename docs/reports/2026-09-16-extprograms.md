# Herstellerneutrale IMAPSieve-Programme: Review und Abschluss

Status: **am 16.09.2026 abgeschlossen und produktiv ausgerollt: 0.11.0**.
Beide Serverreplikas sind bereit, der Scrub-CronJob referenziert denselben
veröffentlichten Digest. Öffentliche Abnahme mit beiden Testkonten bestanden;
vier Lernaufträge über den Python-Helper bestätigt (zweimal Spam, zweimal Ham).

## Ergebnis der Übernahme

Claudes unterbrochene Arbeit vom 16.09.2026 wurde anhand des Gesprächsverlaufs
und der Quellen rekonstruiert. Eine vollständige Ausgangssicherung liegt unter
`/private/tmp/imapcephpoc-before-extprograms-review-20260916.tgz` (Modus 0600).
Der Workspace besitzt weiterhin kein Git-Repository; daher kein Commit oder Git-Tag.

Der Go-Server startet konfigurierbare Programme über eine Allowlist. Die
Rspamd-Anbindung liegt in `programs/learn.py`; der dauerhafte Lernpfad bleibt
Ceph-gestützt, mit Lease, Commitnachweisen, Retry und Fehleraufträgen.

## Review und Korrekturen

- **Nachrichtenerhalt bei Pipe-Fehlern:** LMTP fällt auf das Original in INBOX
  zurück; IMAP setzt bei fehlgeschlagenem Pipe kein Deleted. `:try` hebt den
  impliziten Keep bei Fehlschlag nicht auf. Doppelte Pipes auf denselben
  Programmnamen und die Kombination mit Reject werden abgewiesen.
- **Filter und Aktionsreihenfolge:** Filter sehen vorherige Headeränderungen;
  nachfolgende Aktionen sehen das neue Ergebnis. Bereits vorgemerkte Aktionen
  behalten ihre eigene Nachrichtenversion. `execute :output` behält Zeilenenden.
  Leere Filterausgabe ersetzt keine Nachricht. Die Zeilenenden-Konfiguration
  für Execute/Filter und gegenseitig ausschließende Tags sind geprüft.
- **Identitätsgrenze:** Auf fremden Mailboxen werden keine Programme mit der
  Identität des Eigentümers gestartet, selbst wenn der Auslöser ACL-Rechte hat.
- **Konfiguration:** Viper-normalisierte Environment-Namen werden wieder in
  Großbuchstaben überführt; Lernregeln müssen ein Programm mit passender
  `learning`-Freigabe nennen. Variablenwerte werden nicht verändert.
- **Prozessgrenzen:** Der Timeout umfasst auch das Warten auf einen freien
  Prozessplatz. Programmdatei und Programmverzeichnis werden geprüft;
  Symlinks und gruppen-/weltschreibbare Einträge werden zurückgewiesen.
  Skriptargumente werden auch am Runner begrenzt und validiert.
- **Queue-Kompatibilität:** Die Produktionsprogramme heißen weiterhin `spam`
  und `ham`. Dadurch sind gespeicherte Aufträge und gleichzeitig laufende
  0.10.1-Worker kompatibel. Go enthält keine Sonderbehandlung dieser Namen.
  Der Enqueue-Pfad speichert nun auch Eigentümer und Mailbox-ID.
- **Python-Helper:** Keine HTTP-Redirects mit dem Token, begrenzte Antworten,
  Behandlung von 208, JSON-Typprüfung und genaue Already-learned-Erkennung.
  Der optionale spamc-Aufruf nutzt `-x` und geprüfte Exitcode-Zuordnung.
- **Testumgebung:** Nachrichtenkopien sind zwischen Testblöcken isoliert;
  Programmumgebung folgt dem jeweiligen Aufruf. Die vorhandene Adress-Fixture
  wird als `.svtest.in` gelesen. Ausgelassene Konformitätsdateien zählen nicht
  länger irrtümlich als bestanden.

Semantikgrundlage: [Pigeonhole extprograms specification](https://github.com/dovecot/pigeonhole/blob/main/doc/rfc/spec-bosch-sieve-extprograms.txt).
Der optionale spamc-Vertrag orientiert sich an der [Apache-Dokumentation](https://spamassassin.apache.org/full/4.0.x/doc/spamc.html).

## Prüfung

| Prüfung | Ergebnis |
|---|---|
| Gesamtes Serverprojekt: Linux `go test -race -tags tentacle ./...` | bestanden |
| Fork: `go build ./...` und `go test ./...` | bestanden |
| Pigeonhole extprograms | alle 10 Dateien bestanden |
| Pigeonhole gesamt | 127 bestanden, 20 fehlgeschlagen; CPU-Limit-Datei separat |
| Isolierter CPU-Limit-Test | nach 20 Sekunden äußerem Limit abgebrochen, nicht bestanden |
| Python-Helper HTTP- und spamc-Verträge | bestanden; nur synthetische Daten und Token |
| Aktive gegen vorgeschlagene Serverkonfiguration | ausschließlich learning/extprograms verändert |
| Kandidatenressourcen: Kubernetes-Server-Dry-run | bestanden |
| Finales Image: Build mit `go vet`, Python, `config check` | bestanden; UID 167, Helper und Verzeichnis root/0755 |
| Kandidat: beide Konten, native IMAPSieve-Abnahme | bestanden; APPEND, FETCH/Seen, MOVE und Shardgrenze 20 |
| Kandidat: echte extprograms-Aufrufe, beide Konten | Pipe-Erfolg, Pipe-Fehler, Pipe-try-Fehler und Execute bestanden |
| Kandidat: Lernqueue | 4 vorbereitet; 3 vom Kandidaten-Helper bestätigt; vierter Auftrag nicht separat korreliert bei parallel aktiven alten Workern |
| Dovecot imaptest am Kandidaten | 72 Gruppen: 1 bekannter Fehler, 11 Capability-Skips; Basis 1/460, Erweiterungen 0/261 Fehler |
| Nauthilus im imaptest-Lauf | keine UNAVAILABLE-/Initialisierungsfehler |
| Docker Hub | Push und Manifest-Readback erfolgreich |
| Produktion: öffentliche TLS-Abnahme über Director, beide Konten | bestanden einschließlich höherer Ceph-Shards |
| Produktion: neue Lernbestätigungen | exakt 4; Helper zweimal spam und zweimal ham erfolgreich |
| Produktion: Image-/Konfigurationsparität | beide Pod-imageIDs, Deployment, ConfigMap und Scrub-CronJob geprüft |
| Postflight | 2/2 bereit, keine Warn-/Fehlermeldungen im Prüfzeitraum |
| Temporäre Ressourcen | Kandidat, Kandidaten-Service/-ConfigMap/-NetworkPolicy, imaptest-Job und eigener lokaler Tunnel entfernt |

Der gleichartige Linux-Vergleich mit Claudes gesichertem Ausgangsstand
zeigt **keine neuen fehlgeschlagenen Dateien**. Drei vorher rote Dateien
sind jetzt grün; der CPU-Test wird in beiden Vergleichen separat behandelt.

Die 20 sonstigen Pigeonhole-Fehler betreffen Parser-/Fehlerzählung,
Multiscript, extlists, weitere Dovecot-Plugins, Fuzz-Fixtures und bestehende
SMTP/Vacation/Notify-Abweichungen. Sie sind keine bestandene Vollkonformität.
Der CPU-Limit-Test bleibt eine eigenständige bekannte Lücke außerhalb dieser
extprograms-Umstellung. Die Quelldateien von Pigeonhole wurden nicht geändert.

Der einzelne imaptest-Fehler ist der bereits dokumentierte Unterschied bei
veralteten Source-Routes in `fetch-envelope`; keine neue IMAP-Abweichung.
Die bewusst ausgelösten Kandidatenfehler ergaben vier fehlgeschlagene
Testprogrammaufrufe und zwei Apply-Fehler, jeweils wie erwartet bei erhaltenen
Nachrichten. Diese Testprogramme sind nicht in der Produktionskonfiguration.
Ein erster Tunnelversuch traf auf bereits belegte lokale Ports; die Abnahme
wurde ausschließlich über anschließend bestätigte eigene Ports durchgeführt.

Prüfprotokolle (lokal):
- `/private/tmp/extprograms-server-verified.log`
- `/private/tmp/extprograms-conformance-complete.log`
- `/private/tmp/extprograms-conformance-summary.txt`
- `/private/tmp/extprograms-cpu-limit.log`
- `/private/tmp/extprograms-release-build.log`
- `/private/tmp/extprograms-baseline-conformance.log`
- `/private/tmp/extprograms-imaptest.log` (0600)
- `/private/tmp/extprograms-candidate-native.log`
- `/private/tmp/extprograms-production-native.log`
- `/private/tmp/extprograms-production-metrics-before.json`
- `/private/tmp/extprograms-production-metrics-after.json`
- `/private/tmp/extprograms-push.log`

Ein früher paralleler Testlauf war wegen gleichzeitig erneuertem Vendorbaum
ungültig. Der erfolgreiche vollständige Lauf verwendet eine feste Quellkopie.

## Rollout und Rollback

Der Benutzer hat Docker-Hub-Push und Cluster-Rollout ausdrücklich freigegeben.
Veröffentlicht: **0.11.0**, vor dem Push war der Tag nicht vorhanden.
Digest: `sha256:75b1361e550f6eef7ec4d11b1deb3497ef9ab6c2ee9e9666f91c733812af4318`.
Image-Build einschließlich `go vet`, lokales `config check` und Registry-Readback
werden als Freigabekette protokolliert.
Produktivänderungen wurden auf Server-Deployment, dessen ConfigMap und das
Image des Scrub-CronJobs begrenzt. Director, Rspamd und Ceph-Konfiguration
bleiben außerhalb der Änderung.

Vorheriges Image:
`docker.io/chrroessner/imapcephpoc:0.10.1@sha256:f9bdea21100a3bcd76a1e8de40fec52f4e70109b2e6770247307c79e8a25574b`.

Rollback-Sicherungen (0600):
- `/private/tmp/imap-extprograms-deployment-before.json`
- `/private/tmp/imap-extprograms-scrub-before.json`
- `/private/tmp/extprograms-config-before.json`

Die bisherige ConfigMap `imap-server-config-gcc6246d4t` bleibt für den Rollback verfügbar;
aktiv ist `imap-server-config-dk4c766t5f`. Rollback
stellt das alte Image und die alte ConfigMap gemeinsam wieder her;
kein Löschen von Mail- oder Queue-Daten. Das spätere Umbenennen von
Programmnamen erfordert eine eigene Queue-Übergangsstrategie.

Externe Programmaufrufe und Ceph-Änderungen bilden keine gemeinsame
Transaktion. Wiederholte externe Effekte sind bei Abbrüchen möglich;
Exactly-once wird nicht zugesichert. Der optionale spamc-Backendvertrag wurde
synthetisch geprüft, nicht gegen einen produktiven SpamAssassin-Dienst.
