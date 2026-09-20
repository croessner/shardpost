# Architecture diagrams

The `.mmd` files are the editable Mermaid sources. Alongside them are
SVGs for Markdown viewers without Mermaid support. Labels and
captions point out where a diagram abbreviates details.

| Source | View | Purpose |
|---|---|---|
| [context.mmd](context.mmd) | [SVG](context.svg) | System boundaries |
| [components.mmd](components.mmd) | [SVG](components.svg) | Components inside the process |
| [deployment.mmd](deployment.mmd) | [SVG](deployment.svg) | Kubernetes/RADOS |
| [interfaces.mmd](interfaces.mmd) | [SVG](interfaces.svg) | UML interfaces and implementations |
| [login.mmd](login.mmd) | [SVG](login.svg) | Login sequence |
| [delivery.mmd](delivery.mmd) | [SVG](delivery.svg) | Local LMTP delivery |
| [imap-states.mmd](imap-states.mmd) | [SVG](imap-states.svg) | IMAP state machine |
| [storage.mmd](storage.mmd) | [SVG](storage.svg) | Physical data model |
| [shard-commit.mmd](shard-commit.mmd) | [SVG](shard-commit.svg) | Pending/commit sequence |
| [learning.mmd](learning.mmd) | [SVG](learning.svg) | Durable learning job |

## Rendering

Verified renderer: `@mermaid-js/mermaid-cli` 11.17.0 with Chrome.
Install the renderer outside the Go project or use an existing
`mmdc`. Then, from the project directory:

```bash
MMDC=/path/to/mmdc hack/render-docs-diagrams.sh
```

When using an existing Chrome, point at the executable via `executablePath`
in a Puppeteer JSON configuration and set that file's path
as `PUPPETEER_CONFIG`. The rendering helper does not upload any project data
to a diagram service. After a change, update the source and the SVG together
and check for readable labels and truncated content.
