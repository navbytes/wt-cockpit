package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/navbytes/wt-cockpit/internal/model"
)

// ---- P2-5: topbar counter emphasis (style-call seam) ----

// TestUnreviewedCountStyleAccentsOnlyWhenPositive and
// TestAlertsCountStyleWarnsOnlyWhenPositive are the "style-call seam" half of
// P2-5's test requirement: comparing the *style* a count would render with,
// rather than parsing ANSI bytes, so the assertion holds regardless of the
// package's forced-Ascii test color profile (which renders no escape codes
// at all — see fake_test.go's TestMain).
func TestUnreviewedCountStyleAccentsOnlyWhenPositive(t *testing.T) {
	if unreviewedCountStyle(0).GetForeground() != styles.Txt.GetForeground() {
		t.Error("unreviewedCountStyle(0) should be the plain body-text color — nothing to draw attention to")
	}
	if unreviewedCountStyle(3).GetForeground() != styles.Accent.GetForeground() {
		t.Error("unreviewedCountStyle(3) should accent — there's something to review")
	}
}

func TestAlertsCountStyleWarnsOnlyWhenPositive(t *testing.T) {
	if alertsCountStyle(0).GetForeground() != styles.Dim.GetForeground() {
		t.Error("alertsCountStyle(0) should dim — nothing outstanding")
	}
	if alertsCountStyle(2).GetForeground() != styles.Warn.GetForeground() {
		t.Error("alertsCountStyle(2) should warn — a guardrail hit is outstanding")
	}
}

func TestRenderTopbarShowsAllFourCounts(t *testing.T) {
	wts := []model.Worktree{{State: model.StateActive, Reviewed: 0, Stats: model.Stats{Files: 1}}}
	out := stripANSI(renderTopbar(80, wts, connLive, 0, screenRadar))
	for _, want := range []string{"1 worktrees", "1 active", "1 unreviewed", "0 alerts", "● live", "◐ Radar"} {
		if !strings.Contains(out, want) {
			t.Errorf("topbar = %q, want it to contain %q", out, want)
		}
	}
}

// TestRenderTopbarRightAlignsChipAndViewIndicator pins the ux-expert
// P3-cheap layout: the conn chip + view indicator sit at the far right of
// the topbar, matching the mock, rather than immediately trailing the counts.
func TestRenderTopbarRightAlignsChipAndViewIndicator(t *testing.T) {
	out := stripANSI(renderTopbar(80, nil, connLive, 0, screenRadar))
	trimmed := strings.TrimRight(out, " ")
	if !strings.HasSuffix(trimmed, "Radar") {
		t.Errorf("topbar = %q, want the view indicator right-aligned at the line's end", out)
	}
	if lipgloss.Width(out) != 80 {
		t.Errorf("topbar width = %d, want exactly 80 (the requested width)", lipgloss.Width(out))
	}
}

// ---- P2-4: reconnect truthfulness ----

func TestConnChipShowsAttemptCountWhileReconnecting(t *testing.T) {
	out := stripANSI(connChip(connReconnecting, 4))
	if !strings.Contains(out, "reconnecting (4)") {
		t.Errorf("chip = %q, want the attempt count", out)
	}
}

func TestConnChipLiveShowsNoAttemptCount(t *testing.T) {
	out := stripANSI(connChip(connLive, 4))
	if strings.Contains(out, "4") {
		t.Errorf("chip = %q, a live connection must not show a stale attempt count", out)
	}
}

// TestRenderTopbarReconnectingShowsShowingLastKnownStateNote and
// TestRenderTopbarRestoresOnceLiveAgain are the two transitions ux-expert
// P2-4 asks for: into reconnecting (chip gains an attempt count, topbar
// gains the dim admission that the data on screen is stale) and back out of
// it (both disappear). Width 130 (this team's own standard capture width,
// per qa-run-3.md/manifest.md) gives the full counts+note+chip+view combo
// enough room to all render at once — see the 80-col degradation test below
// for the narrower case.
func TestRenderTopbarReconnectingShowsShowingLastKnownStateNote(t *testing.T) {
	out := stripANSI(renderTopbar(130, nil, connReconnecting, 2, screenRadar))
	if !strings.Contains(out, "showing last known state") {
		t.Errorf("topbar = %q, want the dim reconnecting note", out)
	}
	if !strings.Contains(out, "reconnecting (2)") {
		t.Errorf("topbar = %q, want the chip's attempt count", out)
	}
}

// TestRenderTopbarDropsNoteRatherThanTruncatingChipAtEightyCols is the
// narrow-terminal counterpart: at 80 cols, counts + the reconnecting note +
// chip + view indicator don't all fit. The connection chip (with its own
// attempt count) matters more than the note, so the note is the one dropped
// first — clipWidth's usual right-to-left truncation safety net (same as
// every other row/header in this package) takes it from there, and at this
// width even the view indicator can end up sacrificed before the chip does,
// since the chip sits to its left.
func TestRenderTopbarDropsNoteRatherThanTruncatingChipAtEightyCols(t *testing.T) {
	out := stripANSI(renderTopbar(80, nil, connReconnecting, 2, screenRadar))
	if strings.Contains(out, "showing last known state") {
		t.Errorf("topbar = %q, the reconnecting note must not survive at the cost of the chip", out)
	}
	if !strings.Contains(out, "reconnecting (2)") {
		t.Errorf("topbar = %q, want the chip + attempt count intact even when the note doesn't fit", out)
	}
	if lipgloss.Width(out) != 80 {
		t.Errorf("topbar width = %d, want exactly 80", lipgloss.Width(out))
	}
}

func TestRenderTopbarRestoresOnceLiveAgain(t *testing.T) {
	out := stripANSI(renderTopbar(80, nil, connLive, 0, screenRadar))
	if strings.Contains(out, "showing last known state") {
		t.Errorf("topbar = %q, must not show the reconnecting note once live", out)
	}
	if !strings.Contains(out, "● live") {
		t.Errorf("topbar = %q, want the live chip restored", out)
	}
}

// ---- renderKeybar: variant selection + toast styling ----

// TestRenderKeybarSelectsVariantByScreenAndFocus pins ux-expert P2-3's
// keybar-variant seam: Radar unfocused, Radar diff-focused, and Review each
// get a distinct keybar text.
func TestRenderKeybarSelectsVariantByScreenAndFocus(t *testing.T) {
	cases := []struct {
		name        string
		screen      screenID
		diffFocused bool
		want        string
	}{
		{"radar unfocused", screenRadar, false, "r review"},
		{"radar diff-focused", screenRadar, true, "o fold"},
		{"review", screenReview, false, "toggle"},
	}
	for _, c := range cases {
		out := renderKeybar(100, c.screen, c.diffFocused, "", false)
		if !strings.Contains(out, c.want) {
			t.Errorf("%s: keybar = %q, want it to contain %q", c.name, out, c.want)
		}
	}
}

func TestRenderKeybarToastReplacesKeybarText(t *testing.T) {
	out := renderKeybar(100, screenRadar, false, "some toast", false)
	if !strings.Contains(out, "some toast") {
		t.Errorf("keybar = %q, want the toast text", out)
	}
	if strings.Contains(out, "review") {
		t.Errorf("keybar = %q, a toast should replace the keybar text entirely", out)
	}
}

// TestRenderKeybarToastStyleDependsOnOK pins the ux-expert P3-cheap success
// toast: the same message renders differently styled depending on toastOK —
// Ok (success) vs. Warn (everything else). Forced TrueColor since the
// package's Ascii test profile renders no escape codes at all.
func TestRenderKeybarToastStyleDependsOnOK(t *testing.T) {
	saved := lipgloss.ColorProfile()
	defer lipgloss.SetColorProfile(saved)
	lipgloss.SetColorProfile(termenv.TrueColor)

	const msg = "same text either way"
	warn := renderKeybar(100, screenRadar, false, msg, false)
	ok := renderKeybar(100, screenRadar, false, msg, true)
	if warn == ok {
		t.Error("a success toast (toastOK=true) should render styled differently from a warn toast")
	}
}
