# shardpost.de

Static website for ShardPost: product pages, the manual, support,
Impressum and Datenschutzerklärung. Styling follows nauthilus.org and
jmapper.de (navy, slate-blue bars, lime accent). The site is English; the
Impressum and the Datenschutzerklärung are German, as German law requires.

## Layout

- `src/layout.html` – page shell (navbar, footer). Placeholders: `{{name}}`.
- `src/pages/*.html` – the hand-written pages, each starting with a JSON
  meta comment (`title`, `description`, `path`, optional `lang`, `toc`,
  `heading`, `eyebrow`, `lede`, `layout`, `nav`, `noindex`).
- `src/partials/stack.html` – the interactive stack diagram, inserted with
  `{{stack_diagram}}` on the start page and on *How it works*.
- `static/` – CSS, JS, images, copied as they are. `img/shardpost-mark.svg`
  is the ShardPost logo; the favicons and `img/og-card.png` are rendered
  from it with `rsvg-convert`.
- `build.py` – renders everything into `dist/`. Site-wide values such as
  the version shown on the pages live in `SITE` at the top.

## The manual is rendered, not written here

Everything under `/docs/` comes from the repository: the Markdown documents
listed in `DOCS` in `build.py` (rendered with markdown-it-py, headings get
GitHub's anchors) and every page in `docs/man/man1` (rendered with mandoc).
To change the manual, change the document in `docs/` and rebuild. A link
between rendered documents stays on the site; a link to any other file of
the repository points at GitHub. The build fails when a link points at a
file that does not exist, or when any link of any page points at a
missing page or heading.

A new document appears on the site only when it is added to `DOCS`. The
release reports, review rounds and history stay in the repository.

## Privacy

No external resources, no cookies. The only browser storage is the
optional color-scheme choice (`shardpost-theme`); the manual search fetches
`/search-index.json` from the site and runs in the browser. The
Datenschutzerklärung describes exactly this. Keep it that way, or update
the policy in the same change.

## Commands

```bash
make build    # venv with requirements.txt, then dist/ (needs mandoc)
make serve    # preview dist/ on http://127.0.0.1:8000
make run      # production image with its nginx config on http://127.0.0.1:8088
make deploy   # build, push to docker.roessner-net.de and restart on node1
```

The image is built with the repository root as context
(`docker build -f website/Dockerfile ..`); `Dockerfile.dockerignore`
replaces the root `.dockerignore` for it.

`make deploy` copies `deploy/docker-compose.yml` to
`node1.roessner-net.de:/srv/docker/shardpost-website` via the management
host's Ansible and pins the image tag in `.env` there. Rollback: set
`SHARDPOST_WEBSITE_TAG` in that `.env` to the previous tag and run
`docker compose up -d`.

When a release changes the version, update `SITE["version"]` in `build.py`.
