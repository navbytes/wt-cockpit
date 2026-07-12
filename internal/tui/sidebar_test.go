package tui

import (
	"testing"
	"time"

	"github.com/navbytes/wt-cockpit/internal/model"
)

func wt(id, repo, name string, ago time.Duration) model.Worktree {
	return model.Worktree{ID: id, Repo: repo, Name: name, LastChange: time.Now().Add(-ago)}
}

// TestBuildRowsGroupsByRepoAlphabeticalAndSortsMostRecentFirstWithinRepo pins
// P3-design.md §1.1's sidebar order: "repos alphabetical, worktrees within a
// repo most-recent-first".
func TestBuildRowsGroupsByRepoAlphabeticalAndSortsMostRecentFirstWithinRepo(t *testing.T) {
	wts := []model.Worktree{
		wt("z1", "zeta", "old", 10*time.Minute),
		wt("a1", "alpha", "newer", 1*time.Minute),
		wt("z2", "zeta", "new", 1*time.Second),
		wt("a2", "alpha", "oldest", 20*time.Minute),
	}
	rows := buildRows(wts)

	var got []string
	for _, r := range rows {
		if r.header {
			got = append(got, "H:"+r.repo)
		} else {
			got = append(got, r.wt.ID)
		}
	}
	want := []string{"H:alpha", "a1", "a2", "H:zeta", "z2", "z1"}
	if len(got) != len(want) {
		t.Fatalf("rows = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

func TestSetWorktreesSelectsFirstRowByDefault(t *testing.T) {
	var s sidebar
	s.setWorktrees([]model.Worktree{wt("a1", "alpha", "one", time.Second), wt("a2", "alpha", "two", time.Minute)})
	if s.selectedID != "a1" {
		t.Errorf("selectedID = %q, want a1 (most recent, first row)", s.selectedID)
	}
	if !s.hasData {
		t.Error("hasData should be true after setWorktrees, even with a nonempty result")
	}
}

func TestSetWorktreesOnEmptySliceClearsSelectionButMarksHasData(t *testing.T) {
	var s sidebar
	s.setWorktrees(nil)
	if s.selectedID != "" {
		t.Errorf("selectedID = %q, want empty for an empty workspace", s.selectedID)
	}
	if !s.hasData {
		t.Error("hasData should be true — the fetch succeeded, it just found nothing")
	}
}

func TestMoveDownAndUpSkipHeaders(t *testing.T) {
	var s sidebar
	s.setWorktrees([]model.Worktree{
		wt("a1", "alpha", "one", time.Second),
		wt("z1", "zeta", "two", time.Second),
	})
	if s.selectedID != "a1" {
		t.Fatalf("precondition: selectedID = %q, want a1", s.selectedID)
	}
	s.moveDown()
	if s.selectedID != "z1" {
		t.Errorf("after moveDown, selectedID = %q, want z1 (header row skipped)", s.selectedID)
	}
	s.moveUp()
	if s.selectedID != "a1" {
		t.Errorf("after moveUp, selectedID = %q, want a1", s.selectedID)
	}
}

func TestMoveDownAtLastRowIsNoOp(t *testing.T) {
	var s sidebar
	s.setWorktrees([]model.Worktree{wt("a1", "alpha", "one", time.Second)})
	s.moveDown()
	if s.selectedID != "a1" {
		t.Errorf("selectedID = %q, want a1 unchanged (nothing past the last row)", s.selectedID)
	}
}

func TestMoveUpAtFirstRowIsNoOp(t *testing.T) {
	var s sidebar
	s.setWorktrees([]model.Worktree{wt("a1", "alpha", "one", time.Second)})
	s.moveUp()
	if s.selectedID != "a1" {
		t.Errorf("selectedID = %q, want a1 unchanged", s.selectedID)
	}
}

// TestSelectionFollowsIDAcrossUpsertReSort is P3-design.md §1.1's core
// promise: "Selection is keyed by worktree ID, not index, so live re-sorts
// and upserts never move the user's cursor."
func TestSelectionFollowsIDAcrossUpsertReSort(t *testing.T) {
	var s sidebar
	s.setWorktrees([]model.Worktree{
		wt("a1", "alpha", "one", 10*time.Minute),
		wt("a2", "alpha", "two", 1*time.Minute),
	})
	s.selectedID = "a1" // select the currently-older, second-row entry
	if idx := s.selectedIndex(); idx != 2 {
		t.Fatalf("precondition: a1 at row index %d, want 2 (header, a2, a1)", idx)
	}

	// a1 just changed — now the most recent — flipping the sort order.
	s.upsert(wt("a1", "alpha", "one", 0))

	if s.selectedID != "a1" {
		t.Fatalf("selectedID = %q, want a1 to remain selected across the re-sort", s.selectedID)
	}
	if idx := s.selectedIndex(); idx != 1 {
		t.Errorf("a1 should now be the first row (index 1, after the header) since it's most recent; got index %d", idx)
	}
}

func TestUpsertNewWorktreeAppearsWithoutMovingExistingSelection(t *testing.T) {
	var s sidebar
	s.setWorktrees([]model.Worktree{wt("a1", "alpha", "one", time.Minute)})
	s.upsert(wt("a2", "alpha", "two", time.Second)) // newer, would sort first

	if s.selectedID != "a1" {
		t.Errorf("selectedID = %q, want a1 unchanged by an upsert of a different worktree", s.selectedID)
	}
	if len(s.worktrees()) != 2 {
		t.Errorf("worktrees() = %d, want 2 after the upsert", len(s.worktrees()))
	}
}

// TestRemoveDropsRowAndAdvancesSelectionToNext pins the SSE
// worktree.removed application contract (P3-design.md §2.4): "drop row; if
// selected, select next".
func TestRemoveDropsRowAndAdvancesSelectionToNext(t *testing.T) {
	var s sidebar
	s.setWorktrees([]model.Worktree{
		wt("a1", "alpha", "one", time.Second),
		wt("a2", "alpha", "two", time.Minute),
	})
	s.remove("a1")

	if s.selectedID != "a2" {
		t.Errorf("selectedID = %q, want a2 (the next available row)", s.selectedID)
	}
	if _, ok := s.selected(); !ok {
		t.Error("selected() should still find a2")
	}
	for _, w := range s.worktrees() {
		if w.ID == "a1" {
			t.Error("a1 should have been removed from worktrees()")
		}
	}
}

func TestRemoveLastWorktreeLeavesEmptySelection(t *testing.T) {
	var s sidebar
	s.setWorktrees([]model.Worktree{wt("a1", "alpha", "one", time.Second)})
	s.remove("a1")

	if s.selectedID != "" {
		t.Errorf("selectedID = %q, want empty after removing the only worktree", s.selectedID)
	}
	if _, ok := s.selected(); ok {
		t.Error("selected() should report false for an empty workspace")
	}
}

func TestRemoveOfUnselectedWorktreeLeavesSelectionUntouched(t *testing.T) {
	var s sidebar
	s.setWorktrees([]model.Worktree{
		wt("a1", "alpha", "one", time.Second),
		wt("a2", "alpha", "two", time.Minute),
	})
	s.remove("a2")
	if s.selectedID != "a1" {
		t.Errorf("selectedID = %q, want a1 unchanged (a2 wasn't selected)", s.selectedID)
	}
}
