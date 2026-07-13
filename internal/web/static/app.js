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

// ---- reading room: review toggle + approve (P4-design.md WP3) ----
// Same boundary rules as swapWorktrees above: no markup is ever built from
// response data here, only checked/disabled/textContent/class changes plus
// values read out of the already-escaped, already-rendered page (dataset
// attributes) or set via textContent from a decoded JSON field.
(function () {
  "use strict";

  var diffEl = document.getElementById("diff");
  if (!diffEl) return; // not the room page

  var wrap = document.querySelector(".review-wrap");
  var wtID = wrap ? wrap.dataset.id : "";
  var banner = document.getElementById("banner");

  function csrfToken() {
    var m = document.querySelector('meta[name="csrf-token"]');
    return m ? m.content : "";
  }

  function postJSON(url, body) {
    return fetch(url, {
      method: "POST",
      credentials: "same-origin",
      headers: { "Content-Type": "application/json", "X-Csrf-Token": csrfToken() },
      body: JSON.stringify(body)
    });
  }

  function showBanner(el, text) {
    if (!el) return;
    el.textContent = text;
    el.classList.remove("hidden");
  }

  // syncFile flips every checkbox for `file` (the rail item and the file
  // card's own header both toggle the same review state) and the "done"
  // styling on their row, then recomputes the rail's aggregate numbers.
  function syncFile(file, reviewed) {
    document.querySelectorAll(".rev-toggle").forEach(function (el) {
      if (el.dataset.file !== file) return;
      el.checked = reviewed;
      var row = el.closest(".fitem, .file-h");
      if (row) row.classList.toggle("done", reviewed);
    });
    updateProgress();
  }

  function updateProgress() {
    var boxes = document.querySelectorAll(".rail-toggle");
    var total = boxes.length, done = 0;
    boxes.forEach(function (b) { if (b.checked) done++; });

    var progTxt = document.getElementById("prog-txt");
    if (progTxt) progTxt.textContent = done + " / " + total;
    var bar = document.getElementById("prog-bar");
    if (bar) bar.style.width = (total ? (done / total * 100) : 0) + "%";

    var btn = document.getElementById("approve-btn");
    if (!btn) return;
    var allDone = total > 0 && done === total;
    btn.disabled = !allDone;
    btn.textContent = allDone
      ? "✓ Approve & merge to " + btn.dataset.base
      : "✓ Approve & merge (" + (total - done) + " left)";
  }

  document.addEventListener("change", function (e) {
    if (!e.target.classList || !e.target.classList.contains("rev-toggle")) return;
    var el = e.target;
    var file = el.dataset.file, hash = el.dataset.fileHash, reviewed = el.checked;
    syncFile(file, reviewed); // optimistic

    postJSON("/api/review", { id: wtID, file: file, reviewed: reviewed, hash: hash })
      .then(function (r) {
        if (r.ok) return;
        return r.text().then(function (msg) {
          syncFile(file, !reviewed); // revert
          showBanner(banner, r.status === 409
            ? "file changed since you loaded this page — reload to continue"
            : "could not update review state: " + msg);
        });
      })
      .catch(function () {
        syncFile(file, !reviewed);
        showBanner(banner, "network error updating review state — reload to continue");
      });
  });

  function showTerminal(res) {
    var term = document.getElementById("terminal");
    var base = document.getElementById("terminal-base");
    if (base) base.textContent = res.into || "";
    if (wrap) wrap.classList.add("hidden");
    if (term) term.classList.remove("hidden");
  }

  var approveBtn = document.getElementById("approve-btn");
  if (approveBtn) {
    approveBtn.addEventListener("click", function () {
      if (approveBtn.disabled) return;
      approveBtn.disabled = true;
      var errEl = document.getElementById("approve-error");
      if (errEl) errEl.classList.add("hidden");

      postJSON("/api/approve", { id: wtID })
        .then(function (r) {
          if (r.ok) return r.json().then(showTerminal);
          return r.text().then(function (msg) {
            approveBtn.disabled = false;
            showBanner(errEl, msg);
          });
        })
        .catch(function () {
          approveBtn.disabled = false;
          showBanner(errEl, "network error — try again");
        });
    });
  }
})();
