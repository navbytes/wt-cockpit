package web

import "net/http"

// handleFragmentWorktrees answers GET /fragment/worktrees: the same
// repo-grouped worktree list the index page renders inline, bare (no layout
// wrapper around it) — what app.js swaps into the page's #worktrees
// container on every SSE-observed change (P4-design.md §1.6).
func (a *app) handleFragmentWorktrees(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.tmpl["index"].ExecuteTemplate(w, "worktrees", a.buildIndexView()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// handleFragmentRail answers GET /fragment/rail?id=<wt>: the room's right
// rail (file checklist, progress, approve button), bare — what app.js swaps
// into #rail on a worktree.upserted for the open id (P4-design.md §1.6), so a
// review made from a second tab/the CLI/an agent shows up here without a
// reload. Reuses buildRoomView wholesale (same cost profile as the full page:
// the highlight LRU already absorbs the redundant per-file pass when content
// hasn't changed) rather than a second, narrower view-builder — one source of
// truth for "what the rail shows" beats two that could drift.
func (a *app) handleFragmentRail(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	d, ok := a.eng.Diff(id)
	if !ok {
		http.Error(w, "unknown worktree", http.StatusNotFound)
		return
	}
	view := a.buildRoomView(id, d, a.eng.Registry().Get(id), nil)
	if err := a.tmpl["room"].ExecuteTemplate(w, "rail", view); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// handleFragmentComments answers GET /fragment/comments?id=<wt>: every
// file's comment groups plus the orphaned section, bare — what app.js
// refetches on comment.changed (add/resolve/delete, from any client) and on
// diff.ready (a file's Stale bit can flip the instant the file's content
// moves, independent of whether the user reloads for the diff itself).
//
// app.js reconciles per FILE container (#comments-<idx>, always rendered by
// the room page even for a file with zero comments — see room.tmpl), not per
// comment group: an innerHTML swap of one file's whole strip trivially
// covers a group's first comment appearing, its last comment being deleted,
// and everything in between, with no separate insert/update/remove-stale
// bookkeeping. The orphaned section is refreshed the same way, as one unit.
// A file that no longer has a live container at all (its position in the
// diff changed since page load) is the one real "anchor missing" case —
// app.js shows a "comments updated — reload" chip for it (P4-design.md §1.6).
func (a *app) handleFragmentComments(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	d, ok := a.eng.Diff(id)
	if !ok {
		http.Error(w, "unknown worktree", http.StatusNotFound)
		return
	}
	views, _ := a.eng.Comments(id)
	if err := a.tmpl["room"].ExecuteTemplate(w, "commentBlocks", buildCommentsFragmentView(d, views)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
