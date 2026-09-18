# IMAPSieve, externe Programme und Lernen

IMAPSieve reagiert nach dem Speichern auf APPEND/MULTIAPPEND, COPY, MOVE (Sieve-Ursache COPY), tatsächliche STORE-Flagänderungen und FETCH mit gesetztem Seen-Flag. Ein unverändertes STORE und BODY.PEEK lösen keinen Flaglauf aus. Skripte laufen außerhalb der Verbindungsschreibsperre; Ergebnisänderungen gelangen über den vorhandenen IMAP-Benachrichtigungspfad an Clients.

```yaml
imapsieve:
  enabled: true
  user_scripts: true
  managesieve_url: sieve://mail.example.org:4190
  timeout_ms: 500
  max_parallel: 4
  learning:
    enabled: true
    max_attempts: 5
    max_message_bytes: 4194304
    rules:
      - name: spam
        operations: [COPY, MOVE]
        destination_special_use: '\Junk'
        source_not_special_use: '\Junk'
        program: spam
      - name: ham
        operations: [MOVE]
        source_special_use: '\Junk'
        destination_not_special_use: ['\Junk', '\Trash']
        program: ham
extprograms:
  bin_dir: /usr/local/lib/shardpost/programs
  programs:
    spam:
      command: learn.py
      args: ["--action", "spam"]
      kinds: [learning]
      timeout_ms: 30000
      env: {LEARN_BACKEND: rspamd, LEARN_URL: "http://classifier:11334", LEARN_TOKEN_FILE: /run/secrets/learning/token}
    ham:
      command: learn.py
      args: ["--action", "ham"]
      kinds: [learning]
      timeout_ms: 30000
      env: {LEARN_BACKEND: rspamd, LEARN_URL: "http://classifier:11334", LEARN_TOKEN_FILE: /run/secrets/learning/token}
```

## Externe Programme (extprograms)

Der Server kennt keinen Klassifizierer und keine Shell. Alles, was ein
fremdes Programm startet, läuft über die Tabelle `extprograms.programs`,
nach dem Muster einer sudoers-Regel: der Name ist der Schlüssel, der
Eintrag ist genau eine Kommandozeile. Ein Sieve-Skript oder eine Lernregel
kann nur den Namen wählen.

* `command` liegt unter `bin_dir`, ohne Verzeichnisanteil, muss eine
  reguläre Datei sein (kein Symlink) und darf für Gruppe und Andere nicht
  schreibbar sein. Das Verzeichnis gehört root, der Server läuft als
  unprivilegierter Benutzer.
* `args` sind fest; `{account}` und `{mailbox}` werden ersetzt, bei
  Quota-Programmen zusätzlich die Quota-Platzhalter (siehe `docs/quota.md`). Skripte
  dürfen nur mit `allow_script_args: true` eigene Argumente anhängen
  (Zeichenfilter: keine Steuerzeichen, höchstens 64 Argumente zu je 1024 Bytes).
* `kinds` beschränkt die Verwendung auf `execute`, `pipe`, `filter`,
  `learning` oder `quota` (Quota-Warnungen und Over-Flag); leer heißt alle.
* Umgebung: nur `PATH`, `LANG`, `HOME`, die Einträge aus `env` und die
  Aufrufwerte (bei `quota` die `QUOTA_*`-Werte statt dieser) `SIEVE_ACCOUNT`, `SIEVE_MAILBOX`, `SIEVE_CAUSE`,
  `SIEVE_CHANGED_FLAGS`. Im Lernworker enthält `SIEVE_MAILBOX` die stabile
  Mailbox-ID; bei Sieve-Ereignissen den Mailboxnamen. Die Nachricht kommt auf stdin, stdout wird bis
  `max_output_bytes` gelesen (Standard 1 MiB), `timeout_ms` (Standard
  10 s) beendet die Prozessgruppe, einschließlich des Wartens auf einen freien Ausführungsplatz. `max_parallel` (Standard 4) begrenzt
  gleichzeitige Läufe je Eintrag.
* Exit-Codes: 0 Erfolg, 75 (`EX_TEMPFAIL`) vorübergehender Fehler,
  alles andere endgültig. Metrik `imap_extprograms_runs_total{program,result}`.

In Sieve stehen damit Pigeonholes Erweiterungen zur Verfügung, bei der
Zustellung wie in IMAPSieve-Skripten, mit den Capability-Namen
`vnd.dovecot.execute`, `vnd.dovecot.pipe` und `vnd.dovecot.filter`
(ManageSieve zeigt sie nur an, wenn Programme konfiguriert sind):

```sieve
require ["vnd.dovecot.execute", "vnd.dovecot.pipe", "vnd.dovecot.filter", "variables", "copy"];
if execute :pipe :output "score" "classify" { ... }   # Programm liest die Nachricht, stdout in ${score}
pipe :copy "archive";                                  # Kopie an ein Programm, Zustellung geht weiter
filter "rewrite";                                      # stdout des Programms ersetzt die Nachricht
```

`execute` und `filter` laufen während der Skriptauswertung. `pipe` wird
als Zustellaktion vorgemerkt und vor dem Verwerfen beziehungsweise dem
Setzen von `\Deleted` ausgeführt. Ein Fehler ohne `:try` erhält bei IMAP
das Original und führt bei LMTP zur sicheren INBOX-Zustellung. Ein
fehlgeschlagenes `pipe :try` wirkt wie eine nicht ausgeführte Aktion und
hebt den impliziten Keep nicht auf. Ein Programm darf pro Skript nur eine
Pipe-Aktion erhalten; Pipe und Reject sind unvereinbar.

`execute` und `filter` sind auch als Tests verwendbar. Programmnamen sind
Konstanten. Filter und Headeränderungen gelten für nachfolgende Aktionen;
bereits vorgemerkte Aktionen behalten ihre eigene Nachrichtenversion.
Die Ausgabe von `execute :output` wird einschließlich Zeilenenden erhalten.
Die Eingabe dieser beiden Aufrufe verwendet standardmäßig LF;
`sieve_execute_input_eol=crlf` beziehungsweise `sieve_filter_input_eol=crlf`
in `sieve.settings` kann CRLF wählen. Die dauerhafte Lernqueue übergibt die
Originalnachricht unverändert an den Programmstarter.

Externe Programme dürfen im IMAP-Pfad nur durch den Mailbox-Eigentümer
aufgerufen werden. Ein Zugriff auf eine geteilte Mailbox darf keine
externen Seiteneffekte mit dessen Identität auslösen. Programmaufrufe und
Ceph-Schreiboperationen sind keine gemeinsame Transaktion: Nach einem
Abbruch können externe Effekte wiederholt werden. Programme müssen für
solche Fälle geeignet sein; Exactly-once wird nicht zugesichert.

## Lernprogramm

`programs/learn.py` ist ein Beispiel mit reiner Standardbibliothek, das
über `LEARN_BACKEND` zwischen `rspamd` (HTTP-Controller, `Password`-
Header, `/learnspam` und `/learnham`), `spamc` (SpamAssassin) und `log`
umschaltet und die Ergebnisse auf die Exit-Codes abbildet. Ein anderer
Klassifizierer braucht ein anderes Programm im Verzeichnis, sonst nichts.

Der HTTP-Helper folgt keinen Redirects, damit der Token nicht an andere
Ziele weitergegeben wird. 208 und bestätigtes „already learned“ gelten als
Erfolg; Transportfehler, 408, 429 und 5xx werden wiederholt. Der alternative
`spamc`-Pfad deaktiviert Safe-Fallback mit `-x`; er ist im Standardimage
nicht installiert und wurde über seinen Aufruf-/Exitcode-Vertrag geprüft.

Beim Upgrade von 0.10.1 bleiben im Produktionsprofil **spam** und **ham**
die Programmnamen. Diese entsprechen den gespeicherten `Action`-Werten
alter Queue-Einträge und sind auch für alte Worker während des rollenden
Upgrades verständlich. Programmnamen bleiben generell frei wählbar, dürfen
aber bei vorhandenen Aufträgen nicht ohne Übergang entfernt oder
umbenannt werden. Go enthält dazu keine Klassifizierer-Sonderbehandlung.
Viper normalisiert YAML-Schlüssel; `env`-Schlüssel werden deshalb vor der
Prüfung in Großbuchstaben überführt. Ihre Werte bleiben unverändert.

## Benutzer-Skripte

ManageSieve PUTSCRIPT legt das Skript ab. SETMETADATA ordnet es zu; SETACTIVE betrifft weiterhin ausschließlich die Zustellung und weist Skripte mit `require "imapsieve"` ab.

```text
SETMETADATA INBOX ("/shared/imapsieve/script" "inbox-events")
```

Ein vorhandener Mailbox-Eintrag gewinnt gegenüber dem geteilten Server-Eintrag im reservierten Namespace `~server`. Ein leerer oder auf ein fehlendes Skript verweisender Mailbox-Eintrag deaktiviert den Lauf ohne Fallback. Geteilte Server-Metadaten sind für Benutzer schreibgeschützt. Auf freigegebenen Mailboxen benötigt die Skriptzuordnung das Administrationsrecht `a`; das Skript stammt aus dem Speicher des Eigentümers. Seine Aktionen bleiben durch die Rechte des auslösenden Benutzers begrenzt. Cross-Owner-fileinto und ausgehende Nachrichten aus fremden Mailboxen werden abgewiesen.

Beispiel:

```sieve
require ["imapsieve", "environment", "imap4flags"];
if allof (environment :is "imap.cause" "FLAG",
          environment :contains "imap.changedflags" "\\seen") {
    addflag "processed";
}
```

`fileinto` benötigt derzeit eine bereits vorhandene Zielmailbox; `:create` erzeugt im IMAP-Pfad keine Mailbox. `keep` behält das Original; `fileinto` erzeugt eine zusätzliche Nachricht und markiert das Original ohne keep als Deleted. `discard` markiert das Original als Deleted. Es gibt kein automatisches EXPUNGE. Headeränderungen wirken nur auf nachfolgende Kopien/Weiterleitungen. Redirect und mailto-Notify verwenden den konfigurierten SMTP-Relay mit Schleifenschutz; sie setzen eine gültige Eigentümeradresse voraus. Reject, Ereject und Vacation sind im IMAP-Kontext unzulässig; Envelope-Tests erzeugen einen Laufzeitfehler. Acht Aktionen und das konfigurierte Zeitlimit begrenzen einen Lauf. FLAG-Skripte lösen keine weiteren FLAG-Skripte aus; fileinto erzeugt kein COPY-Ereignis.

Skriptfehler lassen die ursprüngliche IMAP-Änderung bestehen. Bei konkurrierenden Flags gewinnt die spätere Clientänderung. Zielkopien werden vor einer möglichen Löschmarkierung des Originals dauerhaft bestätigt. Ein Absturz in diesem Zwischenraum kann eine zusätzliche Kopie hinterlassen; er entfernt keine einzige verbleibende Nachrichtenkopie. Bei mehrdeutigem Speicherfehler werden bestätigte Zielkopien nicht zurückgerollt.

## Dauerhafte Lernwarteschlange

Lernen ist eine Betreiberregel und unabhängig von Benutzerskripten; jede
Regel nennt den `extprograms`-Eintrag, der die Nachricht bekommt. Unterstützt werden COPY/MOVE innerhalb des eigenen Kontos; APPEND/Importe und Cross-Owner-Operationen trainieren nicht. Die erste passende Regel gewinnt; Special-Use-Zuordnungen sind unabhängig von übersetzten Ordnernamen.

Vor dem Ceph-Link wird eine unabhängige Kopie des Inhalts im Datenpool-Namespace `~learning` persistiert. Ein Commitnachweis wird atomar mit dem Zieleintrag geschrieben, auch bei höheren Indexshards. Erst danach kann der Worker senden. EXPUNGE lässt den Payload unberührt. Link und Mailbox-DELETE koordinieren sich über eine Ceph-Lease; vor dem Löschen werden noch benötigte Commitnachweise in die Queue übertragen.

Genau ein Worker hält die clusterweite Lernlease. Fehlversuche werden mit Backoff wiederholt. Die Auswahl priorisiert die ältesten fälligen Einträge. Nach erfolgreicher Antwort wird der Abschlusszustand gespeichert, anschließend werden Receipt und Payload entfernt. Exit 75 des Programms führt zur Wiederholung mit Backoff, jeder andere Fehlschlag beendet den Auftrag endgültig. Ein Absturz zwischen Annahme durch das Programm und lokaler Bestätigung kann einen Wiederholungsaufruf verursachen: der Transport bietet mindestens einmalige Zustellung, keine verteilte Exactly-once-Garantie. Endgültig fehlgeschlagene oder nach 24 Stunden weiterhin unbestätigte Jobs bleiben für die Diagnose erhalten; sie werden nicht still als erfolgreich entfernt.

Nachrichten oberhalb der Größenbegrenzung werden nicht gelernt und als `oversize` gezählt. Ein Fehler beim dauerhaften Vorbereiten des Lernauftrags verhindert den betreffenden Link, damit kein erfolgreich bestätigter Lernvorgang ohne gespeicherten Auftrag entsteht.

Metriken: `imap_imapsieve_events_total{phase=...}` und `imap_imapsieve_learning_total{result=...}`. Keine Nachrichtentexte, Passwörter oder Skriptfehler mit Nachrichtendaten werden geloggt. Dead-Letter-Aufträge benötigen eine bewusste Betreiberentscheidung; es gibt noch keine grafische Queue-Verwaltung.

Der Lernpfad benutzt dieselbe Programmtabelle wie die Sieve-Erweiterungen; im IMAP-Pod läuft nur das konfigurierte Programm, keine Shell.

Referenz: RFC 6785, https://www.rfc-editor.org/rfc/rfc6785.html


## Ressourcengrenzen des Sieve-Interpreters (ab 0.11.1)

Ein regulärer Skriptlauf ist standardmäßig auf fünf Sekunden verstrichene
Zeit begrenzt. `sieve_max_cpu_time` in den Sieve-Einstellungen akzeptiert
positive Go-Zeitintervalle wie `2s`; die Grenze misst verstrichene Zeit und
nicht ausschließlich CPU-Zeit. Ein kürzerer Timeout des Aufrufers gewinnt.
Die bestehende IMAPSieve-Einstellung `timeout_ms` gilt daher auch während
aufwendiger Regex-Vergleiche. Die Ausführung läuft bei Abbruch nicht in einer
abgekoppelten Goroutine weiter.

`foreverypart` darf viermal verschachtelt werden, einschließlich Includes.
Der MIME-Baum ist auf 1000 Teile, Tiefe 20 und 256 KiB Header pro Teil
begrenzt. Bei Überschreitung wird die Skriptausführung abgebrochen. LMTP
verwendet seinen bestehenden Fehlerpfad mit Originalnachricht in INBOX;
IMAPSieve verwirft das fehlgeschlagene Ergebnis und markiert die Nachricht
nicht als gelöscht. Die Nachricht selbst wird nicht gekürzt, um eine
Grenzüberschreitung zu kaschieren.

Details: [Sieve-Korrekturbericht](reports/2026-09-16-sieve-conformance-fixes.md).


Ab 0.11.2 akzeptiert der Rspamd-Helper HTTP 204 und 208 als erfolgreiche
Antworten ohne JSON-Body. Ein leeres HTTP 200 bleibt ein temporärer Fehler.
HTTP 204 wurde bei der produktiven Abnahme tatsächlich beobachtet; die
vorherige Version wiederholte solche bereits erfolgreich beantworteten
Aufträge fälschlich bis zum Fehlerbestand.
