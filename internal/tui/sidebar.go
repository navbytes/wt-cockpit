package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

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

// fixSelection keeps selectedID pointing at a real row when possible: if it
// still exists after a rebuild, leave it; otherwise pick the first
// selectable row, or "" if there are none (empty workspace).
func (s *sidebar) fixSelection() {
	if s.selectedID != "" {
		for _, r := range s.rows {
			if !r.header && r.wt.ID == s.selectedID {
				return
			}
		}
	}
	for _, r := range s.rows {
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
	idx := s.selectedIndex()
	if idx < 0 {
		return
	}
	for i := idx + delta; i >= 0 && i < len(s.rows); i += delta {
		if !s.rows[i].header {
			s.selectedID = s.rows[i].wt.ID
			return
		}
	}
}

func (s *sidebar) selectedIndex() int {
	for i, r := range s.rows {
		if !r.header && r.wt.ID == s.selectedID {
			return i
		}
	}
	return -1
}

// selected returns the currently selected worktree, or false if there is
// none (empty workspace).
func (s *sidebar) selected() (model.Worktree, bool) {
	for _, r := range s.rows {
		if !r.header && r.wt.ID == s.selectedID {
			return r.wt, true
		}
	}
	return model.Worktree{}, false
}

// view renders the sidebar into a width-constrained block. WP1: plain rows
// (state dot, name, agent chip, relative time, +add -del); the mock's ⚠
// guardrail badge coloring by worst severity, and virtualized scrolling for
// more rows than height, are WP2/WP3 polish once the diff pane makes a
// taller sidebar worth scrolling.
func (s *sidebar) view(width, height int) string {
	var b strings.Builder
	fmt.Fprintln(&b, styles.Faint.Render("WORKTREES · LIVE"))

	switch {
	case !s.hasData:
		fmt.Fprint(&b, styles.Dim.Render("  connecting to wtd…"))
	case len(s.rows) == 0:
		fmt.Fprint(&b, styles.Dim.Render("  no worktrees — wtd -root <dir>"))
	default:
		for i, r := range s.rows {
			if i > 0 {
				fmt.Fprintln(&b)
			}
			if r.header {
				fmt.Fprint(&b, styles.RepoHeader.Render(r.repo))
				continue
			}
			fmt.Fprint(&b, renderSidebarRow(r.wt, r.wt.ID == s.selectedID))
		}
	}
	return lipgloss.NewStyle().Width(width).Height(height).Render(b.String())
}

func renderSidebarRow(w model.Worktree, selected bool) string {
	dot := stateDotStyle(w.State).Render("●")
	name := styles.Txt.Render(clampWidth(w.Name, 24))
	agent := agentChipStyle(w.Agent).Render(string(w.Agent))
	age := styles.Dim.Render(relativeTime(w.LastChange))
	stats := styles.Add.Render(fmt.Sprintf("+%d", w.Stats.Add)) + " " + styles.Del.Render(fmt.Sprintf("-%d", w.Stats.Del))
	badge := ""
	if len(w.Guardrails) > 0 {
		badge = " " + styles.Warn.Render("⚠")
	}
	row := fmt.Sprintf(" %s %s  %s  %s  %s%s", dot, name, agent, age, stats, badge)
	if selected {
		return styles.SelectedRow.Render(row)
	}
	return row
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

// clampWidth is a byte-length clamp (ellipsize), matching cmd/wt's existing
// truncate helper. Rune/wide-char-aware truncation (P3-design.md §1.2) is a
// WP2/WP3 polish item once real narrow-terminal layouts are being tuned.
func clampWidth(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}
