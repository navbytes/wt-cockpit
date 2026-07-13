package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/navbytes/wt-cockpit/internal/model"
)

// railWidth is the Review screen's right rail (P3-design.md §1.2: "rail at
// 28").
const railWidth = 28

// reviewView composes the Review screen's main pane (P3-design.md §1.1):
// the same unified diff Radar shows, plus the right rail (file checklist,
// progress, approve hint). It owns no diff/scroll state of its own — Review
// borrows radarView's shared diffview/highlightCache/diffLRU (P3-design.md
// §2.3: "two lightweight view structs that borrow the shared diffview...
// components"), so switching between `r` and `esc` never re-fetches and
// never loses scroll position.
type reviewView struct{}

// view renders the Review screen's main pane for worktree w. The shared diff
// pane always renders unfocused (Review has no separate diff-focus state to
// cue — see radarView.view's own comment) and always in "reviewing" mode, so
// its header reads "reviewing <branch> vs <base>" rather than Radar's plain
// "base <base>" (ux-expert P3-cheap).
func (reviewView) view(width, height int, w model.Worktree, radar *radarView) string {
	diffWidth := width - railWidth
	if diffWidth < 0 {
		diffWidth = 0
	}
	diffPane := radar.view(diffWidth, height, w, false, true)
	rail := renderRail(railWidth, height, w, radar)
	return lipgloss.JoinHorizontal(lipgloss.Top, diffPane, rail)
}

// reviewProgress counts how many of d's files are currently marked
// reviewed (per Diff.Reviewed, the WP3 per-file map) against the total.
// Computed directly from the loaded diff rather than Worktree.Reviewed's
// aggregate count, so an optimistic space-toggle updates the rail's own
// numbers instantly, with no dependency on the review.changed SSE round trip.
func reviewProgress(d model.Diff) (reviewed, total int) {
	total = len(d.Files)
	for _, f := range d.Files {
		if d.Reviewed[f.Path] {
			reviewed++
		}
	}
	return reviewed, total
}

// renderRail is the mock's "Files in this worktree" checklist + progress +
// approve footer (P3-design.md §1.1).
func renderRail(width, height int, w model.Worktree, radar *radarView) string {
	var b strings.Builder
	fmt.Fprintln(&b, styles.Faint.Render("FILES IN THIS WORKTREE"))

	loaded := radar.pane.diff.WorktreeID == w.ID && radar.diffErr == ""
	if !loaded {
		fmt.Fprint(&b, styles.Dim.Render("  loading…"))
		return lipgloss.NewStyle().Width(width).Height(height).Render(b.String())
	}

	d := radar.pane.diff
	cur := radar.pane.currentFileIndex()
	for i, f := range d.Files {
		fmt.Fprintln(&b)
		fmt.Fprint(&b, renderRailRow(width, f, d.Reviewed[f.Path], i == cur))
	}

	reviewed, total := reviewProgress(d)
	fmt.Fprintln(&b)
	fmt.Fprintln(&b)
	fmt.Fprint(&b, renderApproveFooter(width, w.Base, reviewed, total))

	return lipgloss.NewStyle().Width(width).Height(height).Render(b.String())
}

// renderRailRow is one checklist line: "[x] name +a -d", strikethrough+faint
// once done (P3-design.md §1.1), highlighted when it's the file under the
// diff pane's cursor (kept in lockstep with `[`/`]`/j/k's currentFileIndex).
//
// styles.SelectedRow's BorderLeft(true) is a real column lipgloss adds on top
// of whatever it's given — the same border-width-overflow class ux-expert
// P1-1 fixed in the sidebar (renderSidebarRow/packRow). Content is clipped to
// contentWidth (width minus that one reserved border column when current)
// *before* SelectedRow ever wraps it, so the border lands inside the budget
// instead of pushing the rendered row one column past the rail's fixed width.
func renderRailRow(width int, f model.DiffFile, reviewed, current bool) string {
	contentWidth := width
	if current {
		contentWidth-- // styles.SelectedRow's left border owns one column of its own
	}
	if contentWidth < 0 {
		contentWidth = 0
	}

	box := "[ ]"
	nameBudget := contentWidth - 12
	if nameBudget < 4 {
		nameBudget = 4
	}
	name := clampWidth(f.Path, nameBudget)
	nameStyle := styles.Txt
	if reviewed {
		box = "[x]"
		nameStyle = styles.Faint.Strikethrough(true)
	}
	stats := styles.Add.Render(fmt.Sprintf("+%d", f.Stats.Add)) + " " + styles.Del.Render(fmt.Sprintf("-%d", f.Stats.Del))
	row := clipWidth(fmt.Sprintf(" %s %s %s", box, nameStyle.Render(name), stats), contentWidth)
	if current {
		return styles.SelectedRow.Render(row)
	}
	return row
}

// renderApproveFooter is the mock's progress bar + approve affordance
// (P3-design.md §1.1). Approve itself is driven by the `a` key elsewhere;
// this is a status indicator, not a widget. Every line is clipped to width
// like renderRailRow's own rows (ux-expert P2-7) — the rail is a fixed
// 28-col column, so a long base branch name must never wrap it — and the
// incomplete-state copy is shortened ("✓ approve — N left") so it actually
// fits there instead of running past the column the way "✓ Approve & merge
// to <base> (N left)" could.
func renderApproveFooter(width int, base string, reviewed, total int) string {
	label := fmt.Sprintf("Reviewed %d / %d", reviewed, total)
	bar := progressBar(reviewed, total, 16)
	approve := fmt.Sprintf("✓ approve → %s", base)
	if reviewed < total {
		approve = fmt.Sprintf("✓ approve — %d left", total-reviewed)
	}
	lines := []string{clipWidth(label, width), clipWidth(bar, width), clipWidth(styles.Accent.Render(approve), width)}
	return strings.Join(lines, "\n")
}

// progressBar renders a done/total bar as filled/empty block characters —
// a plain ASCII-safe rendering (no lipgloss.Progress bubble; the design's
// dependency list doesn't name one, and this is a one-line format).
func progressBar(done, total, width int) string {
	if total <= 0 {
		return styles.Faint.Render(strings.Repeat("░", width))
	}
	filled := width * done / total
	if filled > width {
		filled = width
	}
	return styles.Add.Render(strings.Repeat("█", filled)) + styles.Faint.Render(strings.Repeat("░", width-filled))
}
