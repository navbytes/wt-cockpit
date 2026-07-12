package tui

import (
	"fmt"

	"github.com/navbytes/wt-cockpit/internal/model"
)

// renderTopbar builds the persistent top summary line (P3-design.md §1.1):
// brand, live counters (worktrees/active/unreviewed/alerts), a connection
// chip, and the Radar/Review indicator. The mock's "roots · N projects"
// summary is omitted in WP1 — the API doesn't surface configured roots yet
// — which is exactly the mock's own documented fallback: "if unavailable,
// just brand".
func renderTopbar(width int, wts []model.Worktree, conn connState, screen screenID) string {
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
	brand := styles.Brand.Render("wt cockpit")
	counts := styles.Dim.Render(fmt.Sprintf("%d worktrees · %d active · %d unreviewed · %d alerts",
		len(wts), active, unreviewed, alerts))
	line := fmt.Sprintf("%s  %s  %s  %s", brand, counts, connChip(conn), viewIndicator(screen))
	return styles.Topbar.Width(width).Render(line)
}

// connChip is the topbar's connection indicator (P3-design.md §1.1/§1.4).
func connChip(s connState) string {
	switch s {
	case connLive:
		return styles.Add.Render("● live")
	case connConnecting:
		return styles.Dim.Render("◌ connecting")
	case connReconnecting:
		return styles.Warn.Render("↻ reconnecting")
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
func renderKeybar(width int, screen screenID, toast string) string {
	if toast != "" {
		return styles.Warn.Width(width).Render(toast)
	}
	text := radarKeybar
	if screen == screenReview {
		text = reviewKeybar
	}
	return styles.Keybar.Width(width).Render(text)
}
