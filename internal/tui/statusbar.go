package tui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/navbytes/wt-cockpit/internal/model"
)

// renderTopbar builds the persistent top summary line (P3-design.md §1.1):
// brand, live counters (worktrees/active/unreviewed/alerts), a connection
// chip, and the Radar/Review indicator. The mock's "roots · N projects"
// summary is omitted in WP1 — the API doesn't surface configured roots yet
// — which is exactly the mock's own documented fallback: "if unavailable,
// just brand".
//
// ux-expert polish: counts render bright/bold with the unreviewed/alerts
// numbers additionally accented/warned once they're actually nonzero
// (P2-5); the chip + view indicator are right-aligned to width, matching the
// mock's layout (P3-cheap); while reconnecting, a dim note admits the data on
// screen is stale rather than silently implying it's current (P2-4).
func renderTopbar(width int, wts []model.Worktree, conn connState, attempt int, screen screenID) string {
	var active, unreviewed, alerts int
	for _, w := range wts {
		if w.State == model.StateActive {
			active++
		}
		if w.Reviewed < w.Stats.Files {
			unreviewed++
		}
		if len(w.Guardrails) > 0 {
			alerts++
		}
	}
	left := styles.Brand.Render("wt cockpit") + "  " + renderTopbarCounts(len(wts), active, unreviewed, alerts)
	right := connChip(conn, attempt)

	// The view indicator and (while reconnecting) the "showing last known
	// state" note are both optional trailing polish, added only once they
	// demonstrably still fit — dropped whole at a tight width rather than
	// left to clipWidth's usual truncation, which would cut either mid-word
	// ("Radar" -> "Rad"). The connection chip is what actually matters most:
	// it's never a truncation candidate by construction here.
	if withView := right + "  " + viewIndicator(screen); lipgloss.Width(left)+1+lipgloss.Width(withView) <= width {
		right = withView
	}
	if conn == connReconnecting {
		if withNote := left + "  " + styles.Dim.Render("showing last known state"); lipgloss.Width(withNote)+1+lipgloss.Width(right) <= width {
			left = withNote
		}
	}

	gap := width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	line := left + strings.Repeat(" ", gap) + right
	return styles.Topbar.Width(width).Render(clipWidth(line, width))
}

// renderTopbarCounts builds the "N worktrees · N active · N unreviewed · N
// alerts" segment with each number bright/bold and the unreviewed/alerts
// numbers additionally styled by whether they're worth the user's attention
// right now (ux-expert P2-5) — labels stay dim throughout.
func renderTopbarCounts(total, active, unreviewed, alerts int) string {
	sep := styles.Dim.Render(" · ")
	parts := []string{
		countStat(total, styles.Txt) + styles.Dim.Render(" worktrees"),
		countStat(active, styles.Txt) + styles.Dim.Render(" active"),
		countStat(unreviewed, unreviewedCountStyle(unreviewed)) + styles.Dim.Render(" unreviewed"),
		countStat(alerts, alertsCountStyle(alerts)) + styles.Dim.Render(" alerts"),
	}
	return strings.Join(parts, sep)
}

// countStat renders one topbar number in bold against st's color.
func countStat(n int, st lipgloss.Style) string {
	return st.Bold(true).Render(strconv.Itoa(n))
}

// unreviewedCountStyle accents the unreviewed count once there's actually
// something to review — zero is a quiet, unremarkable state.
func unreviewedCountStyle(n int) lipgloss.Style {
	if n > 0 {
		return styles.Accent
	}
	return styles.Txt
}

// alertsCountStyle warns on the alerts count once there's actually a
// guardrail hit outstanding; zero fades to dim rather than drawing an eye.
func alertsCountStyle(n int) lipgloss.Style {
	if n > 0 {
		return styles.Warn
	}
	return styles.Dim
}

// connChip is the topbar's connection indicator (P3-design.md §1.1/§1.4).
// attempt (only meaningful while reconnecting) makes the reconnect visibly
// truthful about how long the daemon has been unreachable (ux-expert P2-4),
// matching the design doc's own "↻ reconnecting (n)" wording.
func connChip(s connState, attempt int) string {
	switch s {
	case connLive:
		return styles.Add.Render("● live")
	case connConnecting:
		return styles.Dim.Render("◌ connecting")
	case connReconnecting:
		return styles.Warn.Render(fmt.Sprintf("↻ reconnecting (%d)", attempt))
	default: // connDown
		return styles.Del.Render("✕ down")
	}
}

func viewIndicator(s screenID) string {
	if s == screenReview {
		return styles.Accent.Render("▤ Review")
	}
	return styles.Accent.Render("◐ Radar")
}

// renderKeybar renders the bottom context-sensitive keybar (P3-design.md
// §1.1/§1.3), or a transient toast in its place (§1.5: one at a time,
// latest wins, displayed for 4s — the model clears it on toastExpiredMsg).
// diffFocused selects Radar's diff-focus keybar variant (ux-expert P2-3)
// when the diff pane — not the sidebar — has the keyboard; toastOK renders
// a successful toast (e.g. a completed approve) in a success style rather
// than the warn style every other toast uses (ux-expert P3-cheap).
func renderKeybar(width int, screen screenID, diffFocused bool, toast string, toastOK bool) string {
	if toast != "" {
		st := styles.Warn
		if toastOK {
			st = styles.Ok
		}
		return st.Width(width).Render(toast)
	}
	text := radarKeybar
	switch {
	case screen == screenReview:
		text = reviewKeybar
	case diffFocused:
		text = diffKeybar
	}
	return styles.Keybar.Width(width).Render(text)
}
