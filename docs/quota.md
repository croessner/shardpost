# Quota: Grenzen, Regeln, Warnungen und Over-Flag

Die Quota eines Kontos besteht aus zwei Zählern (Bytes und Nachrichten) im
Registry-Objekt des Kontos und einem Limit. Das Limit kommt in dieser
Reihenfolge aus `SETQUOTA` bzw. dem Userdb-Attribut
(`server.quota_attribute`, Standard `rnsMSQuota`, Wert mit optionalem
K/M/G/T-Suffix) oder aus `server.quota_bytes`/`server.quota_msgs`
(0 = unbegrenzt). Das Attribut setzt nur das Speicherlimit; das
Nachrichtenlimit folgt weiter `SETQUOTA` bzw. `server.quota_msgs`.

Darüber liegt der Abschnitt `quota`, angelehnt an Dovecots
`quota_grace`, `quota_rule`, `quota_warning` und `quota_over_flag`:

```yaml
quota:
  grace: 10%                 # oder eine Größe wie 10M; leer = 0
  rules:                     # erste passende Regel gewinnt
    - special_use: '\Trash'
      extra_storage: 100M    # oder 10%; extra_messages: 100 oder 10%
    - special_use: '\Junk'
      ignore: true
    - mailbox: 'Archiv/Extern*'
      ignore: true
  warnings:                  # erste passende Warnung gewinnt: höchste Schwelle zuerst
    - name: full
      resource: storage      # oder messages
      percent: 100
      program: quota-warning
    - name: almost-full
      resource: storage
      percent: 90
      program: quota-warning
    - name: below-again
      resource: storage
      percent: 100
      direction: under       # Standard: over
      program: quota-warning
  over_flag:
    enabled: true
    attribute: quotaOverFlag
    over_value: 'TRUE'       # Standard; Vergleich ohne Groß-/Kleinschreibung
    program: quota-over-flag
    recheck_seconds: 300
extprograms:
  bin_dir: /usr/local/lib/shardpost/programs
  programs:
    quota-warning:
      command: quota_warning.py      # mitgeliefert, siehe unten
      kinds: [quota]
      env: {WARN_FROM: postmaster@example.org}
    quota-over-flag:
      command: quota_flag.py         # mitgeliefert, siehe unten
      kinds: [quota]
      timeout_ms: 30000
      env: {FLAG_LDAP_URI: "ldaps://ldap.example.org", ...}
```

`quota.grace`, `quota.over_flag.enabled`, `.attribute`, `.over_value`,
`.program` und `.recheck_seconds` sind auch per Umgebung setzbar
(`QUOTA_GRACE`, `QUOTA_OVER_FLAG_ENABLED`, `QUOTA_OVER_FLAG_ATTRIBUTE`,
`QUOTA_OVER_FLAG_OVER_VALUE`, `QUOTA_OVER_FLAG_PROGRAM`,
`QUOTA_OVER_FLAG_RECHECK_SECONDS`); Regeln und Warnungen nur per YAML.

## Grace

Eine Speicherung, die unter dem Speicherlimit beginnt, darf das Limit um
die Grace überschreiten; ab dem Limit wird abgelehnt. So scheitert nicht
die letzte große Mail knapp vor dem Limit. Standard ist 0 (bisheriges
Verhalten); Dovecot verwendet 10 %.

## Mailbox-Regeln

Eine Regel trifft entweder über `mailbox` (Namensmuster: `*` beliebig
viele Zeichen einschließlich `/`, `?` genau eines; `INBOX` ohne
Groß-/Kleinschreibung, führende und abschließende `/` werden entfernt)
oder über `special_use` (RFC 6154, unabhängig von übersetzten
Ordnernamen).

* `ignore: true` nimmt die Mailbox aus der Zählung: Zustellung, APPEND
  und COPY dorthin reservieren nichts, EXPUNGE und DELETE geben nichts
  frei. MOVE aus einer ignorierten in eine gezählte Mailbox wird wie ein
  COPY geprüft und kann mit `OVERQUOTA` scheitern; MOVE in eine ignorierte
  Mailbox gibt den Platz frei; das gilt auch, wenn der Scrubber einen
  abgebrochenen MOVE vollendet. Ein RENAME, das eine Mailbox in die
  Zählung hinein oder aus ihr heraus bewegt (eine Namensregel kann auch
  eine Special-Use-Regel verdecken), zählt ihre Nachrichten nach, wird
  aber nicht abgelehnt (wie ein gesenktes Limit).
* `extra_storage`/`extra_messages` erhöhen das Limit für Speicherungen in
  diese Mailbox, absolut oder in Prozent des Limits. Typisch ist der
  Papierkorb: Ein volles Konto kann per COPY dorthin noch Platz schaffen.
  MOVE zwischen zwei verschiedenen gezählten Mailboxen ändert die Nutzung
  nicht und wird nie abgelehnt. (MOVE in die gerade selektierte Mailbox
  selbst läuft als COPY mit EXPUNGE und unterliegt daher dem Limit.) Ein
  unbegrenztes Limit bleibt unbegrenzt.

Wer Regeln oder Namensmuster ändert, ändert, was gezählt wird: der
Scrubber (`shardpost scrub`) zählt nach den aktuellen Regeln und gleicht
die Zähler nach zwei übereinstimmenden Läufen an.

## Warnungen

Eine Warnung startet ihr Programm, wenn eine Änderung der Nutzung die
Schwelle (`percent` des Basislimits, ohne Extras) überschreitet:
`over` beim Anstieg von unter auf mindestens die Schwelle, `under` beim
Fall von mindestens auf unter die Schwelle. Pro Änderung läuft nur die
erste passende Warnung in Konfigurationsreihenfolge. Auslöser sind
Zustellung, APPEND, COPY, MOVE (bei geänderter Zählung), EXPUNGE, DELETE
und RENAME. Eine Speicherung meldet ihre Änderung erst, wenn sie gelungen
ist; eine Reservierung, die eine fehlgeschlagene Speicherung zurückgibt,
meldet nichts. Wird dagegen ein bereits gespeicherter Teil einer
Zustellung in mehrere Ordner oder eines MULTIAPPEND zurückgerollt, ist das
ein EXPUNGE und kann eine `under`-Warnung auslösen. Limitänderungen und
die Scrubber-Korrektur lösen nichts aus.

Die Zähler ändern sich per Compare-and-swap auf dem Registry-Objekt; nur
der Pod, dessen Schreibvorgang die Schwelle überschreitet, sieht den
Übergang. Eine Warnung läuft daher clusterweit genau einmal pro Übergang,
ohne Koordination zwischen den Pods.

## Over-Flag

Das Over-Flag ist ein Userdb-Attribut, mit dem ein MTA Mail für volle
Konten früh ablehnen kann. Der Server vergleicht bei jeder Anmeldung
(IMAP, POP3, ManageSieve), bei jeder LMTP-Zustellung vor dem Speichern
(mit den Attributen der Empfängerauflösung) und nach einer gelungenen
Speicherung (eine wegen OVERQUOTA abgelehnte Zustellung ändert den
Zustand nicht) den Attributwert mit dem echten Zustand (Nutzung ≥ Limit
bei Bytes oder Nachrichten). Weichen sie ab, startet das Programm, das das Attribut korrigieren soll
(z. B. per LDAP). Zusätzlich läuft das Programm, wenn eine Änderung der
Nutzung das Limit selbst über- oder unterschreitet, etwa weil ein
IMAP-Client Platz schafft, ohne dass eine Anmeldung oder Zustellung folgt
(dann ohne bisherigen Attributwert). Ein fehlendes Attribut gilt als
„nicht voll“.
Steuerzeichen im Attributwert werden entfernt, er wird auf 256 Bytes
gekürzt. Weil die Userdb (Nauthilus-Cache) den alten Wert noch eine
Weile liefern kann, läuft das Programm für dasselbe Konto und
denselben Zustand höchstens einmal je `recheck_seconds` und Pod; ein
Zustandswechsel läuft sofort.

## Programme

Beide Ereignisse starten Einträge aus `extprograms.programs` mit der Art
`quota` (sudoers-Prinzip wie bei Sieve, siehe
[IMAPSieve und externe Programme](imapsieve.md)): feste Kommandozeile unter
`bin_dir`, keine Shell, private Umgebung, Timeout, keine Eingabe auf
stdin. `kinds: [quota]` verhindert, dass ein Sieve-Skript das Programm
aufruft.

Platzhalter in `args`:

| Platzhalter | Warnung | Over-Flag |
| --- | --- | --- |
| `{account}` | Konto | Konto |
| `{warning}`, `{resource}`, `{percent}`, `{direction}` | Name, `storage`/`messages`, Schwelle, `over`/`under` | – |
| `{state}`, `{flag}` | – | `over`/`under`, bisheriger Attributwert |
| `{storage_usage}`, `{storage_limit}`, `{messages_usage}`, `{messages_limit}` | nach der Änderung | aktueller Stand |

Umgebung: `QUOTA_EVENT` (`warning` oder `over_flag`), `QUOTA_ACCOUNT`,
`QUOTA_STORAGE_USAGE`, `QUOTA_STORAGE_LIMIT`, `QUOTA_MESSAGES_USAGE`,
`QUOTA_MESSAGES_LIMIT`; bei Warnungen zusätzlich `QUOTA_WARNING`,
`QUOTA_RESOURCE`, `QUOTA_PERCENT`, `QUOTA_DIRECTION`; beim Over-Flag
`QUOTA_OVER` (`true`/`false`) und `QUOTA_FLAG`.

Die Programme laufen asynchron; Zustellung und Anmeldung warten nicht.
Höchstens 64 Läufe sind gleichzeitig offen, weitere werden verworfen und
gezählt. Beim Herunterfahren wartet der Server bis zu 10 s auf laufende
Programme. Metriken: `imap_quota_events_total{event,result}` (`dispatched`,
`dropped`) und `imap_extprograms_runs_total{program,result}`.

## Mitgelieferte Programme

Das Image enthält zwei Programme unter
`/usr/local/lib/shardpost/programs` (Einträge mit `kinds: [quota]`, ohne
`args`; sie lesen die `QUOTA_*`-Umgebung). Konfiguriert werden sie über
`env` des `extprograms`-Eintrags, Secrets über Dateipfade.

**`quota_warning.py`** stellt eine Warnmail in das Postfach zu, über die
lokale Zustellung der [Admin-API](admin-api.md) (`shardpostctl deliver`,
standardmäßig ohne Quota-Durchsetzung wie `dovecot-lda … noenforcing`,
mit Sieve). Die Mail trägt `Auto-Submitted: auto-generated`, damit
Vacation und Redirect nicht reagieren.

| Variable | Bedeutung |
| --- | --- |
| `WARN_FROM` | Absender (Pflicht) |
| `WARN_TEMPLATE_OVER`, `WARN_TEMPLATE_UNDER` | Vorlagen; Standard `templates/quota-warning-{over,under}.txt` (DE/EN) |
| `WARN_MSGID_DOMAIN` | Domain der Message-ID (Standard: die von `WARN_FROM`) |
| `WARN_ENFORCE_QUOTA` | `true` stellt mit Durchsetzung zu (Standard `false`) |
| `WARN_SIEVE` | `false` umgeht das Benutzerskript |
| `WARN_SHARDPOSTCTL`, `WARN_ADMIN_SOCKET` | Pfade, falls abweichend |
| `WARN_DEADLINE` | Gesamtzeit in Sekunden (Standard 50, unter `timeout_ms` halten) |

Eine Vorlage besteht aus `Subject: …`, einer Leerzeile und dem Text;
Platzhalter `${account}`, `${percent}`, `${warning}`, `${direction}`,
`${resource}`, `${storage_usage}`, `${storage_limit}`,
`${storage_usage_human}`, `${storage_limit_human}`, `${messages_usage}`,
`${messages_limit}`.

**`quota_flag.py`** setzt das Attribut des Kontos per LDAP auf den
**Zielwert** (kein Umschalten; schon richtig heißt kein Schreibzugriff)
und leert danach den Nauthilus-Cache des Kontos
(`DELETE /api/v1/cache/flush`, Bearer-Token per OIDC
client_credentials, pro Lauf neu geholt). Es nutzt `ldap3` aus dem venv
des Images (`/usr/local/lib/shardpost/venv`, `programs/requirements.txt`
mit festen Hashes) und verweigert unverschlüsseltes LDAP.

| Variable | Bedeutung |
| --- | --- |
| `FLAG_LDAP_URI`, `FLAG_LDAP_BASE`, `FLAG_LDAP_FILTER` | Server, Suchbasis, Filter mit `{account}` (RFC-4515-escaped) |
| `FLAG_LDAP_ATTRIBUTE`, `FLAG_LDAP_VALUE_OVER`, `FLAG_LDAP_VALUE_UNDER` | Standard `rnsMSOverQuota`, `TRUE`, `FALSE` |
| `FLAG_LDAP_CA_FILE`, `FLAG_LDAP_STARTTLS` | Server-CA; StartTLS für `ldap://` |
| `FLAG_LDAP_AUTH` | `external` (SASL EXTERNAL mit `FLAG_LDAP_CERT_FILE`/`FLAG_LDAP_KEY_FILE`) oder `simple` (`FLAG_LDAP_BIND_DN`, `FLAG_LDAP_BIND_PASSWORD_FILE`) |
| `FLAG_FLUSH_URL` | Flush-Endpunkt; leer schaltet den Flush ab |
| `FLAG_OIDC_TOKEN_URL`, `FLAG_OIDC_CLIENT_ID`, `FLAG_OIDC_CLIENT_SECRET_FILE` | client_credentials |
| `FLAG_OIDC_SCOPE`, `FLAG_OIDC_AUTH_METHOD` | Standard `nauthilus:authenticate nauthilus:security` (Basis- und Flush-Scope), `client_secret_basic` |
| `FLAG_HTTP_CONNECT_TO` | `host:port`, mit dem statt des URL-Hosts verbunden wird (wie `curl --connect-to`); TLS prüft weiter den URL-Host. Nötig, weil die öffentliche Kante `/api/v1/cache` nicht routet |
| `FLAG_HTTP_CA_FILE` | CA für HTTPS (Standard: System-Trust) |
| `FLAG_LDAP_TIMEOUT`, `FLAG_HTTP_TIMEOUT` | Sekunden pro Operation (Standard 5) |
| `FLAG_DEADLINE` | Gesamtzeit in Sekunden (Standard 20, mit Abstand unter `timeout_ms` halten) |

Nach Ablauf der Gesamtzeit brechen beide Programme selbst mit 75 und
Statuszeile ab, bevor der Server sie nach `timeout_ms` beendet. Konten mit
Zeichen außerhalb von `A-Z a-z 0-9 @ . _ % + -` lehnen beide mit 1 ab
(wie die Skripte des mailstack). Beide Programme schreiben eine
Statuszeile nach stdout, die der Server mitloggt, und enden mit 0 (erledigt), 75 (vorübergehend, etwa LDAP oder
Nauthilus nicht erreichbar) oder 1 (Konfiguration, kein oder mehrere
Einträge). `make test-programs` prüft beide im Laufzeit-Basisimage,
`make test-programs-ldap` `quota_flag.py` gegen ein Wegwerf-OpenLDAP
(`chrroessner/openldap`, RNS-Schema, SASL EXTERNAL). Dessen
`hack/testdata/quota-ldap/slapd.conf` zeigt die vorgeschlagene
Provider-Regel: `authz-regexp` für die Zertifikatsidentität und eine ACL,
die ihr nur `rnsMSOverQuota` zum Schreiben gibt.
