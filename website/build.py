#!/usr/bin/env python3
"""Static site builder for shardpost.de.

Two kinds of pages:

* Hand-written pages in src/pages/. Each starts with a JSON meta block in an
  HTML comment and is wrapped in src/layout.html:

      <!--meta {"title": "...", "description": "...", "path": "/faq/"} -->

* The manual under /docs/, rendered from the repository itself: the Markdown
  documents listed in DOCS and every manual page in docs/man/man1. Nothing in
  the manual is written for the website; change the document in docs/ and
  rebuild. Links between rendered documents stay on the site, every other
  repository link goes to GitHub, and a link to a missing heading fails the
  build.

Needs markdown-it-py (requirements.txt) and mandoc.
"""

import hashlib
import html
import json
import os
import re
import shutil
import subprocess
import sys
from datetime import date
from pathlib import Path, PurePosixPath

from markdown_it import MarkdownIt

ROOT = Path(__file__).resolve().parent
REPO = Path(os.environ.get("SHARDPOST_REPO", ROOT.parent)).resolve()
SRC = ROOT / "src"
STATIC = ROOT / "static"
DIST = ROOT / "dist"

SITE = {
    "base_url": "https://shardpost.de",
    "github": "https://github.com/croessner/shardpost",
    "support_email": "christian@roessner.email",
    "version": "0.52.0",
    "year": str(date.today().year),
}
SITE["stack_diagram"] = (SRC / "partials" / "stack.html").read_text(encoding="utf-8")
BLOB = SITE["github"] + "/blob/main/"
TREE = SITE["github"] + "/tree/main/"

NAV = [
    ("home", "/", "Home"),
    ("how", "/how-it-works/", "How it works"),
    ("docs", "/docs/", "Docs"),
    ("start", "/get-started/", "Get started"),
    ("support", "/support/", "Support"),
]

# The manual: (group, [(slug, source, short label, lede)]). The ledes are the
# descriptions of docs/README.md, so the site says what the index says.
DOCS = [
    ("Start here", [
        ("architecture", "ARCHITECTURE.md", "Architecture",
         "System boundaries, components, deployment, impact of outages."),
        ("protocols", "docs/protocols.md", "Protocols and RFCs",
         "Per-protocol reference: capabilities, commands, methods, limits, deviations, RFC index."),
        ("configuration", "docs/configuration.md", "Configuration",
         "Viper, environment variables, secret files, log levels."),
    ]),
    ("Operations", [
        ("operations", "docs/operations/README.md", "Operations and releases",
         "Dependencies, safe diagnosis, release and rollback, running without Kubernetes."),
        ("storage-guide", "docs/architecture/storage-guide.md", "Storage guide",
         "How ShardPost uses Ceph, built up from the basics: Ceph, the pools, one account's objects, "
         "walkthroughs, programs, operator playbook, monitoring, sizing."),
        ("observability", "docs/observability.md", "Observability",
         "What the metrics mean, sampling, privacy, operational logs."),
        ("admin-api", "docs/admin-api.md", "Admin API and LDA",
         "OpenAPI contract, shardpostctl, roles, local delivery."),
        ("quota", "docs/quota.md", "Quota",
         "Grace, mailbox rules, warning programs, over flag."),
        ("sieve", "docs/imapsieve.md", "Sieve and learning",
         "Script execution, external programs, learning queue."),
        ("bench", "docs/operations/bench.md", "Bench",
         "Bench pools, shardpost bench populate/measure, what an account costs in Ceph."),
    ]),
    ("JMAP", [
        ("jmap", "docs/jmap.md", "JMAP",
         "Account state, session resource, API endpoint, blobs, what is built and what is not."),
        ("jmap-examples", "docs/jmap-examples.md", "JMAP by example",
         "A recorded send and receive through the Director, as JSON."),
    ]),
    ("Internals", [
        ("storage", "docs/architecture/storage.md", "RADOS storage",
         "Object layout, CAS, shards, intents, references, repair."),
        ("runtime", "docs/architecture/runtime.md", "Runtime and UML",
         "Go interfaces, login, delivery, IMAP states."),
        ("decisions", "docs/decisions/architecture.md", "Architecture decisions",
         "Reasons for and consequences of the central design decisions."),
        ("development", "docs/development/README.md", "Developer guide",
         "Code map, build, tests, change paths, vendor fork."),
        ("quality", "docs/development/quality.md", "Quality",
         "Test levels, conformance limits, current and historical evidence."),
        ("diagrams", "docs/diagrams/README.md", "Diagrams",
         "Mermaid sources, SVGs, and reproducible rendering."),
    ]),
    ("Reference", [
        ("man", "docs/man/README.md", "Manual pages",
         "shardpostctl command by command, with examples."),
        ("index", "docs/README.md", "Documentation index",
         "Reading paths, every document, release evidence and history."),
    ]),
]
MAN_DIR = "docs/man/man1"

META_RE = re.compile(r"\A\s*<!--meta\s*(\{.*?\})\s*-->\s*", re.S)
H2_RE = re.compile(r'<h2[^>]*\bid="([^"]+)"[^>]*>(.*?)</h2>', re.S)
VAR_RE = re.compile(r"\{\{\s*([a-z_]+)\s*\}\}")
ATTR_RE = re.compile(r'\b(href|src)="([^"]*)"')
ID_RE = re.compile(r'\bid="([^"]+)"')
TAG_RE = re.compile(r"<[^>]+>")
NOT_PUBLISHED = {"temp"}  # git-ignored scratch space
NOT_IN_IMAGE = {"vendor"}  # in git, but not in the website image's build context
IMAGE_EXT = {".svg", ".png", ".jpg", ".jpeg", ".webp", ".gif"}


def fail(msg):
    sys.exit(f"build.py: {msg}")


def asset_version(*paths):
    digest = hashlib.sha256()
    for path in paths:
        digest.update((STATIC / path).read_bytes())
    return digest.hexdigest()[:10]


def strip_tags(text):
    return html.unescape(TAG_RE.sub("", text)).strip()


def substitute(text, values):
    def repl(match):
        key = match.group(1)
        if key not in values:
            fail(f"unknown placeholder {{{{{key}}}}}")
        return values[key]

    return VAR_RE.sub(repl, text)


def output_path(path):
    if path.endswith(".html") or path.endswith(".json"):
        return DIST / path.lstrip("/")
    return DIST / path.strip("/") / "index.html"


def render_nav(active):
    items = []
    for key, href, label in NAV:
        current = ' aria-current="page"' if key == active else ""
        items.append(f'<a class="nav-link" href="{href}"{current}>{label}</a>')
    return "\n".join(items)


def render_toc(content, label, cls="toc"):
    entries = H2_RE.findall(content)
    if not entries:
        return ""
    links = "\n".join(
        f'<li><a href="#{anchor}">{html.escape(strip_tags(text))}</a></li>' for anchor, text in entries
    )
    return (
        f'<nav class="{cls}" aria-label="{html.escape(label)}">'
        f'<p class="toc-title">{html.escape(label)}</p><ol>{links}</ol></nav>'
    )


# ---------- Markdown ----------

def gh_slug(text):
    """Heading anchor as GitHub computes it, so the links in docs/ keep working."""
    slug = text.strip().lower()
    slug = re.sub(r"[^\w\- ]", "", slug)
    return slug.replace(" ", "-")


def inline_text(token):
    parts = []
    for child in token.children or []:
        if child.type in ("text", "code_inline", "html_inline"):
            parts.append(child.content if child.type != "html_inline" else "")
    return "".join(parts)


class Doc:
    def __init__(self, slug, source, label, lede, group, url):
        self.slug, self.source, self.label, self.lede, self.group, self.url = (
            slug, source, label, lede, group, url)
        self.title = label
        self.body = ""
        self.sections = []  # (anchor, heading, text) for the search index
        self.ids = set()
        self.links = []  # (target url, fragment) of links to other pages of the site


def make_markdown():
    md = MarkdownIt("commonmark", {"html": True, "typographer": False})
    md.enable(["table", "strikethrough"])
    return md


def render_markdown(md, doc):
    text = (REPO / doc.source).read_text(encoding="utf-8")
    tokens = md.parse(text)

    seen, used = {}, set()
    title_range = None
    section = ("", doc.title, [])
    sections = [section]
    for i, tok in enumerate(tokens):
        if tok.type == "heading_open":
            heading = inline_text(tokens[i + 1])
            if tok.tag == "h1" and title_range is None:
                doc.title = heading.strip()
                title_range = (i, i + 3)
                continue
            base_slug = slug = gh_slug(heading)
            while slug in used:
                seen[base_slug] = seen.get(base_slug, 0) + 1
                slug = f"{base_slug}-{seen[base_slug]}"
            used.add(slug)
            tok.attrSet("id", slug)
            if tok.tag in ("h2", "h3"):
                section = (slug, heading.strip(), [])
                sections.append(section)
        elif tok.type == "inline" and (title_range is None or i >= title_range[1]):
            section[2].append(inline_text(tok))
    if title_range:
        del tokens[title_range[0]:title_range[1]]

    doc.sections = [(a, h, " ".join(t)) for a, h, t in sections if t or a]
    doc.body = md.renderer.render(tokens, md.options, {})


# ---------- manual pages ----------

def render_man(doc):
    out = subprocess.run(
        ["mandoc", "-T", "html", "-O", "fragment", str(REPO / doc.source)],
        check=True, capture_output=True, text=True,
    ).stdout
    # The page's own title is the h1 of the layout; man sections become h2.
    out = re.sub(r"<h2 class=\"Ss\"", '<h3 class="Ss"', out).replace("</h2>", "</h3>")
    out = re.sub(r"<h1 class=\"Sh\"", '<h2 class="Sh"', out).replace("</h1>", "</h2>")
    out = re.sub(r'<a class="permalink" href="#[^"]*">(.*?)</a>', r"\1", out, flags=re.S)
    name = re.search(r'id="NAME".*?<p class="Pp">(.*?)</p>', out, re.S)
    if name:
        summary = re.sub(r"\s+", " ", strip_tags(name.group(1)))
        doc.lede = summary.split(" - ", 1)[-1].strip()
        doc.lede = doc.lede[:1].upper() + doc.lede[1:] + "."
    doc.title = f"{doc.label}(1)"
    doc.body = f'<div class="manpage">{out}</div>'
    for anchor, heading in H2_RE.findall(out):
        start = out.find(f'id="{anchor}"')
        nxt = H2_RE.search(out, start + 10)
        chunk = out[start: nxt.start() if nxt else len(out)]
        doc.sections.append((anchor, strip_tags(heading).title(), strip_tags(chunk)))


# ---------- links ----------

def rewrite_links(doc, by_source):
    base = PurePosixPath(doc.source).parent

    def resolve(target):
        path, _, frag = target.partition("#")
        if not path:
            return None, frag
        if path.startswith("/"):
            fail(f"{doc.source}: absolute link {target}; link relative to the document")
        parts = []
        for part in (base / path).parts:
            if part == "..":
                if not parts:
                    fail(f"{doc.source}: link {target} leaves the repository")
                parts.pop()
            elif part != ".":
                parts.append(part)
        return "/".join(parts), frag

    def repl(match):
        attr, target = match.group(1), html.unescape(match.group(2))
        if re.match(r"^[a-z][a-z0-9+.-]*:", target, re.I) or target.startswith("//"):
            return match.group(0)
        if target.startswith("#"):
            doc.links.append((doc.url, target[1:]))
            return match.group(0)
        path, frag = resolve(target)
        suffix = f"#{frag}" if frag else ""
        top = path.split("/", 1)[0]
        if top in NOT_PUBLISHED:
            fail(f"{doc.source}: link {target} points into {top}/, which is not in the public repository")
        if path in by_source:
            dest = by_source[path]
            doc.links.append((dest.url, frag))
            new = dest.url + suffix
        elif PurePosixPath(path).suffix.lower() in IMAGE_EXT and (REPO / path).is_file():
            out = DIST / path
            out.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(REPO / path, out)
            new = "/" + path
        elif top in NOT_IN_IMAGE:
            new = BLOB + path + suffix  # present on GitHub, left out of the image build context
        elif (REPO / path).is_dir():
            new = TREE + path.rstrip("/") + suffix
        elif (REPO / path).exists():
            new = BLOB + path + suffix
        else:
            fail(f"{doc.source}: link to missing file {target}")
        return f'{attr}="{html.escape(new, quote=True)}"'

    doc.body = ATTR_RE.sub(repl, doc.body)
    doc.ids = set(ID_RE.findall(doc.body))


def render_docnav(groups, current):
    out = []
    for group, docs in groups:
        items = []
        for d in docs:
            cur = ' aria-current="page"' if d is current else ""
            items.append(f'<li><a href="{d.url}"{cur}>{html.escape(d.label)}</a></li>')
        out.append(f'<p class="docnav-title">{html.escape(group)}</p><ul>{"".join(items)}</ul>')
    return '<nav class="docnav" aria-label="Manual">' + "".join(out) + "</nav>"


SEARCH_BOX = (
    '<div class="docsearch" role="search">'
    '<label class="visually-hidden" for="doc-search">Search the manual</label>'
    '<input id="doc-search" type="search" placeholder="Search the manual" autocomplete="off" spellcheck="false">'
    '<ol id="doc-results" class="doc-results" hidden></ol>'
    "</div>"
)


def doc_page(doc, nav_groups):
    toc = render_toc(doc.body, "On this page")
    inline_toc = render_toc(doc.body, "On this page", "toc-inline") if toc else ""
    source = (
        f'<p class="doc-source">Rendered from <a href="{BLOB}{doc.source}"><code>{doc.source}</code></a>'
        f' in the ShardPost repository.</p>'
    )
    head = (
        f'<header class="page-head"><p class="eyebrow">{html.escape(doc.group)}</p>'
        f"<h1>{html.escape(doc.title)}</h1>"
        + (f"<p>{html.escape(doc.lede)}</p>" if doc.lede else "")
        + "</header>"
    )
    side = (
        f'<aside class="docs-side"><a class="docs-back" href="/docs/">← All documentation</a>'
        f"{SEARCH_BOX}{render_docnav(nav_groups, doc)}</aside>"
    )
    article = f'<article class="doc prose">{inline_toc}{doc.body}{source}</article>'
    return f'{head}<div class="docs-shell{" has-toc" if toc else ""}">{side}{article}{toc}</div>'


def docs_landing(nav_groups):
    cards = []
    for group, docs in nav_groups:
        if group == "Manual pages":
            continue
        items = "".join(
            f'<a class="doc-card" href="{d.url}"><strong>{html.escape(d.label)}</strong>'
            f"<span>{html.escape(d.lede)}</span></a>"
            for d in docs
        )
        cards.append(f'<h2 id="{gh_slug(group)}">{html.escape(group)}</h2><div class="doc-cards">{items}</div>')
    man = next(docs for group, docs in nav_groups if group == "Manual pages")
    man_items = "".join(
        f'<li><a href="{d.url}"><code>{html.escape(d.label)}</code></a> — {html.escape(d.lede)}</li>' for d in man
    )
    paths = (
        '<div class="paths">'
        '<div class="path-card"><p class="node-kicker">New to the project</p>'
        '<p><a href="/docs/architecture/">Architecture</a> → <a href="/docs/runtime/">Runtime and UML</a>'
        ' → <a href="/docs/development/">Developer guide</a></p></div>'
        '<div class="path-card"><p class="node-kicker">Running it</p>'
        '<p><a href="/docs/configuration/">Configuration</a> → <a href="/docs/operations/">Operations</a>'
        ' → <a href="/docs/observability/">Observability</a> → <a href="/docs/man/">Manual pages</a></p></div>'
        '<div class="path-card"><p class="node-kicker">Storage change</p>'
        '<p><a href="/docs/storage-guide/">Storage guide</a> → <a href="/docs/storage/">RADOS storage</a>'
        ' → <a href="/docs/decisions/">Decisions</a> → <a href="/docs/quality/">Quality</a></p></div>'
        "</div>"
    )
    head = (
        '<header class="page-head"><p class="eyebrow">Documentation</p><h1>The ShardPost manual</h1>'
        f"<p>Architecture, protocols, configuration, operations and every <code>shardpostctl</code> command, "
        f"rendered from the documentation in the repository. It describes ShardPost {SITE['version']}.</p></header>"
    )
    body = (
        f'<div class="docs-shell docs-home"><aside class="docs-side">{SEARCH_BOX}'
        f"{render_docnav(nav_groups, None)}</aside>"
        f'<article class="doc prose"><h2 id="reading-paths">Reading paths</h2>{paths}{"".join(cards)}'
        f'<h2 id="manual-pages">Manual pages</h2><ul class="man-list">{man_items}</ul>'
        f'<p class="doc-source">Release reports, review rounds and the build-out history stay in the '
        f'<a href="{TREE}docs">repository</a>; the <a href="/docs/index/">documentation index</a> links them.</p>'
        "</article></div>"
    )
    return head + body


# ---------- build ----------

def build_docs():
    md = make_markdown()
    groups = []
    by_source = {}
    for group, entries in DOCS:
        docs = []
        for slug, source, label, lede in entries:
            if not (REPO / source).is_file():
                fail(f"missing document {source}")
            doc = Doc(slug, source, label, lede, group, f"/docs/{slug}/")
            docs.append(doc)
            by_source[source] = doc
        groups.append((group, docs))
    man_docs = []
    for page in sorted((REPO / MAN_DIR).glob("*.1")):
        source = f"{MAN_DIR}/{page.name}"
        doc = Doc(page.stem, source, page.stem, "", "Manual pages", f"/docs/man/{page.stem}/")
        man_docs.append(doc)
        by_source[source] = doc
    groups.append(("Manual pages", man_docs))

    for _, docs in groups:
        for doc in docs:
            if doc.source.endswith(".md"):
                render_markdown(md, doc)
            else:
                render_man(doc)
            rewrite_links(doc, by_source)

    known = {d.url: d.ids for _, docs in groups for d in docs}
    known["/docs/"] = None
    broken = [
        (d.source, url, frag)
        for _, docs in groups for d in docs for url, frag in d.links
        if frag and known.get(url) is not None and frag not in known[url]
    ]
    for source, url, frag in broken:
        print(f"build.py: {source}: no heading #{frag} in {url}", file=sys.stderr)
    if broken:
        fail(f"{len(broken)} broken anchor(s)")
    return groups


def search_index(groups):
    entries = []
    for group, docs in groups:
        for doc in docs:
            for anchor, heading, text in doc.sections:
                text = re.sub(r"\s+", " ", text).strip()
                entries.append({
                    "p": doc.title, "g": group, "s": heading if anchor else "",
                    "u": doc.url + (f"#{anchor}" if anchor else ""), "t": text[:4000],
                })
    return entries


def check_site_links():
    """Every same-site link of every page must reach a file, and its anchor an id."""
    pages = {}
    for path in DIST.rglob("*.html"):
        pages[path] = path.read_text(encoding="utf-8")
    ids = {path: set(ID_RE.findall(text)) for path, text in pages.items()}
    broken = []
    for path, text in pages.items():
        found = ID_RE.findall(text)
        for dup in sorted({i for i in found if found.count(i) > 1}):
            broken.append(f"{path.relative_to(DIST)}: id {dup} appears more than once")
    for path, text in pages.items():
        for attr, target in ATTR_RE.findall(text):
            target = html.unescape(target)
            if re.match(r"^[a-z][a-z0-9+.-]*:", target, re.I) or target.startswith("//"):
                continue
            url, _, frag = target.partition("#")
            url = url.split("?", 1)[0]
            if not url:
                dest = path
            else:
                dest = DIST / url.lstrip("/") if url.startswith("/") else path.parent / url
                if url.endswith("/") or dest.is_dir():
                    dest = dest / "index.html"
            dest = dest.resolve()
            if not dest.exists():
                broken.append(f"{path.relative_to(DIST)}: {target} (no such file)")
            elif frag and dest.suffix == ".html" and frag not in ids.get(dest, set()):
                broken.append(f"{path.relative_to(DIST)}: {target} (no such anchor)")
    for line in broken:
        print(f"build.py: {line}", file=sys.stderr)
    if broken:
        fail(f"{len(broken)} broken link(s)")


def build():
    if DIST.exists():
        shutil.rmtree(DIST)
    shutil.copytree(STATIC, DIST)

    layout = (SRC / "layout.html").read_text(encoding="utf-8")
    versions = {
        "css_version": asset_version("css/site.css"),
        "js_version": asset_version("js/site.js", "js/theme.js"),
    }
    sitemap = []

    def write(path, title, description, content, lang="en", layout_kind="doc", nav="", noindex=False):
        values = {
            **SITE,
            **versions,
            "lang": lang,
            "title": html.escape(title),
            "description": html.escape(description),
            "canonical": SITE["base_url"] + path,
            "body_class": f"layout-{layout_kind}",
            "nav": render_nav(nav),
            "robots": "noindex" if noindex else "index,follow",
        }
        # The content goes in after the substitution, so a "{{...}}" in a rendered
        # document stays text instead of being taken for a placeholder.
        page_html = substitute(layout.replace("{{content}}", "\0"), values).replace("\0", content)
        out = output_path(path)
        out.parent.mkdir(parents=True, exist_ok=True)
        out.write_text(page_html, encoding="utf-8")
        if not noindex:
            sitemap.append(SITE["base_url"] + path)

    for page in sorted((SRC / "pages").glob("*.html")):
        raw = page.read_text(encoding="utf-8")
        match = META_RE.match(raw)
        if not match:
            fail(f"{page.name} has no meta block")
        meta = json.loads(match.group(1))
        content = substitute(raw[match.end():], SITE)
        lang = meta.get("lang", "en")
        layout_kind = meta.get("layout", "doc")
        toc = render_toc(content, "Inhalt" if lang == "de" else "On this page") if meta.get("toc") else ""
        if layout_kind == "doc":
            content = (
                f'<div class="doc-shell{" has-toc" if toc else ""}">{toc}'
                f'<article class="doc prose">{content}</article></div>'
            )
            if meta.get("heading"):
                eyebrow = meta.get("eyebrow", "")
                head = f'<p class="eyebrow">{html.escape(eyebrow)}</p>' if eyebrow else ""
                head += f'<h1>{html.escape(meta["heading"])}</h1>'
                if meta.get("lede"):
                    head += f'<p>{html.escape(meta["lede"])}</p>'
                content = f'<header class="page-head">{head}</header>{content}'
        write(meta["path"], meta["title"], meta["description"], content, lang, layout_kind,
              meta.get("nav", ""), meta.get("noindex", False))

    groups = build_docs()
    write("/docs/", "Documentation – ShardPost",
          "The ShardPost manual: architecture, protocols, configuration, operations, storage on Ceph, "
          "JMAP and every shardpostctl command.",
          docs_landing(groups), layout_kind="docs", nav="docs")
    for _, docs in groups:
        for doc in docs:
            description = doc.lede or doc.title
            write(doc.url, f"{doc.title} – ShardPost docs", description, doc_page(doc, groups),
                  layout_kind="docs", nav="docs")
    (DIST / "search-index.json").write_text(
        json.dumps(search_index(groups), ensure_ascii=False, separators=(",", ":")), encoding="utf-8")

    check_site_links()

    urls = "\n".join(f"  <url><loc>{u}</loc></url>" for u in sitemap)
    (DIST / "sitemap.xml").write_text(
        '<?xml version="1.0" encoding="UTF-8"?>\n'
        '<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">\n'
        f"{urls}\n</urlset>\n",
        encoding="utf-8",
    )
    (DIST / "robots.txt").write_text(
        f"User-agent: *\nAllow: /\nSitemap: {SITE['base_url']}/sitemap.xml\n", encoding="utf-8")
    print(f"built {len(sitemap)} pages into {DIST}")


if __name__ == "__main__":
    build()
