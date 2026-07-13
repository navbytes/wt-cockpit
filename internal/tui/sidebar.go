package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/navbytes/wt-cockpit/internal/model"
)

// sidebarRow is one line in the sidebar's flattened row list: either a repo
// group header (non-selectable) or a worktree row.
type sidebarRow struct {
	header bool
	repo   string // header text when header; owning repo name otherwise
	wt     model.Worktree
}

// sidebar is the tree component: repo group headers (alphabetical), worktree
// rows sorted most-recent-first within each repo (matches `wt ls`/registry.
// List's order), cursor movement that skips headers, and selection keyed by
// worktree ID rather than row index so a live re-sort or upsert never moves
// the user's cursor out from under them (P3-design.md §1.1). Filter/search
// (`/`, `f`) is WP3 — setWorktrees always shows every worktree it's given.
type sidebar struct {
	rows       []sidebarRow
	selectedID string
	hasData    bool // Worktrees() has succeeded at least once (even if empty)

	// WP3 filtering (P3-design.md §1.3 "/" and "f"). filterInput.Value() is
	// the live fuzzy-substring query regardless of focus state — committing
	// (⏎) just blurs it, clearing (esc) blanks it; reading Value() on a
	// never-initialized zero-value Model is safe (always ""), so no explicit
	// constructor is needed for a plain `var s sidebar`/zero-value appModel.
	filterInput textinput.Model
	activeOnly  bool // "f" toggle — active-only worktrees
}

// setWorktrees replaces the full worktree set (a fresh GET /api/worktrees or
// a snapshot-triggered resync).
func (s *sidebar) setWorktrees(wts []model.Worktree) {
	s.hasData = true
	s.rows = buildRows(wts)
	s.fixSelection()
}

// upsert applies a worktree.upserted delta in place — update if present,
// insert if new — and re-derives the sorted row list, never refetching the
// world (P3-design.md §2.4).
func (s *sidebar) upsert(w model.Worktree) {
	s.hasData = true
	wts := s.worktrees()
	found := false
	for i, existing := range wts {
		if existing.ID == w.ID {
			wts[i] = w
			found = true
			break
		}
	}
	if !found {
		wts = append(wts, w)
	}
	s.rows = buildRows(wts)
	s.fixSelection()
}

// remove applies a worktree.removed delta: drop the row; if it was
// selected, selection moves to the next available row.
func (s *sidebar) remove(id string) {
	wts := s.worktrees()
	out := wts[:0]
	for _, w := range wts {
		if w.ID != id {
			out = append(out, w)
		}
	}
	wasSelected := s.selectedID == id
	s.rows = buildRows(out)
	if wasSelected {
		s.selectedID = ""
	}
	s.fixSelection()
}

// worktrees extracts the current flat worktree list back out of rows: the
// canonical storage is the sorted/grouped row list, so upsert/remove always
// re-derive through the same grouping logic setWorktrees uses.
func (s *sidebar) worktrees() []model.Worktree {
	wts := make([]model.Worktree, 0, len(s.rows))
	for _, r := range s.rows {
		if !r.header {
			wts = append(wts, r.wt)
		}
	}
	return wts
}

// fixSelection keeps selectedID pointing at a real, currently-*visible* row
// when possible: if it still exists in the filtered view after a rebuild
// (or after the filter itself changed), leave it; otherwise pick the first
// visible row, or "" if there are none (empty workspace, or everything
// filtered out).
func (s *sidebar) fixSelection() {
	rows := s.filtered()
	if s.selectedID != "" {
		for _, r := range rows {
			if !r.header && r.wt.ID == s.selectedID {
				return
			}
		}
	}
	for _, r := range rows {
		if !r.header {
			s.selectedID = r.wt.ID
			return
		}
	}
	s.selectedID = ""
}

// buildRows groups wts by repo (alphabetical) and sorts each group
// most-recent-change-first, interleaving one header row per repo.
func buildRows(wts []model.Worktree) []sidebarRow {
	byRepo := map[string][]model.Worktree{}
	var repos []string
	for _, w := range wts {
		if _, ok := byRepo[w.Repo]; !ok {
			repos = append(repos, w.Repo)
		}
		byRepo[w.Repo] = append(byRepo[w.Repo], w)
	}
	sort.Strings(repos)

	var rows []sidebarRow
	for _, repo := range repos {
		rows = append(rows, sidebarRow{header: true, repo: repo})
		group := byRepo[repo]
		sort.SliceStable(group, func(i, j int) bool {
			if !group[i].LastChange.Equal(group[j].LastChange) {
				return group[i].LastChange.After(group[j].LastChange)
			}
			return group[i].Name < group[j].Name
		})
		for _, w := range group {
			rows = append(rows, sidebarRow{repo: repo, wt: w})
		}
	}
	return rows
}

// moveDown/moveUp move the selection to the next/previous selectable
// (non-header) row. Moving past an end is a no-op — the mock has no
// wraparound cue.
func (s *sidebar) moveDown() { s.move(1) }
func (s *sidebar) moveUp()   { s.move(-1) }

func (s *sidebar) move(delta int) {
	rows := s.filtered()
	idx := -1
	for i, r := range rows {
		if !r.header && r.wt.ID == s.selectedID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return
	}
	for i := idx + delta; i >= 0 && i < len(rows); i += delta {
		if !rows[i].header {
			s.selectedID = rows[i].wt.ID
			return
		}
	}
}

func (s *sidebar) selectedIndex() int {
	for i, r := range s.filtered() {
		if !r.header && r.wt.ID == s.selectedID {
			return i
		}
	}
	return -1
}

// selected returns the currently selected worktree, or false if there is
// none (empty workspace, or the selection has been filtered out).
func (s *sidebar) selected() (model.Worktree, bool) {
	for _, r := range s.filtered() {
		if !r.header && r.wt.ID == s.selectedID {
			return r.wt, true
		}
	}
	return model.Worktree{}, false
}

// ---- WP3: "/" search + "f" active-only filter (P3-design.md §1.3) ----

// searching reports whether the search input currently has keyboard focus
// (app.go routes every key to it while true).
func (s *sidebar) searching() bool { return s.filterInput.Focused() }

// hasFilter reports whether a query or the active-only toggle would narrow
// the visible rows right now — the esc ladder's "clears active search/
// filter first if one is set" rung checks this.
func (s *sidebar) hasFilter() bool {
	return s.filterInput.Value() != "" || s.activeOnly
}

// clearFilter resets both the query and the active-only toggle together —
// esc's "back to everything" rung.
func (s *sidebar) clearFilter() {
	s.filterInput.SetValue("")
	s.activeOnly = false
	s.fixSelection()
}

// startSearch opens the search input, seeded with whatever query was
// already committed (so re-opening `/` to refine a filter doesn't lose it).
// A fresh textinput.Model is (re)built here — its zero value has a nil
// Cursor/KeyMap and isn't safe to Focus/Update/View until textinput.New has
// run, which this is the one path that ever needs to.
func (s *sidebar) startSearch() {
	prev := s.filterInput.Value()
	s.filterInput = textinput.New()
	s.filterInput.Prompt = "/ "
	s.filterInput.SetValue(prev)
	s.filterInput.CursorEnd()
	s.filterInput.Focus()
}

// commitSearch is "/"'s ⏎: stop capturing keys but keep the query applied.
func (s *sidebar) commitSearch() {
	s.filterInput.Blur()
	s.fixSelection()
}

// cancelSearch is "/"'s esc while typing: blank the query entirely and stop
// capturing keys (distinct from the general esc ladder's clearFilter, which
// only fires once you're no longer in the input at all — same end state,
// reached from a different key context).
func (s *sidebar) cancelSearch() {
	s.filterInput.SetValue("")
	s.filterInput.Blur()
	s.fixSelection()
}

// updateSearchInput forwards one keystroke to the text input and re-derives
// the live filter (P3-design.md: "⏎ keeps filter" implies the filter is
// already live while typing, not only once committed).
func (s *sidebar) updateSearchInput(msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	s.filterInput, cmd = s.filterInput.Update(msg)
	s.fixSelection()
	return cmd
}

// filtered returns the rows actually shown/navigable given the current
// query and active-only toggle: a fuzzy-substring match (case-insensitive
// substring — P3-design.md doesn't specify a scored fuzzy algorithm, and a
// plain substring check is the simplest thing satisfying "fuzzy-substring")
// against repo/name/branch, ANDed with the active-only toggle. A repo's
// header is kept only if at least one of its worktrees still matches.
// Returns s.rows unchanged (the fast path, and the only path every pre-WP3
// caller/test exercises) when neither filter is active.
func (s *sidebar) filtered() []sidebarRow {
	query := s.filterInput.Value()
	if query == "" && !s.activeOnly {
		return s.rows
	}
	var out []sidebarRow
	var pendingHeader *sidebarRow
	for i := range s.rows {
		r := s.rows[i]
		if r.header {
			h := r
			pendingHeader = &h
			continue
		}
		if !matchesFilter(r.wt, query, s.activeOnly) {
			continue
		}
		if pendingHeader != nil {
			out = append(out, *pendingHeader)
			pendingHeader = nil
		}
		out = append(out, r)
	}
	return out
}

// filterStatusLine is the dim reminder shown once a filter is applied but
// the search input isn't currently focused (so the user knows why rows are
// missing, and that esc clears it).
func filterStatusLine(query string, activeOnly bool) string {
	var parts []string
	if query != "" {
		parts = append(parts, "/"+query)
	}
	if activeOnly {
		parts = append(parts, "active-only")
	}
	return "  " + strings.Join(parts, " · ") + " (esc clears)"
}

// matchesFilter is filtered()'s per-worktree predicate.
func matchesFilter(w model.Worktree, query string, activeOnly bool) bool {
	if activeOnly && w.State != model.StateActive {
		return false
	}
	if query == "" {
		return true
	}
	q := strings.ToLower(query)
	return strings.Contains(strings.ToLower(w.Repo), q) ||
		strings.Contains(strings.ToLower(w.Name), q) ||
		strings.Contains(strings.ToLower(w.Branch), q)
}

// view renders the sidebar into a width-constrained block. Rows are the
// mock's two-line layout (renderSidebarRow, ux-expert P1-1). WP3 adds the
// "/" search input (shown only while focused) and a dim status line once a
// query/active-only filter is applied but no longer being typed. conn drives
// the header's LIVE/PAUSED truthfulness (ux-expert P2-4): while a mid-session
// SSE reconnect is in flight, the data on screen is stale, and the header
// says so instead of silently claiming LIVE.
func (s *sidebar) view(width, height int, conn connState) string {
	var b strings.Builder
	header, headerStyle := "WORKTREES · LIVE", styles.Faint
	if conn == connReconnecting {
		header, headerStyle = "WORKTREES · PAUSED", styles.Warn
	}
	fmt.Fprintln(&b, headerStyle.Render(header))

	switch {
	case s.searching():
		fmt.Fprintln(&b, s.filterInput.View())
	case s.hasFilter():
		fmt.Fprintln(&b, styles.Dim.Render(filterStatusLine(s.filterInput.Value(), s.activeOnly)))
	}

	rows := s.filtered()
	switch {
	case !s.hasData:
		fmt.Fprint(&b, styles.Dim.Render("  connecting to wtd…"))
	case len(rows) == 0 && len(s.rows) > 0:
		fmt.Fprint(&b, styles.Dim.Render("  no worktrees match the filter"))
	case len(rows) == 0:
		fmt.Fprint(&b, styles.Dim.Render("  no worktrees — wtd -root <dir> or edit ~/.config/wtcockpit/config.toml"))
	default:
		for i, r := range rows {
			if i > 0 {
				fmt.Fprintln(&b)
			}
			if r.header {
				fmt.Fprint(&b, styles.RepoHeader.Render(r.repo))
				continue
			}
			fmt.Fprint(&b, renderSidebarRow(r.wt, r.wt.ID == s.selectedID, width))
		}
	}
	return lipgloss.NewStyle().Width(width).Height(height).Render(b.String())
}

// renderSidebarRow is the mock's two-line worktree row (ux-expert P1-1,
// P3-design.md §1.1's ASCII sketch):
//
//	● name                    +240 −96
//	  [chip] 3s ago  8 files ⚠
//
// Every piece is packed via packRow, which *guarantees* each line is exactly
// width display cells regardless of how long the name/agent/age/counts are —
// the previous single-line row had no such guarantee, so an ordinary name
// (or the selected row's own left border) could push a line one cell past
// the sidebar's fixed width. lipgloss's Width-wrap (not truncate) then
// silently wrapped that row's overflow onto a new visual line inside the
// sidebar's own column — and since relativeTime's text grows a character at
// various points ("9s ago" -> "10s ago"), a row already right at the edge
// could start wrapping on a later tick with no other state change: the
// "reflow on clock tick" bug this rewrite kills by construction (see
// TestRenderSidebarRowAlwaysExactlyTwoLinesWithinRailWidth).
func renderSidebarRow(w model.Worktree, selected bool, width int) string {
	contentWidth := width
	if selected {
		contentWidth-- // the accent border below owns one column of its own
	}
	if contentWidth < 0 {
		contentWidth = 0
	}

	dot := stateDotStyle(w.State).Render("●")
	nameStyle := styles.Txt
	if selected {
		nameStyle = nameStyle.Bold(true) // mock's ".wt.sel .name{color:#fff}" — brighter, via weight
	}
	name := nameStyle.Render(w.Name)
	stats := styles.Add.Render(fmt.Sprintf("+%d", w.Stats.Add)) + " " + styles.Del.Render(fmt.Sprintf("-%d", w.Stats.Del))
	line1 := packRow(fmt.Sprintf(" %s %s", dot, name), stats, contentWidth)

	agent := agentChipStyle(w.Agent).Render(string(w.Agent))
	age := styles.Dim.Render(relativeTime(w.LastChange))
	right2 := styles.Dim.Render(fmt.Sprintf("%d files", w.Stats.Files))
	if badge := severityBadge(w.Guardrails); badge != "" {
		right2 += " " + badge
	}
	line2 := packRow(fmt.Sprintf("   %s %s", agent, age), right2, contentWidth)

	if !selected {
		return line1 + "\n" + line2
	}
	border := styles.Accent.Render("│")
	bg := bgPrefix(styles.SelectedBg)
	return border + tintRow(bg, line1) + "\n" + border + tintRow(bg, line2)
}

// severityBadge is the sidebar row's ⚠ guardrail indicator (ux-expert
// P2-6/P3-design.md §1.1): worst severity wins — red ("danger" style) if any
// hit is severity "danger", else amber warn. Empty when there are no hits.
func severityBadge(hits []model.GuardrailHit) string {
	if len(hits) == 0 {
		return ""
	}
	for _, h := range hits {
		if h.Severity == "danger" {
			return styles.Del.Render("⚠")
		}
	}
	return styles.Warn.Render("⚠")
}

// packRow lays left flush-start and right flush-end within *exactly* width
// display columns (ux-expert P1-1) — never wraps, never overshoots. left
// (the name, or the agent+age pair) is unbounded in principle, so it's
// clipped first to whatever room remains once right (a short, bounded
// handful of digits/glyphs) has taken its share; fitWidth is then a hard
// backstop that pads or truncates the joined result to exactly width, the
// same never-wrap contract every other row/header in this package already
// follows (diffview.go's clipWidth/fitWidth).
func packRow(left, right string, width int) string {
	if width < 0 {
		width = 0
	}
	rw := lipgloss.Width(right)
	budget := width - rw - 1 // 1-col minimum gap between left and right
	if budget < 0 {
		budget = 0
	}
	left = clipWidth(left, budget)
	gap := width - lipgloss.Width(left) - rw
	if gap < 0 {
		gap = 0
	}
	return fitWidth(left+strings.Repeat(" ", gap)+right, width)
}

func stateDotStyle(s model.WorktreeState) lipgloss.Style {
	switch s {
	case model.StateActive:
		return styles.Add
	case model.StateDirty:
		return styles.Warn
	default:
		return styles.Faint
	}
}

func agentChipStyle(a model.AgentKind) lipgloss.Style {
	switch a {
	case model.AgentClaude:
		return styles.ChipClaude
	case model.AgentCodex:
		return styles.ChipCodex
	case model.AgentAider:
		return styles.ChipAider
	default:
		return styles.ChipGit
	}
}

// relativeTime renders a mock-style "Ns ago"/"Nm ago"/"Nh ago" duration.
func relativeTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
}

// clampWidth ellipsizes s to at most n display cells. Reuses diffview.go's
// clipWidth (lipgloss's rune/display-width-aware MaxWidth, already proven
// safe with CJK/wide runes by TestFitWidthNeverSplitsAMultiByteRuneOrProducesInvalidUTF8)
// rather than the previous byte-length clamp, which could slice a multibyte
// rune in half and corrupt the UTF-8 stream for any non-ASCII worktree name.
func clampWidth(s string, n int) string {
	if lipgloss.Width(s) <= n {
		return s
	}
	if n <= 1 {
		return clipWidth(s, n)
	}
	return clipWidth(s, n-1) + "…"
}
