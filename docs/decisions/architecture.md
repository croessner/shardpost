# Architekturentscheidungen

Status: Beschreibung vorhandener Entscheidungen am 16.09.2026. Diese Datei
ist kein Beschluss, zusätzliche Produktfunktionen umzusetzen.

| Entscheidung | Grund | Konsequenz und Grenze |
|---|---|---|
| Direktes RADOS statt Dateisystem | Nachrichten und geordnete Mailboxmetadaten passen auf Objekt-/omap-Operationen | Eigenverantwortung für Indizes, Konsistenz, Reparatur und Migration |
| Getrennte Index-/Datenpools | omap-Metadaten und große Payloads haben unterschiedliche Anforderungen | Sicherung, Restore und Kapazitätsplanung müssen beide Pools umfassen |
| Gemeinsame Pools, Namespace je Konto | Vermeidet einen Pool pro Benutzer | Kanonisierung und Autorisierung müssen zuverlässig sein; Namespace ist keine eigene Credential-Grenze |
| Interfaces zwischen Protokoll und Store | Protokolltests ohne RADOS; definierte Datenverträge | Der große User-Vertrag muss in beiden Backends konsistent bleiben; LMTP ist enger an Ceph gekoppelt |
| Objekt-CAS und Journale | Mehrere Writer ohne zentralen Datenbankdienst | Keine globale Transaktion; Konflikte und Pending-Zustände gehören zum Normalmodell |
| Intents für mehrteilige Änderungen | Abbrüche müssen erkennbar und reparierbar bleiben | Scrub und Altersgrenzen sind Teil des Designs, nicht optionale Kosmetik |
| Watch/Notify plus persistente Änderungserkennung | Schnelle Updates ohne zusätzlichen Message-Bus | Benachrichtigungen sind unzuverlässige Hinweise, keine alleinige Wahrheitsquelle |
| Kanonisches Konto aus Auth-Result | Aliase und Loginformen sollen auf denselben Mailbestand zeigen | Änderung der Kanonisierung ist potenziell eine Datenmigration |
| Sieve-Fork im Projekt | Benötigte Interpreterfunktionen und konkrete Konformitätskorrekturen | Fork, Vendor-Kopie, Fixturebestand und Tests müssen gemeinsam gepflegt werden |
| Allowlist für externe Programme | Austauschbare Verarbeitung mit begrenzter Ausführung | Keine gemeinsame Transaktion mit Ceph; Seiteneffekte können wiederholt werden |
| Persistente Lernkopie und Commit-Belege | Lernen soll EXPUNGE und Prozessabbrüche überstehen | Separate Queue-Lebenszyklen, Retries, Dead-Jobs und Speicherbedarf |
| Strukturierte slog-Logs | Sitzungen und Fehler im Cluster nachvollziehen | Betriebsdaten enthalten Client-IP/Konto; Level und Aufbewahrung bewusst betreiben |

## Regeln für künftige Entscheidungen

Eine neue Entscheidung beschreibt konkreten Anlass, vorhandenen Vertrag,
Optionen, gewählte Lösung, Folgen, Migration und Überprüfung. Bei RADOS-
Änderungen gehört die Absturzmatrix dazu; bei Protokolländerungen Zustand,
Rechte, Antwortsemantik und Capability. Bei Infrastrukturänderungen sind
andere Komponenten und deren Verantwortliche ausdrücklich zu benennen.

Die frühere [Architekturbeschreibung](../history/architecture-before-2026-09-16.md)
bleibt als historische Entstehungsgeschichte erhalten. Sie enthält teilweise
stärkere Aussagen und frühere Ausbauziele, die keine aktuellen Garantien sind.
