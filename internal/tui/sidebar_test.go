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

// ---- WP3: "/" search + "f" active-only filter ----

func TestMatchesFilterSubstringOnRepoNameBranchCaseInsensitive(t *testing.T) {
	w := model.Worktree{Repo: "API-Server", Name: "Auth-Refactor", Branch: "feature/AUTH-99"}
	for _, q := range []string{"api", "AUTH-refactor", "feature/auth", ""} {
		if !matchesFilter(w, q, false) {
			t.Errorf("matchesFilter(query=%q) = false, want true", q)
		}
	}
	if matchesFilter(w, "nope", false) {
		t.Error("matchesFilter(query=nope) = true, want false")
	}
}

func TestMatchesFilterActiveOnlyExcludesNonActive(t *testing.T) {
	active := model.Worktree{State: model.StateActive}
	idle := model.Worktree{State: model.StateIdle}
	if !matchesFilter(active, "", true) {
		t.Error("an active worktree must pass the active-only filter")
	}
	if matchesFilter(idle, "", true) {
		t.Error("an idle worktree must not pass the active-only filter")
	}
}

func TestFilteredReturnsRowsUnchangedWhenNoFilterActive(t *testing.T) {
	var s sidebar
	s.setWorktrees([]model.Worktree{wt("a1", "alpha", "one", time.Second)})
	got := s.filtered()
	if len(got) != len(s.rows) {
		t.Errorf("filtered() = %d rows, want %d (identical to rows with no filter)", len(got), len(s.rows))
	}
}

func TestFilteredDropsRepoHeaderWhenNoMemberMatches(t *testing.T) {
	var s sidebar
	s.setWorktrees([]model.Worktree{
		{ID: "a1", Repo: "alpha", Name: "one", State: model.StateActive},
		{ID: "z1", Repo: "zeta", Name: "two", State: model.StateIdle},
	})
	s.activeOnly = true
	got := s.filtered()

	var repos []string
	for _, r := range got {
		if r.header {
			repos = append(repos, r.repo)
		}
	}
	if len(repos) != 1 || repos[0] != "alpha" {
		t.Errorf("headers in filtered() = %v, want only alpha (zeta has no active member)", repos)
	}
}

func TestFilteredCombinesQueryAndActiveOnlyWithAnd(t *testing.T) {
	var s sidebar
	s.setWorktrees([]model.Worktree{
		{ID: "a1", Repo: "alpha", Name: "one", State: model.StateActive},
		{ID: "a2", Repo: "alpha", Name: "two", State: model.StateIdle},
	})
	s.filterInput.SetValue("one")
	s.activeOnly = true

	var ids []string
	for _, r := range s.filtered() {
		if !r.header {
			ids = append(ids, r.wt.ID)
		}
	}
	if len(ids) != 1 || ids[0] != "a1" {
		t.Errorf("filtered ids = %v, want exactly [a1] (matches query AND active)", ids)
	}
}

func TestSearchLifecycleStartTypeCommit(t *testing.T) {
	var s sidebar
	s.setWorktrees([]model.Worktree{
		{ID: "a1", Repo: "alpha", Name: "one"},
		{ID: "z1", Repo: "zeta", Name: "two"},
	})
	if s.searching() {
		t.Fatal("precondition: not searching yet")
	}
	s.startSearch()
	if !s.searching() {
		t.Fatal("startSearch() should focus the input")
	}
	s.filterInput.SetValue("zeta") // simulate typed input landing (Update() itself is bubbles' own machinery)
	s.fixSelection()
	if _, ok := s.selected(); !ok {
		t.Fatal("a selection should exist among the filtered rows")
	}
	if sel, _ := s.selected(); sel.ID != "z1" {
		t.Errorf("selected = %q, want z1 once the query narrows to zeta", sel.ID)
	}

	s.commitSearch()
	if s.searching() {
		t.Error("commitSearch() should blur the input")
	}
	if s.filterInput.Value() != "zeta" {
		t.Errorf("filterInput.Value() = %q, want the query kept after commit", s.filterInput.Value())
	}
}

func TestSearchCancelClearsQueryAndSelectionReturnsToFullSet(t *testing.T) {
	var s sidebar
	s.setWorktrees([]model.Worktree{
		{ID: "a1", Repo: "alpha", Name: "one"},
		{ID: "z1", Repo: "zeta", Name: "two"},
	})
	s.startSearch()
	s.filterInput.SetValue("zeta")
	s.fixSelection()

	s.cancelSearch()
	if s.searching() {
		t.Error("cancelSearch() should blur the input")
	}
	if s.filterInput.Value() != "" {
		t.Errorf("filterInput.Value() = %q, want cleared after cancel", s.filterInput.Value())
	}
	if len(s.filtered()) != len(s.rows) {
		t.Error("clearing the search should restore every row")
	}
}

func TestHasFilterAndClearFilter(t *testing.T) {
	var s sidebar
	s.setWorktrees([]model.Worktree{wt("a1", "alpha", "one", time.Second)})
	if s.hasFilter() {
		t.Fatal("precondition: no filter active yet")
	}
	s.activeOnly = true
	if !s.hasFilter() {
		t.Error("hasFilter() should be true once active-only is set")
	}
	s.clearFilter()
	if s.hasFilter() || s.activeOnly || s.filterInput.Value() != "" {
		t.Errorf("clearFilter() left state = query %q activeOnly %v, want both cleared", s.filterInput.Value(), s.activeOnly)
	}
}

// TestFixSelectionReselectsWithinFilteredSetWhenCurrentSelectionFilteredOut
// pins the generalisation of fixSelection to the *visible* set: a
// worktree.upserted-style event that quietly falls outside the active
// filter shouldn't leave the selection dangling on a hidden row.
func TestFixSelectionReselectsWithinFilteredSetWhenCurrentSelectionFilteredOut(t *testing.T) {
	var s sidebar
	s.setWorktrees([]model.Worktree{
		{ID: "a1", Repo: "alpha", Name: "one", State: model.StateActive},
		{ID: "a2", Repo: "alpha", Name: "two", State: model.StateIdle},
	})
	s.selectedID = "a2"
	s.activeOnly = true // a2 (idle) no longer matches

	s.fixSelection()
	if s.selectedID != "a1" {
		t.Errorf("selectedID = %q, want a1 (the only visible row) once a2 is filtered out", s.selectedID)
	}
}

func TestMoveUpDownOperateOverFilteredRows(t *testing.T) {
	var s sidebar
	s.setWorktrees([]model.Worktree{
		{ID: "a1", Repo: "alpha", Name: "one", State: model.StateActive, LastChange: fixedTime(3)},
		{ID: "a2", Repo: "alpha", Name: "two", State: model.StateIdle, LastChange: fixedTime(2)},
		{ID: "a3", Repo: "alpha", Name: "three", State: model.StateActive, LastChange: fixedTime(1)},
	})
	s.activeOnly = true // hides a2

	if s.selectedID != "a3" {
		t.Fatalf("precondition: selectedID = %q, want a3 (most recent active)", s.selectedID)
	}
	s.moveDown()
	if s.selectedID != "a1" {
		t.Errorf("moveDown skipped the filtered-out a2: selectedID = %q, want a1", s.selectedID)
	}
	s.moveUp()
	if s.selectedID != "a3" {
		t.Errorf("selectedID = %q, want back to a3", s.selectedID)
	}
}
