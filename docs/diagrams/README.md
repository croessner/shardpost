# Architekturdiagramme

Die `.mmd`-Dateien sind die editierbaren Mermaid-Quellen. Daneben liegen
SVGs für Markdown-Viewer ohne Mermaid-Unterstützung. Beschriftungen und
Bildunterschriften erläutern, wo ein Diagramm Details verkürzt.

| Quelle | Ansicht | Verwendung |
|---|---|---|
| [context.mmd](context.mmd) | [SVG](context.svg) | Systemgrenzen |
| [components.mmd](components.mmd) | [SVG](components.svg) | Komponenten im Prozess |
| [deployment.mmd](deployment.mmd) | [SVG](deployment.svg) | Kubernetes/RADOS |
| [interfaces.mmd](interfaces.mmd) | [SVG](interfaces.svg) | UML-Interfaces und Implementierungen |
| [login.mmd](login.mmd) | [SVG](login.svg) | Login-Sequenz |
| [delivery.mmd](delivery.mmd) | [SVG](delivery.svg) | Lokale LMTP-Zustellung |
| [imap-states.mmd](imap-states.mmd) | [SVG](imap-states.svg) | IMAP-Zustandsautomat |
| [storage.mmd](storage.mmd) | [SVG](storage.svg) | Physisches Datenmodell |
| [shard-commit.mmd](shard-commit.mmd) | [SVG](shard-commit.svg) | Pending-/Commit-Sequenz |
| [learning.mmd](learning.mmd) | [SVG](learning.svg) | Persistenter Lernjob |

## Rendern

Geprüfter Renderer: `@mermaid-js/mermaid-cli` 11.17.0 mit Chrome.
Den Renderer außerhalb des Go-Projekts installieren oder einen vorhandenen
`mmdc` verwenden. Danach vom Projektverzeichnis:

```bash
MMDC=/pfad/zum/mmdc hack/render-docs-diagrams.sh
```

Bei Verwendung eines vorhandenen Chrome die ausführbare Datei in einer
Puppeteer-JSON-Konfiguration über `executablePath` angeben und deren Pfad
als `PUPPETEER_CONFIG` setzen. Die Rendering-Hilfe lädt keine Projektdaten
zu einem Diagrammdienst hoch. Nach Änderungen Quelle und SVG gemeinsam
aktualisieren und auf lesbare Beschriftung sowie abgeschnittene Inhalte prüfen.
