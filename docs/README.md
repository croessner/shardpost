# Dokumentation

Diese Dokumentation beschreibt den Code-Stand ShardPost 0.13.0 vom
18.09.2026. Architektur beschreibt vorhandene Mechanismen; konkrete
Betriebsergebnisse tragen ein Datum und einen Nachweis. Ziele sind als Ziele
gekennzeichnet. Der Produktname ist ShardPost, seit 0.13.0 auch in allen Betriebsbezeichnern.

## Lesepfade

**Neu im Projekt:** [Überblick](../README.md) → [Architektur](../ARCHITECTURE.md)
→ [Laufzeit und UML](architecture/runtime.md) → [Entwicklerhandbuch](development/README.md).

**Speicheränderung:** [Konsistenz und Datenmodell](architecture/storage.md)
→ [Architekturentscheidungen](decisions/architecture.md) →
[Tests und Grenzen](development/quality.md).

**Betrieb:** [Konfiguration](configuration.md) → [Betrieb und Releases](operations/README.md)
→ [Logs, Metriken und Traces](observability.md).

| Dokument | Inhalt |
|---|---|
| [Architektur](../ARCHITECTURE.md) | Systemgrenzen, Komponenten, Deployment, Auswirkungen von Ausfällen |
| [Laufzeit und UML](architecture/runtime.md) | Go-Interfaces, Anmeldung, Zustellung, IMAP-Zustände |
| [RADOS-Speicher](architecture/storage.md) | Objektlayout, CAS, Shards, Intents, Referenzen, Reparatur |
| [Entwicklerhandbuch](development/README.md) | Codekarte, Build, Tests, Änderungspfade, Vendor-Fork |
| [Qualität](development/quality.md) | Testebenen, Konformitätsgrenzen, aktuelle und historische Evidenz |
| [Stufe 8: Betriebsverwaltung](development/admin-stage.md) | Geplant: shardpostctl, OpenAPI, Mailboxen, ACL, Quota, FTS, Reparaturen und Migration |
| [Betrieb](operations/README.md) | Abhängigkeiten, sichere Diagnose, Release und Rollback |
| [Konfiguration](configuration.md) | Viper, Umgebungsvariablen, Secret-Dateien, Log-Level |
| [Sieve und Lernen](imapsieve.md) | Skriptausführung, externe Programme, Lernqueue |
| [Quota](quota.md) | Grace, Mailbox-Regeln, Warnprogramme, Over-Flag |
| [Admin-API und LDA](admin-api.md) | OpenAPI-Vertrag, shardpostctl, Rollen, lokale Zustellung |
| [Observability](observability.md) | Metrikbedeutung, Sampling, Datenschutz, Betriebslogs |
| [Architekturentscheidungen](decisions/architecture.md) | Gründe und Folgen der zentralen Entwurfsentscheidungen |
| [Projektname](decisions/project-name.md) | Kandidaten und kompatible Umbenennung |
| [Diagramme](diagrams/README.md) | Mermaid-Quellen, SVGs und reproduzierbares Rendern |

## Nachweise und Historie

- [ShardPost 0.13.0](reports/2026-09-18-shardpost-0.13.0.md): Lint-Bereinigung, Namespace-/Pool-/Identitätsumzug, aktueller Rollout.
- [ShardPost 0.12.0](reports/2026-09-16-shardpost.md): Umbenennung und Kompatibilität.
- [Logging 0.11.3](reports/2026-09-16-logging.md): Logging-Erweiterung und damaliger Protokollnachweis.
- [Sieve-Korrekturen bis 0.11.2](reports/2026-09-16-sieve-conformance-fixes.md).
- [Externe Programme](reports/2026-09-16-extprograms.md).
- [IMAPSieve](reports/2026-09-15-imapsieve.md) und [PROXY-Protokoll](reports/2026-09-15-proxy-protocol.md).
- [Konformitätsergebnisse](../hack/conformance.md): versionierte Resultate, kein pauschales Grün.
- [Review-Runde 1](review-round1.md), [2](review-round2.md), [3](review-round3.md),
  [4](review-round4.md), [5](review-round5.md), [6](review-round6.md): historische Befunde und Änderungen.
- [Frühere Architektur](history/architecture-before-2026-09-16.md),
  [frühere README](history/readme-before-2026-09-16.md), [Ausbauhistorie](../ROADMAP.md).

Historische Dokumente bleiben als Begründung und Nachweis erhalten. Für neue
Implementierungen gelten die aktuelle Codekarte und die tatsächlich
implementierten Interfaces. Ändert sich ein Ablauf, werden Beschreibung,
Mermaid-Quelle und gerendertes SVG gemeinsam aktualisiert.
