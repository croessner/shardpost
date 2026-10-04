# Architecture diagrams

The `.mmd` files are the editable Mermaid sources. Alongside them are
SVGs for Markdown viewers without Mermaid support. Labels and
captions point out where a diagram abbreviates details. The `storage-guide-*`
diagrams belong to [Storage guide](../architecture/storage-guide.md).

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
| [storage-guide-ceph-basics.mmd](storage-guide-ceph-basics.mmd) | [SVG](storage-guide-ceph-basics.svg) | Storage guide: how a client reaches an OSD |
| [storage-guide-ceph-objects.mmd](storage-guide-ceph-objects.mmd) | [SVG](storage-guide-ceph-objects.svg) | Storage guide: object parts, replication and erasure coding |
| [storage-guide-pools.mmd](storage-guide-pools.mmd) | [SVG](storage-guide-pools.svg) | Storage guide: pools and namespaces |
| [storage-guide-account-objects.mmd](storage-guide-account-objects.mmd) | [SVG](storage-guide-account-objects.svg) | Storage guide: the objects of one account |
| [storage-guide-delivery.mmd](storage-guide-delivery.mmd) | [SVG](storage-guide-delivery.svg) | Storage guide: delivery of one message |
| [storage-guide-read-search.mmd](storage-guide-read-search.mmd) | [SVG](storage-guide-read-search.svg) | Storage guide: reading and searching |
| [storage-guide-expunge.mmd](storage-guide-expunge.mmd) | [SVG](storage-guide-expunge.svg) | Storage guide: expunge and the reference count |
| [storage-guide-coordination.mmd](storage-guide-coordination.mmd) | [SVG](storage-guide-coordination.svg) | Storage guide: CAS, watch/notify, locks, intents |
| [storage-guide-store-wide.mmd](storage-guide-store-wide.mmd) | [SVG](storage-guide-store-wide.svg) | Storage guide: store-wide namespaces and their users |
| [storage-guide-programs.mmd](storage-guide-programs.mmd) | [SVG](storage-guide-programs.svg) | Storage guide: programs and pools |
| [storage-guide-operator-flow.mmd](storage-guide-operator-flow.mmd) | [SVG](storage-guide-operator-flow.svg) | Storage guide: where an operator looks first |
| [storage-guide-monitoring.mmd](storage-guide-monitoring.mmd) | [SVG](storage-guide-monitoring.svg) | Storage guide: monitoring layers |

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

The `storage-guide-*` SVGs (2026-10-04) were rendered with Mermaid 11.6.0 in a
headless Chrome, theme `neutral`, white background, not with the
verified `mmdc` above; rendering them again with `mmdc` replaces them
with equivalent output.
