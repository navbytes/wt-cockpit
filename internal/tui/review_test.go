package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"github.com/navbytes/wt-cockpit/internal/model"
)

// ---- reviewProgress ----

func TestReviewProgressCountsReviewedFilesAgainstTotal(t *testing.T) {
	d := model.Diff{
		Files:    []model.DiffFile{{Path: "a.go"}, {Path: "b.go"}, {Path: "c.go"}},
		Reviewed: map[string]bool{"a.go": true, "c.go": true},
	}
	reviewed, total := reviewProgress(d)
	if reviewed != 2 || total != 3 {
		t.Errorf("reviewProgress() = %d/%d, want 2/3", reviewed, total)
	}
}

func TestReviewProgressEmptyDiffIsZeroOverZero(t *testing.T) {
	reviewed, total := reviewProgress(model.Diff{})
	if reviewed != 0 || total != 0 {
		t.Errorf("reviewProgress() = %d/%d, want 0/0", reviewed, total)
	}
}

// TestReviewProgressReflectsOptimisticFlipImmediately pins that the rail's
// numbers are computed from the loaded diff (which the space-toggle
// mutates directly), not from Worktree.Reviewed's SSE-lagged aggregate.
func TestReviewProgressReflectsOptimisticFlipImmediately(t *testing.T) {
	d := model.Diff{Files: []model.DiffFile{{Path: "a.go"}, {Path: "b.go"}}}
	reviewed, total := reviewProgress(d)
	if reviewed != 0 || total != 2 {
		t.Fatalf("precondition: want 0/2, got %d/%d", reviewed, total)
	}
	d.Reviewed = map[string]bool{"a.go": true}
	reviewed, total = reviewProgress(d)
	if reviewed != 1 || total != 2 {
		t.Errorf("reviewProgress() after marking a.go = %d/%d, want 1/2", reviewed, total)
	}
}

// ---- renderRailRow ----

// TestRenderRailRowSelectedNeverExceedsRailWidth is the review rail's version
// of ux-expert P1-1's sidebar fix (see sidebar.go's renderSidebarRow/packRow):
// styles.SelectedRow's left border is a real column added by lipgloss's
// Border rendering, but the old renderRailRow clipped its content to the
// *full* width first and only then wrapped it in styles.SelectedRow — so a
// selected row whose content already reached the width budget rendered one
// column wider than an unselected row, past the rail's fixed 28 cols. Every
// case must hold the budget regardless of a long file path, huge stat
// counts, or the reviewed "[x]" badge, selected or not.
func TestRenderRailRowSelectedNeverExceedsRailWidth(t *testing.T) {
	cases := []struct {
		name     string
		f        model.DiffFile
		reviewed bool
	}{
		{
			name: "long path + huge stats, unreviewed",
			f: model.DiffFile{
				Path:  "internal/some/very/deeply/nested/package/with/a/rather/long/file/name.go",
				Stats: model.Stats{Add: 999999, Del: 999999},
			},
		},
		{
			name: "long path, reviewed ([x] badge)",
			f: model.DiffFile{
				Path:  "internal/some/very/deeply/nested/package/with/a/rather/long/file/name.go",
				Stats: model.Stats{Add: 1, Del: 1},
			},
			reviewed: true,
		},
		{name: "zero value", f: model.DiffFile{}},
		{name: "short ordinary row", f: model.DiffFile{Path: "a.go", Stats: model.Stats{Add: 1}}},
	}
	for _, c := range cases {
		for _, current := range []bool{false, true} {
			out := renderRailRow(railWidth, c.f, c.reviewed, current)
			lines := strings.Split(out, "\n")
			if len(lines) != 1 {
				t.Errorf("%s current=%v: %d lines, want exactly 1", c.name, current, len(lines))
				continue
			}
			if got := lipgloss.Width(lines[0]); got > railWidth {
				t.Errorf("%s current=%v: width = %d, want <= %d (railWidth)", c.name, current, got, railWidth)
			}
		}
	}
}

// ---- renderApproveFooter ----

// TestRenderApproveFooterShowsLeftCountWhenIncomplete pins the ux-expert
// P2-7 shortened copy ("✓ approve — N left"), not the old "✓ Approve & merge
// to <base> (N left)" — chosen because it's short enough to actually fit the
// rail (see the width-clip property test below) rather than relying on
// clipWidth to truncate it mid-word.
func TestRenderApproveFooterShowsLeftCountWhenIncomplete(t *testing.T) {
	out := stripANSI(renderApproveFooter(railWidth, "main", 1, 3))
	if !strings.Contains(out, "Reviewed 1 / 3") {
		t.Errorf("footer = %q, want the Reviewed n/m label", out)
	}
	if !strings.Contains(out, "approve — 2 left") {
		t.Errorf("footer = %q, want the shortened \"approve — 2 left\" copy", out)
	}
}

func TestRenderApproveFooterOmitsLeftCountWhenComplete(t *testing.T) {
	out := stripANSI(renderApproveFooter(railWidth, "main", 3, 3))
	if strings.Contains(out, "left") {
		t.Errorf("footer = %q, must not show a left-count once fully reviewed", out)
	}
	if !strings.Contains(out, "approve → main") {
		t.Errorf("footer = %q, want the complete-state copy naming the base", out)
	}
}

// TestRenderApproveFooterNeverExceedsRailWidth is P2-7's property test: like
// renderRailRow, every line must clip to width rather than wrap. Two cases
// stress the two lines that carry unbounded content: a huge unreviewed count
// (the incomplete-state line) and a long base branch name on a complete diff
// (the "approve → <base>" line, which the incomplete case never renders).
func TestRenderApproveFooterNeverExceedsRailWidth(t *testing.T) {
	cases := []struct {
		name            string
		base            string
		reviewed, total int
	}{
		{"huge unreviewed count", "main", 0, 999999},
		{"long base name, complete", "release/a-rather-long-branch-name-indeed", 5, 5},
	}
	for _, c := range cases {
		out := renderApproveFooter(railWidth, c.base, c.reviewed, c.total)
		lines := strings.Split(out, "\n")
		if len(lines) != 3 {
			t.Fatalf("%s: renderApproveFooter produced %d lines, want exactly 3 (label, bar, approve)", c.name, len(lines))
		}
		for i, ln := range lines {
			if w := lipgloss.Width(ln); w > railWidth {
				t.Errorf("%s: line %d width = %d, want <= %d (railWidth): %q", c.name, i, w, railWidth, ln)
			}
		}
	}
}

// ---- renderRail / rail rendering via reviewView.view ----

func reviewFixtureDiff() model.Diff {
	return model.Diff{WorktreeID: "w1", Files: []model.DiffFile{
		{Path: "a.go", Hash: "ha", Stats: model.Stats{Add: 1}},
		{Path: "b.go", Hash: "hb", Stats: model.Stats{Add: 2, Del: 1}},
	}}
}

func TestRenderRailShowsLoadingBeforeDiffArrives(t *testing.T) {
	r := newRadarView()
	w := model.Worktree{ID: "w1", Repo: "api", Name: "feature"}
	out := stripANSI(renderRail(railWidth, 20, w, &r))
	if !strings.Contains(out, "loading") {
		t.Errorf("rail = %q, want a loading placeholder before the diff has loaded", out)
	}
}

// TestRenderRailShowsCheckedAndUncheckedFiles pins the "[x] name +a -d, done
// = strikethrough+faint" checklist contract.
func TestRenderRailShowsCheckedAndUncheckedFiles(t *testing.T) {
	r := newRadarView()
	r.currentID = "w1"
	d := reviewFixtureDiff()
	d.Reviewed = map[string]bool{"a.go": true}
	r.pane.setDiff(d)
	w := model.Worktree{ID: "w1", Repo: "api", Name: "feature"}

	out := stripANSI(renderRail(railWidth, 20, w, &r))
	if !strings.Contains(out, "[x]") {
		t.Errorf("rail = %q, want a [x] row for the reviewed file", out)
	}
	if !strings.Contains(out, "[ ]") {
		t.Errorf("rail = %q, want a [ ] row for the unreviewed file", out)
	}
	if !strings.Contains(out, "a.go") || !strings.Contains(out, "b.go") {
		t.Errorf("rail = %q, want both file names", out)
	}
	if !strings.Contains(out, "Reviewed 1 / 2") {
		t.Errorf("rail = %q, want the progress label", out)
	}
}

// TestReviewViewComposesDiffPaneAndRail confirms the Review screen actually
// renders the shared diff (not a stub) alongside the rail.
func TestReviewViewComposesDiffPaneAndRail(t *testing.T) {
	r := newRadarView()
	r.currentID = "w1"
	r.pane.setDiff(reviewFixtureDiff())
	w := model.Worktree{ID: "w1", Repo: "api", Name: "feature", Base: "main"}

	var rv reviewView
	out := stripANSI(rv.view(100, 20, w, &r))
	if !strings.Contains(out, "a.go") {
		t.Errorf("review view = %q, want the diff pane's file card", out)
	}
	if !strings.Contains(out, "FILES IN THIS WORKTREE") {
		t.Errorf("review view = %q, want the rail heading", out)
	}
}
