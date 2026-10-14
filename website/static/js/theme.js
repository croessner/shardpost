// Applies an explicitly chosen color scheme before first paint. "system" stores nothing.
(function () {
  try {
    var t = localStorage.getItem("shardpost-theme");
    if (t === "light" || t === "dark") document.documentElement.setAttribute("data-theme", t);
  } catch (e) {}
})();
