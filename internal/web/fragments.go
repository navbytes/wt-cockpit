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
