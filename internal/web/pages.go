package web

import (
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/navbytes/wt-cockpit/internal/diffparse"
	"github.com/navbytes/wt-cockpit/internal/model"
)

// templateFuncs are the functions every page template can call directly
// (P4-design.md §3's "template funcs" note on this file). Room's per-file
// cards need a dir/filename split too (dirsplit, below) — it stays a plain
// Go helper rather than a template func because html/template rejects a
// function with two non-error return values, so it's called once per file
// from buildFileCard and stored on fileCardView instead.
var templateFuncs = template.FuncMap{
	"reltime":    reltime,
	"guardrails": guardrailSummary,
}

// pageHeader is embedded by every page's view struct: what layout.tmpl itself
// needs. Protocol is model.ProtocolVersion, embedded as a <meta> tag so
// app.js can compare it against the SSE hello frame and banner "wtd was
// upgraded — reload" on a mismatch (P4-design.md §1.6/§2) without a second
// round trip to GET /api/version.
type pageHeader struct {
	Title     string
	CSRFToken string
	Protocol  int
}

// newPageHeader builds the header every page constructs identically save for
// its title.
func newPageHeader(title, csrfToken string) pageHeader {
	return pageHeader{Title: title, CSRFToken: csrfToken, Protocol: model.ProtocolVersion}
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
	Worktrees []worktreeRow
}

// worktreeRow is one worktree in the index's list: the raw model.Worktree
// (embedded, so index.tmpl's existing field references keep working
// unchanged) plus its own worst guardrail severity, computed once here so
// the badge can grade red/amber the same way the TUI sidebar badge
// (severityBadge) and CLI radar already do (ux-expert P1-1c) instead of a
// fixed amber regardless of severity.
type worktreeRow struct {
	model.Worktree
	Severity string // "danger" | "warn" | "" (no hits)
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

	byRepo := map[string][]worktreeRow{}
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
		byRepo[w.Repo] = append(byRepo[w.Repo], worktreeRow{Worktree: w, Severity: worstSeverity(w.Guardrails)})
	}
	sort.Strings(order)

	groups := make([]repoGroup, 0, len(order))
	for _, repo := range order {
		groups = append(groups, repoGroup{Repo: repo, Worktrees: byRepo[repo]})
	}

	return indexView{
		pageHeader: newPageHeader("wt cockpit", a.cfg.CSRFToken),
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

// roomView is GET /wt/{id}'s template data: the mock's Review view, side-by-
// side (P4-design.md §2/§1.4). NotFound short-circuits every other field
// (room.tmpl's "content" block checks it first) — an unknown or
// just-removed worktree id renders the 404 state rather than a zero-valued
// room.
type roomView struct {
	pageHeader
	ID       string
	NotFound bool
	Empty    bool // diff has zero files: "no changes vs base" card

	Repo, Name, Branch, Base string
	Agent                    model.AgentKind
	Stats                    model.Stats
	DiffHash                 string

	GuardrailMsg      string // featured hit's message, "" if none tripped
	GuardrailMore     int    // additional distinct hits beyond the featured one
	GuardrailSeverity string // featured hit's severity ("warn"|"danger"), "" if none tripped

	Files                                []fileCardView
	ReviewedCount, TotalFiles, LeftCount int
	AllReviewed                          bool

	// Dirty mirrors the worktree's registry State (P4-fixes.md #3): the
	// engine's Approve also gates on a clean worktree (engine.go's Gate 2,
	// IsDirty), which the room previously never surfaced before the click.
	// This is the *knowable* signal already on the payload — State collapses
	// "uncommitted changes" under "active" while the worktree was touched
	// recently (engine.state), so it's a best-effort pre-check, not a
	// replacement for the 409 backstop.
	Dirty bool

	// OrphanedComments are comments whose file left the diff entirely — no
	// file card exists to strip them under, so they render in their own
	// page-bottom section instead (P4-design.md §1.5/§2). Always populated
	// (possibly empty), never nil-vs-empty-sensitive: the section itself
	// always renders (comments.go/fragments.tmpl), just visually empty, so
	// app.js always has a container to refresh into.
	OrphanedComments []model.CommentView
}

// fileCardView is one DiffFile's rendered content: the header line (name,
// status tag, per-file guardrail severity tag, stats, reviewed checkbox)
// plus however its body renders (binary note, collapsed note, or the
// side-by-side hunks).
type fileCardView struct {
	Idx         int
	Path        string // == model.DiffFile.Path (data-file/anchor keys)
	DisplayPath string // "old -> new" for a rename, else Path
	Dir         string
	FileName    string
	Status      model.FileStatus
	HitSeverity string // this file's own worst guardrail severity ("danger"|"warn"), "" if none tripped
	Stats       model.Stats
	Reviewed    bool
	Hash        string

	Binary       bool
	Collapsed    bool
	CollapseNote string
	HighlightOff bool
	Hunks        []hunkView

	// CommentGroups is this file's comment threads, grouped by (line, side)
	// and rendered in a strip right under the header (comments.go) —
	// unconditional on Binary/Collapsed: a comment can be about a collapsed
	// or binary file too, and always has somewhere to render regardless of
	// whether its hunks do.
	CommentGroups []commentGroupView
}

// hunkView is one hunk's header text plus its side-by-side rows (sxs.go).
type hunkView struct {
	Header string
	Rows   []SxsRow
}

func (a *app) handleRoom(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	d, ok := a.eng.Diff(id)
	view := roomView{pageHeader: newPageHeader("reading room", a.cfg.CSRFToken), ID: id}
	if !ok {
		view.NotFound = true
		w.WriteHeader(http.StatusNotFound)
	} else {
		view = a.buildRoomView(id, d, a.eng.Registry().Get(id), r.URL.Query()["expand"])
	}
	if err := a.tmpl["room"].ExecuteTemplate(w, "layout", view); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// buildRoomView assembles the room page's (and eventually its fragments')
// data from a fresh engine snapshot. expandParams is the raw repeated
// ?expand= query values (pages.go's handleRoom passes r.URL.Query()["expand"]
// straight through) — each names one file path to render un-collapsed,
// P4-design.md §1.4's "plain link ... zero JS, works without script".
func (a *app) buildRoomView(id string, d model.Diff, wt *model.Worktree, expandParams []string) roomView {
	reviewedMap, _ := a.eng.ReviewedMap(id)
	expanded := make(map[string]bool, len(expandParams))
	for _, p := range expandParams {
		expanded[p] = true
	}
	// Comments are read fresh every render (never cached): Stale/Orphaned are
	// computed by the engine against the *current* diff on every call, which
	// is exactly what makes them flip live once a refetch happens (P4-design.md
	// §1.5's frozen semantics; the error case (an unknown id) can't actually
	// happen here since handleRoom already proved the diff exists).
	views, _ := a.eng.Comments(id)

	hits := guardrailHitsFor(wt)
	msg, more, severity := guardrailBanner(hits)
	sev := fileSeverity(hits)

	files := make([]fileCardView, len(d.Files))
	reviewedCount := 0
	for i, f := range d.Files {
		files[i] = a.buildFileCard(i, f, reviewedMap[f.Path], sev, expanded, views)
		if reviewedMap[f.Path] {
			reviewedCount++
		}
	}
	total := len(d.Files)

	view := roomView{
		pageHeader:        newPageHeader(roomTitle(wt, id), a.cfg.CSRFToken),
		ID:                id,
		Empty:             total == 0,
		DiffHash:          d.Hash,
		Files:             files,
		ReviewedCount:     reviewedCount,
		TotalFiles:        total,
		LeftCount:         total - reviewedCount,
		AllReviewed:       total > 0 && reviewedCount == total,
		GuardrailMsg:      msg,
		GuardrailMore:     more,
		GuardrailSeverity: severity,
		Base:              d.Base,
		OrphanedComments:  orphanedComments(views),
	}
	if wt != nil {
		view.Repo, view.Name, view.Branch = wt.Repo, wt.Name, wt.Branch
		view.Agent, view.Stats = wt.Agent, wt.Stats
		view.Dirty = wt.State == model.StateDirty
	}
	return view
}

// buildFileCard renders one DiffFile: binary and collapsed-by-default files
// get a note instead of hunks (guardrail thresholds mirrored from the TUI,
// highlight.go); everything else gets its side-by-side rows, sliced out of
// the file's single per-file highlight pass by a running codeIdx offset —
// the same "concatenated hunk lines, indexed 1:1" convention
// internal/tui/flatten.go's codeIdx uses.
func (a *app) buildFileCard(idx int, f model.DiffFile, reviewed bool, sev map[string]string, expanded map[string]bool, comments []model.CommentView) fileCardView {
	disp := displayPath(f)
	dir, base := dirsplit(disp)
	hitSev := sev[f.Path]
	if hitSev == "" && f.OldPath != "" {
		hitSev = sev[f.OldPath]
	}
	card := fileCardView{
		Idx: idx, Path: f.Path, DisplayPath: disp, Dir: dir, FileName: base,
		Status: f.Status, HitSeverity: hitSev,
		Stats: f.Stats, Reviewed: reviewed, Hash: f.Hash, Binary: f.Binary,
		CommentGroups: fileCommentGroups(comments, f.Path, idx),
	}

	switch {
	case f.Binary:
		// Nothing more to render — git never emits hunks for a binary file.
	case isCollapsedByDefault(f) && !expanded[f.Path]:
		card.Collapsed = true
		card.CollapseNote = fmt.Sprintf("collapsed (%d lines)", totalHunkLines(f))
	default:
		lines, off := a.linesForFile(f)
		card.HighlightOff = off
		card.Hunks = make([]hunkView, len(f.Hunks))
		codeIdx := 0
		for hi, h := range f.Hunks {
			end := codeIdx + len(h.Lines)
			var htmlSlice []template.HTML
			if end <= len(lines) {
				htmlSlice = lines[codeIdx:end]
			}
			card.Hunks[hi] = hunkView{Header: h.Header, Rows: SplitHunk(h, htmlSlice)}
			codeIdx = end
		}
	}
	return card
}

// roomTitle is the <title>/pane-header text: "repo / name", falling back to
// the bare id if the worktree vanished between the Diff and Registry reads
// (defensive only — refreshWorktree updates both together).
func roomTitle(wt *model.Worktree, id string) string {
	if wt == nil {
		return id
	}
	return wt.Repo + " / " + wt.Name
}

// guardrailHitsFor returns wt's tripped guardrails, or nil if wt vanished
// between reads (see roomTitle).
func guardrailHitsFor(wt *model.Worktree) []model.GuardrailHit {
	if wt == nil {
		return nil
	}
	return wt.Guardrails
}

// guardrailBanner mirrors internal/tui/radar.go's renderGuardrailBanner: the
// featured hit (the first "danger"-severity hit, else the first hit at all)
// verbatim, how many additional distinct hits there were, and the featured
// hit's own severity — so room.tmpl can tint the banner red for a danger hit
// instead of the one amber style every severity used to share (the "richer
// hits...show with correct severity" theme applies to the web banner exactly
// as it already does the TUI's sidebar badge).
//
// message runs through diffparse.SanitizeControl first. Diff content/paths
// are already sanitized upstream by diffparse.Parse, but a hit's Message can
// instead be hand-authored directly in a repo's own .wtcockpit.toml pack —
// semi-trusted input (P5-design.md §1.3) that never passes through that
// pipeline. html/template's contextual escaping alone handles `<script>`-
// shaped text but not raw control bytes, so without this a control byte
// smuggled into a pack's Message would reach the response body unescaped —
// harmless to the DOM itself, but not to a terminal reading the raw response
// (curl, view-source). Mirrors the identical defense in the TUI's own banner.
func guardrailBanner(hits []model.GuardrailHit) (message string, more int, severity string) {
	if len(hits) == 0 {
		return "", 0, ""
	}
	featured := hits[0]
	for _, h := range hits {
		if h.Severity == "danger" {
			featured = h
			break
		}
	}
	sev := featured.Severity
	if sev == "" {
		sev = "warn"
	}
	return diffparse.SanitizeControl(featured.Message), len(hits) - 1, sev
}

// worstSeverity grades hits the same "danger wins" rule every other
// severity-aware surface in this app already applies (internal/tui/
// sidebar.go's severityBadge, cmd/wt's renderRadar, guardrailBanner above):
// "danger" if ANY hit is that severity, else "warn" if there's at least one
// hit, else "" for none at all.
func worstSeverity(hits []model.GuardrailHit) string {
	sev := ""
	for _, h := range hits {
		if h.Severity == "danger" {
			return "danger"
		}
		sev = "warn"
	}
	return sev
}

// fileSeverity computes each named file's own worst severity — the file
// card's tag now carries this instead of a single "any hit = red danger"
// bool (ux-expert P1-1a), mirroring internal/tui/radar.go's identically-named
// helper: a warn-only rule (deps-manifest-changed, edits-ci, lockfile-churn,
// large-deletion, binary-added, secrets-entropy) no longer paints its file
// red, and — per room.tmpl — no longer replaces the file's own status tag.
func fileSeverity(hits []model.GuardrailHit) map[string]string {
	byFile := map[string][]model.GuardrailHit{}
	for _, h := range hits {
		if h.File != "" {
			byFile[h.File] = append(byFile[h.File], h)
		}
	}
	out := make(map[string]string, len(byFile))
	for f, hs := range byFile {
		out[f] = worstSeverity(hs)
	}
	return out
}

// displayPath is the file card's title text: "old -> new" for a rename,
// otherwise just the (git forward-slash) path. Mirrors
// internal/tui/flatten.go's identical helper; duplicated, not imported (web
// must not depend on the TUI package).
func displayPath(f model.DiffFile) string {
	if f.Status == model.FileRenamed && f.OldPath != "" && f.OldPath != f.Path {
		return f.OldPath + " → " + f.Path
	}
	return f.Path
}

// dirsplit splits a git diff path (always "/"-separated, regardless of host
// OS) into a dim directory prefix and the base name, matching the mock's
// "path with dim dir prefix" file card treatment. Byte-splitting on '/' is
// safe for any valid UTF-8 path: '/' never appears as, or inside, a
// multi-byte rune's continuation bytes. Mirrors
// internal/tui/flatten.go's splitDirBase.
func dirsplit(p string) (dir, base string) {
	i := strings.LastIndexByte(p, '/')
	if i < 0 {
		return "", p
	}
	return p[:i+1], p[i+1:]
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
