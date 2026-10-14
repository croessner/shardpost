(function () {
  var root = document.documentElement;
  var KEY = "shardpost-theme";
  var ORDER = ["system", "light", "dark"];

  function current() {
    var t = root.getAttribute("data-theme");
    return t === "light" || t === "dark" ? t : "system";
  }

  function apply(choice) {
    try {
      if (choice === "system") {
        root.removeAttribute("data-theme");
        localStorage.removeItem(KEY);
      } else {
        root.setAttribute("data-theme", choice);
        localStorage.setItem(KEY, choice);
      }
    } catch (e) {
      if (choice !== "system") root.setAttribute("data-theme", choice);
    }
    label(choice);
  }

  function label(choice) {
    var button = document.querySelector(".theme-toggle");
    if (!button) return;
    button.dataset.choice = choice;
    var text = "Color scheme: " + choice;
    button.setAttribute("aria-label", text);
    button.setAttribute("title", text);
  }

  var toggle = document.querySelector(".theme-toggle");
  if (toggle) {
    label(current());
    toggle.addEventListener("click", function () {
      apply(ORDER[(ORDER.indexOf(current()) + 1) % ORDER.length]);
    });
  }

  var navToggle = document.querySelector(".nav-toggle");
  var nav = document.getElementById("site-nav");
  if (navToggle && nav) {
    navToggle.addEventListener("click", function () {
      var open = navToggle.getAttribute("aria-expanded") === "true";
      navToggle.setAttribute("aria-expanded", String(!open));
      nav.classList.toggle("open", !open);
    });
  }

  // Highlight the current section in the table of contents
  var tocLinks = document.querySelectorAll("nav.toc a");
  if (tocLinks.length && "IntersectionObserver" in window) {
    var byId = {};
    tocLinks.forEach(function (a) { byId[a.getAttribute("href").slice(1)] = a; });
    var observer = new IntersectionObserver(function (entries) {
      entries.forEach(function (entry) {
        if (!entry.isIntersecting) return;
        tocLinks.forEach(function (a) { a.removeAttribute("aria-current"); });
        var link = byId[entry.target.id];
        if (link) link.setAttribute("aria-current", "true");
      });
    }, { rootMargin: "-80px 0px -70% 0px" });
    Object.keys(byId).forEach(function (id) {
      var el = document.getElementById(id);
      if (el) observer.observe(el);
    });
  }
  // The stack diagram: a node explains itself when it is clicked or focused with Enter.
  var stack = document.querySelector(".stack-svg");
  var detail = document.querySelector(".stack-detail");
  if (stack && detail) {
    var nodes = stack.querySelectorAll(".node");
    var select = function (node) {
      var src = document.querySelector('.stack-details-src [data-node="' + node.getAttribute("data-node") + '"]');
      if (!src) return;
      nodes.forEach(function (n) { n.setAttribute("aria-pressed", String(n === node)); });
      detail.innerHTML = src.innerHTML;
    };
    nodes.forEach(function (node) {
      node.addEventListener("click", function () { select(node); });
      node.addEventListener("keydown", function (e) {
        if (e.key === "Enter" || e.key === " ") { e.preventDefault(); select(node); }
      });
    });
    var first = stack.querySelector('.node[data-node="shardpost"]');
    if (first) select(first);
  }

  // Manual search: the index is fetched from this site once, the search runs in the browser.
  var input = document.getElementById("doc-search");
  var results = document.getElementById("doc-results");
  if (input && results) {
    var index = null, loading = null, active = -1;
    var load = function () {
      if (!loading) {
        loading = fetch("/search-index.json").then(function (r) {
          if (!r.ok) throw new Error("search index: HTTP " + r.status);
          return r.json();
        }).then(function (data) {
          index = data.map(function (e) {
            e.lt = e.t.toLowerCase(); e.lh = (e.p + " " + e.s).toLowerCase(); return e;
          });
        }).catch(function () {
          loading = null;
          results.innerHTML = '<li class="r-empty">The search is not available right now.</li>';
          results.hidden = false;
        });
      }
      return loading;
    };
    var esc = function (s) {
      return s.replace(/[&<>"]/g, function (c) { return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]; });
    };
    // Finds the match ranges on the raw text first, then escapes and wraps in one pass,
    // so a later term never matches inside the markup of an earlier one.
    var mark = function (text, terms) {
      var lower = text.toLowerCase(), ranges = [];
      terms.forEach(function (t) {
        if (t.length < 2) return;
        for (var i = lower.indexOf(t); i !== -1; i = lower.indexOf(t, i + t.length)) ranges.push([i, i + t.length]);
      });
      ranges.sort(function (a, b) { return a[0] - b[0]; });
      var out = "", pos = 0;
      ranges.forEach(function (r) {
        if (r[1] <= pos) return;
        var from = Math.max(r[0], pos);
        out += esc(text.slice(pos, from)) + "<mark>" + esc(text.slice(from, r[1])) + "</mark>";
        pos = r[1];
      });
      return out + esc(text.slice(pos));
    };
    var snippet = function (e, terms) {
      var i = -1;
      for (var k = 0; k < terms.length && i < 0; k++) i = e.lt.indexOf(terms[k]);
      if (i < 0) return e.t.slice(0, 160);
      var from = Math.max(0, i - 60);
      return (from ? "… " : "") + e.t.slice(from, from + 180) + "…";
    };
    var render = function () {
      var q = input.value.trim().toLowerCase();
      active = -1;
      if (q.length < 2 || !index) { results.hidden = true; results.innerHTML = ""; return; }
      var terms = q.split(/\s+/).filter(Boolean);
      var hits = [];
      index.forEach(function (e) {
        var score = 0;
        for (var k = 0; k < terms.length; k++) {
          var t = terms[k], inHead = e.lh.indexOf(t) !== -1, inText = e.lt.indexOf(t) !== -1;
          if (!inHead && !inText) return;
          score += (inHead ? 10 : 0) + (inText ? 1 : 0);
          if (e.s.toLowerCase() === q || e.p.toLowerCase() === q) score += 20;
        }
        hits.push([score, e]);
      });
      hits.sort(function (a, b) { return b[0] - a[0]; });
      if (!hits.length) {
        results.innerHTML = '<li class="r-empty">Nothing found. Try another word.</li>';
      } else {
        results.innerHTML = hits.slice(0, 20).map(function (h) {
          var e = h[1];
          return '<li><a href="' + esc(e.u) + '"><span class="r-where">' + esc(e.g) + " · " + esc(e.p) + "</span>" +
            '<span class="r-title">' + mark(e.s || e.p, terms) + "</span>" +
            '<span class="r-snip">' + mark(snippet(e, terms), terms) + "</span></a></li>";
        }).join("");
      }
      results.hidden = false;
    };
    input.addEventListener("focus", load);
    input.addEventListener("input", function () { load().then(render); });
    input.addEventListener("keydown", function (e) {
      var links = results.querySelectorAll("a");
      if (e.key === "Escape") { results.hidden = true; return; }
      if (!links.length || (e.key !== "ArrowDown" && e.key !== "ArrowUp" && e.key !== "Enter")) return;
      if (e.key === "Enter") {
        e.preventDefault();
        location.href = links[Math.max(active, 0)].getAttribute("href");
        return;
      }
      e.preventDefault();
      active = (active + (e.key === "ArrowDown" ? 1 : -1) + links.length) % links.length;
      links.forEach(function (a, i) { a.classList.toggle("active", i === active); });
      links[active].scrollIntoView({ block: "nearest" });
    });
    document.addEventListener("click", function (e) {
      if (!e.target.closest(".docsearch")) results.hidden = true;
    });
  }
})();
