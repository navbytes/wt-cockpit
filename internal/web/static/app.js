"use strict";

// app.js — progressive enhancement only. Boundary rules (P4-design.md §1.2):
// this file never builds markup from response *data*. innerHTML is only ever
// replaced by a whole fragment fetched same-origin from wtd's /fragment/*
// endpoints (server-escaped by construction, per the two-producer
// template.HTML rule in internal/web), and the one place that assignment
// happens is the shared swapInnerHTML helper below.

// ---- shared helpers (index page + reading room) ----

// swapInnerHTML is the ONLY place in this file an innerHTML assignment
// happens — every fragment consumer below (worktrees list, rail, per-file and
// orphaned comment sections) routes through it, so there is exactly one
// audited mutation site regardless of how many callers use it.
function swapInnerHTML(el, html) {
  if (el) el.innerHTML = html;
}

// connectEvents opens the shared SSE stream and wires the protocol
// handshake: the very first frame is always a named "hello" event (never
// delivered via onmessage) carrying the same {protocol,...} shape as GET
// /api/version. A mismatch against the page's own <meta name=
// "protocol-version"> means this tab was rendered by a since-upgraded/
// downgraded daemon — surfaced as a persistent banner rather than silently
// misinterpreting a shape this build doesn't understand (P4-design.md §1.6/§2).
function connectEvents(onMessage) {
  if (!window.EventSource) return null;
  var events = new EventSource("/api/events");
  events.addEventListener("hello", function (e) {
    var hello;
    try { hello = JSON.parse(e.data); } catch (err) { return; }
    var meta = document.querySelector('meta[name="protocol-version"]');
    var want = meta ? parseInt(meta.content, 10) : 0;
    if (hello.protocol !== want) {
      var banner = document.getElementById("proto-banner");
      if (banner) banner.classList.remove("hidden");
    }
  });
  events.onmessage = onMessage;
  return events;
}

function csrfToken() {
  var m = document.querySelector('meta[name="csrf-token"]');
  return m ? m.content : "";
}

// csrfStaleMsg must match middleware.go's csrfMismatchMsg verbatim — the one
// specific reload trigger (P4-design.md §1.3/§2): the daemon restarted
// mid-session and minted a new per-process token, so nothing this tab POSTs
// can succeed until it reloads and picks up the new one.
var csrfStaleMsg = "csrf token mismatch — reload the page";

// postJSON is the shared POST helper for every state-changing request on this
// page (review, approve, comments). It centralises the CSRF-stale reload so
// every caller gets it for free instead of each needing its own copy.
function postJSON(url, body) {
  return fetch(url, {
    method: "POST",
    credentials: "same-origin",
    headers: { "Content-Type": "application/json", "X-Csrf-Token": csrfToken() },
    body: JSON.stringify(body)
  }).then(function (r) {
    if (r.status === 403) {
      r.clone().text().then(function (msg) {
        if (msg.trim() === csrfStaleMsg) location.reload();
      });
    }
    return r;
  });
}

function showBanner(el, text) {
  if (!el) return;
  el.textContent = text;
  el.classList.remove("hidden");
}

// ---- index page: live worktree list ----
(function () {
  function swapWorktrees() {
    fetch("/fragment/worktrees", { credentials: "same-origin" })
      .then(function (r) { return r.ok ? r.text() : null; })
      .then(function (html) {
        if (html !== null) swapInnerHTML(document.getElementById("worktrees"), html);
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

  if (document.getElementById("worktrees")) connectEvents(scheduleSwap);
})();

// ---- reading room: review/approve, comments, live SSE refresh ----
// Same boundary rules as above: no markup is ever built from response data
// here, only checked/disabled/textContent/class changes, values read out of
// the already-escaped, already-rendered page (dataset attributes), a static
// <template> clone (P4-design.md §1.2 rule (d)), or a whole fetched fragment
// applied via swapInnerHTML.
(function () {
  var paneH = document.querySelector(".pane-h");
  if (!paneH) return; // not the room page (404 page, or the index)

  var wtID = paneH.dataset.id;
  var wrap = document.querySelector(".review-wrap"); // absent on an empty diff
  var banner = document.getElementById("banner");

  // ---- review toggle + approve (P4-design.md WP3; approve rewired to event
  // delegation in WP4 since the rail — and the approve button inside it —
  // can now be replaced wholesale by refreshRail()) ----

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

  // Delegated (not a direct element reference) so the approve button keeps
  // working after refreshRail() replaces #rail's contents wholesale.
  document.addEventListener("click", function (e) {
    var btn = e.target.closest("#approve-btn");
    if (!btn || btn.disabled) return;
    btn.disabled = true;
    var errEl = document.getElementById("approve-error");
    if (errEl) errEl.classList.add("hidden");

    postJSON("/api/approve", { id: wtID })
      .then(function (r) {
        if (r.ok) return r.json().then(showTerminal);
        return r.text().then(function (msg) {
          btn.disabled = false;
          showBanner(errEl, msg);
        });
      })
      .catch(function () {
        btn.disabled = false;
        showBanner(errEl, "network error — try again");
      });
  });

  // ---- comments: composer (clone-and-fill a static <template>), submit,
  // cancel, resolve/delete (P4-design.md WP4) ----

  function composerLabel(line, side) {
    return (Number(line) === 0) ? "file-level comment" : "line " + line + " (" + side + ")";
  }

  function closeComposer() {
    var existing = document.querySelector(".composer");
    if (existing) existing.remove();
  }

  function openComposer(fileIdx, file, line, side) {
    closeComposer();
    var tpl = document.getElementById("tpl-comment-composer");
    var host = document.getElementById("comments-" + fileIdx);
    if (!tpl || !host) return;
    var frag = tpl.content.cloneNode(true);
    var form = frag.querySelector(".composer");
    form.dataset.file = file;
    form.dataset.line = line;
    form.dataset.side = side;
    var lab = frag.querySelector(".composer-lab");
    if (lab) lab.textContent = composerLabel(line, side);
    host.appendChild(frag);
    var ta = host.querySelector(".composer-body");
    if (ta) ta.focus();
  }

  document.addEventListener("click", function (e) {
    var trigger = e.target.closest(".ln[data-line], .c-add");
    if (trigger) {
      openComposer(trigger.dataset.fileIdx, trigger.dataset.file, trigger.dataset.line, trigger.dataset.side);
      return;
    }
    if (e.target.closest(".composer-cancel")) closeComposer();
  });

  function setComposerError(form, msg) {
    var el = form.querySelector(".composer-error");
    if (el) { el.textContent = msg; el.classList.remove("hidden"); }
  }

  document.addEventListener("submit", function (e) {
    var form = e.target;
    if (!form.classList || !form.classList.contains("composer")) return;
    e.preventDefault();
    var ta = form.querySelector(".composer-body");
    var body = ta ? ta.value.trim() : "";
    if (!body) return;
    var submitBtn = form.querySelector('button[type="submit"]');
    if (submitBtn) submitBtn.disabled = true;

    postJSON("/api/comments", {
      id: wtID, file: form.dataset.file, line: parseInt(form.dataset.line, 10) || 0,
      side: form.dataset.side, body: body
    }).then(function (r) {
      if (r.ok) { form.remove(); return; }
      return r.text().then(function (msg) { setComposerError(form, msg); });
    }).catch(function () {
      setComposerError(form, "network error — try again");
    }).then(function () {
      if (submitBtn) submitBtn.disabled = false;
    });
  });

  document.addEventListener("click", function (e) {
    var btn = e.target.closest(".c-resolve, .c-delete");
    if (!btn) return;
    var action = btn.classList.contains("c-resolve") ? "resolve" : "delete";
    btn.disabled = true;
    postJSON("/api/comments/" + action, { id: wtID, commentId: btn.dataset.commentId })
      .then(function (r) {
        if (r.ok) return;
        return r.text().then(function (msg) {
          btn.disabled = false;
          showBanner(banner, msg);
        });
      })
      .catch(function () {
        btn.disabled = false;
        showBanner(banner, "network error — try again");
      });
  });

  // ---- live refresh: rail + comments fragments, diff-changed / removed
  // (P4-design.md §1.6) ----

  function refreshRail() {
    fetch("/fragment/rail?id=" + encodeURIComponent(wtID), { credentials: "same-origin" })
      .then(function (r) { return r.ok ? r.text() : null; })
      .then(function (html) {
        if (html !== null) swapInnerHTML(document.getElementById("rail"), html);
      });
  }

  // applyCommentsFragment distributes the fetched, already-escaped fragment's
  // per-file (and orphaned) blocks into their matching live containers —
  // every file card always carries an (at minimum empty) #comments-<idx>
  // host, so the only real "anchor missing" case is a file whose position in
  // the diff shifted since the page loaded (P4-design.md §1.6's chip state).
  function applyCommentsFragment(html) {
    var tpl = document.createElement("template");
    swapInnerHTML(tpl, html); // parses into tpl.content as inert DOM, nothing live yet
    var missing = false;
    tpl.content.querySelectorAll("[data-file-idx], #orphaned-comments").forEach(function (fresh) {
      var live = fresh.id ? document.getElementById(fresh.id) : null;
      if (live) swapInnerHTML(live, fresh.innerHTML);
      else missing = true;
    });
    if (missing) showBanner(banner, "comments updated — reload to see them all");
  }

  function refreshComments() {
    fetch("/fragment/comments?id=" + encodeURIComponent(wtID), { credentials: "same-origin" })
      .then(function (r) { return r.ok ? r.text() : null; })
      .then(function (html) { if (html !== null) applyCommentsFragment(html); });
  }

  function hasOpenDraft() {
    var ta = document.querySelector(".composer-body");
    return !!(ta && ta.value.trim() !== "");
  }

  function handleServerEvent(e) {
    var data;
    try { data = JSON.parse(e.data); } catch (err) { return; }
    if (!data || data.id !== wtID) return;

    switch (data.type) {
      case "worktree.upserted":
        if (data.worktree && data.worktree.stats) {
          var addEl = document.querySelector(".pane-h .sum .add");
          var delEl = document.querySelector(".pane-h .sum .del");
          if (addEl) addEl.textContent = "+" + data.worktree.stats.add;
          if (delEl) delEl.textContent = "−" + data.worktree.stats.del;
        }
        refreshRail();
        break;
      case "diff.ready":
        refreshComments(); // a comment's stale badge can flip the instant the file changes, independent of reloading for the diff content itself
        if (data.hash && paneH.dataset.diffHash && data.hash !== paneH.dataset.diffHash) {
          showBanner(banner, "diff changed — reload to see the latest");
          if (!hasOpenDraft()) location.reload();
        }
        break;
      case "worktree.removed":
        var approveBtn = document.getElementById("approve-btn");
        showTerminal({ into: approveBtn ? approveBtn.dataset.base : "" });
        break;
      case "comment.changed":
        refreshComments();
        break;
    }
  }

  connectEvents(handleServerEvent);
})();
