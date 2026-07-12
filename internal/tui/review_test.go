package tui

import (
	"strings"
	"testing"

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

// ---- renderApproveFooter ----

func TestRenderApproveFooterShowsLeftCountWhenIncomplete(t *testing.T) {
	out := stripANSI(renderApproveFooter("main", 1, 3))
	if !strings.Contains(out, "Reviewed 1 / 3") {
		t.Errorf("footer = %q, want the Reviewed n/m label", out)
	}
	if !strings.Contains(out, "Approve & merge to main") {
		t.Errorf("footer = %q, want the approve affordance naming the base", out)
	}
	if !strings.Contains(out, "(2 left)") {
		t.Errorf("footer = %q, want \"(2 left)\" while incomplete", out)
	}
}

func TestRenderApproveFooterOmitsLeftCountWhenComplete(t *testing.T) {
	out := stripANSI(renderApproveFooter("main", 3, 3))
	if strings.Contains(out, "left") {
		t.Errorf("footer = %q, must not show a left-count once fully reviewed", out)
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
