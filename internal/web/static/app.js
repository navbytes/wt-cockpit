// app.js — progressive enhancement only. Boundary rules (P4-design.md §1.2):
// this file never builds markup from response *data*. innerHTML is only ever
// replaced by a whole fragment fetched same-origin from wtd's /fragment/*
// endpoints (server-escaped by construction, per the two-producer
// template.HTML rule in internal/web).
(function () {
  "use strict";

  function swapWorktrees() {
    fetch("/fragment/worktrees", { credentials: "same-origin" })
      .then(function (r) { return r.ok ? r.text() : null; })
      .then(function (html) {
        if (html === null) return;
        var el = document.getElementById("worktrees");
        if (el) el.innerHTML = html;
      });
  }

  var swapTimer = null;
  function scheduleSwap() {
    // ponytail: debounce only, no per-event-type filter — a burst of
    // worktree/diff/comment events during a busy refresh collapses into one
    // fetch either way, and refreshing on an event the index doesn't
    // otherwise care about is harmless. Filter by e.data's "type" later if a
    // real cost ever shows up.
    clearTimeout(swapTimer);
    swapTimer = setTimeout(swapWorktrees, 500);
  }

  if (document.getElementById("worktrees") && window.EventSource) {
    var events = new EventSource("/api/events");
    events.onmessage = scheduleSwap;
  }
})();
