# Architecture diagrams

The `.mmd` files are the editable Mermaid sources. Alongside them are
SVGs for Markdown viewers without Mermaid support. Labels and
captions point out where a diagram abbreviates details. The `course-*`
diagrams belong to [Storage explained](../architecture/storage-explained.md).

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
| [course-ceph-basics.mmd](course-ceph-basics.mmd) | [SVG](course-ceph-basics.svg) | Course: how a client reaches an OSD |
| [course-ceph-objects.mmd](course-ceph-objects.mmd) | [SVG](course-ceph-objects.svg) | Course: object parts, replication and erasure coding |
| [course-pools.mmd](course-pools.mmd) | [SVG](course-pools.svg) | Course: pools and namespaces |
| [course-account-objects.mmd](course-account-objects.mmd) | [SVG](course-account-objects.svg) | Course: the objects of one account |
| [course-delivery.mmd](course-delivery.mmd) | [SVG](course-delivery.svg) | Course: delivery of one message |
| [course-read-search.mmd](course-read-search.mmd) | [SVG](course-read-search.svg) | Course: reading and searching |
| [course-expunge.mmd](course-expunge.mmd) | [SVG](course-expunge.svg) | Course: expunge and the reference count |
| [course-coordination.mmd](course-coordination.mmd) | [SVG](course-coordination.svg) | Course: CAS, watch/notify, locks, intents |
| [course-store-wide.mmd](course-store-wide.mmd) | [SVG](course-store-wide.svg) | Course: store-wide namespaces and their users |
| [course-programs.mmd](course-programs.mmd) | [SVG](course-programs.svg) | Course: programs and pools |
| [course-operator-flow.mmd](course-operator-flow.mmd) | [SVG](course-operator-flow.svg) | Course: where an operator looks first |
| [course-monitoring.mmd](course-monitoring.mmd) | [SVG](course-monitoring.svg) | Course: monitoring layers |

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

The `course-*` SVGs (2026-10-04) were rendered with Mermaid 11.6.0 in a
headless Chrome, theme `neutral`, white background, not with the
verified `mmdc` above; rendering them again with `mmdc` replaces them
with equivalent output.
