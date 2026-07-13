package web

import (
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/navbytes/wt-cockpit/internal/model"
)

// templateFuncs are the functions every page template can call directly
// (P4-design.md §3's "template funcs" note on this file). dirsplit — dir
// prefix vs filename for the room's per-file cards — has no caller yet: WP2's
// room is a stub with no file cards, so it's added in WP3 alongside the
// template that actually needs it (ponytail: no unused abstractions).
var templateFuncs = template.FuncMap{
	"reltime":    reltime,
	"guardrails": guardrailSummary,
}

// pageHeader is embedded by every page's view struct: the two things
// layout.tmpl itself needs.
type pageHeader struct {
	Title     string
	CSRFToken string
}

// indexView is GET /'s (and /fragment/worktrees') template data.
type indexView struct {
	pageHeader
	Roots  []string
	Groups []repoGroup
	Counts counts
}

// repoGroup is one repo's worktrees, in the index's tree-like grouping
// (mock's .proj sections). ponytail: grouped by Worktree.Repo (the name)
// only — the mock also shows each repo's language tag and path, but neither
// is available off model.Worktree without a new engine/registry accessor
// (Worktree.Repo is just the owning repo's name); that's a WP3/UX-fidelity
// concern, not required by WP2's done-when (index renders live worktrees).
type repoGroup struct {
	Repo      string
	Worktrees []model.Worktree
}

// counts are the topbar's live numbers (mock: worktrees/active/unreviewed/
// alerts), computed the same way cmd/wt's renderRadar does.
type counts struct {
	Worktrees, Active, Unreviewed, Alerts int
}

// buildIndexView assembles the index page's (and its fragment's) data from a
// fresh engine snapshot every time — cheap (an in-memory list), and it's what
// keeps the fragment endpoint and the full page always in sync.
func (a *app) buildIndexView() indexView {
	wts := a.eng.List()

	byRepo := map[string][]model.Worktree{}
	var order []string
	var c counts
	for _, w := range wts {
		c.Worktrees++
		if w.State == model.StateActive {
			c.Active++
		}
		if w.Stats.Files > 0 && w.Reviewed < w.Stats.Files {
			c.Unreviewed++
		}
		c.Alerts += len(w.Guardrails)

		if _, ok := byRepo[w.Repo]; !ok {
			order = append(order, w.Repo)
		}
		byRepo[w.Repo] = append(byRepo[w.Repo], w)
	}
	sort.Strings(order)

	groups := make([]repoGroup, 0, len(order))
	for _, repo := range order {
		groups = append(groups, repoGroup{Repo: repo, Worktrees: byRepo[repo]})
	}

	return indexView{
		pageHeader: pageHeader{Title: "wt cockpit", CSRFToken: a.cfg.CSRFToken},
		Roots:      a.cfg.Roots,
		Groups:     groups,
		Counts:     c,
	}
}

func (a *app) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.tmpl["index"].ExecuteTemplate(w, "layout", a.buildIndexView()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// roomStubContent is WP2's placeholder for /wt/{id} (P4-design.md WP2: "room
// page is a stub card 'reading room: WP3'"). It renders unconditionally for
// any id, real or unknown alike — the 404/terminal states are WP3's job,
// once there's an actual diff to find or not find. Parsed into a clone of
// layout.tmpl rather than living in its own templates/room.tmpl file: that
// filename is WP3's to add alongside the real room template.
const roomStubContent = `{{define "content"}}
<div class="topbar"><div class="brand"><span>wt</span> cockpit</div></div>
<div class="card">
  <h2>reading room: WP3</h2>
  <p>Side-by-side review for <code>{{.ID}}</code> lands in WP3.</p>
  <p><a href="/">&larr; back to worktrees</a></p>
</div>
{{end}}`

type roomView struct {
	pageHeader
	ID string
}

func (a *app) handleRoom(w http.ResponseWriter, r *http.Request) {
	view := roomView{
		pageHeader: pageHeader{Title: "reading room", CSRFToken: a.cfg.CSRFToken},
		ID:         r.PathValue("id"),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.tmpl["room"].ExecuteTemplate(w, "layout", view); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// reltime renders a coarse human-relative age ("3s ago", "6m ago", ...),
// matching the mock's worktree rows (§2). A zero time (no recorded change
// yet) renders as "" rather than a multi-decade-old nonsense duration.
func reltime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// guardrailSummary mirrors cmd/wt's shortGuard: the first couple of distinct
// tripped rule names, "…" if there were more.
func guardrailSummary(hits []model.GuardrailHit) string {
	seen := map[string]bool{}
	var parts []string
	for _, h := range hits {
		if !seen[h.Rule] {
			seen[h.Rule] = true
			parts = append(parts, h.Rule)
		}
	}
	if len(parts) > 2 {
		parts = append(parts[:2], "…")
	}
	return strings.Join(parts, ", ")
}
