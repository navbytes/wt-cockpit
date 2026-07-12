package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/navbytes/wt-cockpit/internal/model"
)

// ---- diffLRU ----

func TestDiffLRUMissThenHitAfterPut(t *testing.T) {
	d := newDiffLRU(4)
	if _, ok := d.get("a"); ok {
		t.Fatal("get on an empty LRU should miss")
	}
	d.put("a", model.Diff{WorktreeID: "a"})
	got, ok := d.get("a")
	if !ok || got.WorktreeID != "a" {
		t.Errorf("get after put = %+v, ok=%v, want WorktreeID a", got, ok)
	}
}

// TestDiffLRUEvictsLeastRecentlyUsedPastCap pins "last 4 fetched model.Diff"
// (P3-design.md §2.5).
func TestDiffLRUEvictsLeastRecentlyUsedPastCap(t *testing.T) {
	d := newDiffLRU(2)
	d.put("a", model.Diff{WorktreeID: "a"})
	d.put("b", model.Diff{WorktreeID: "b"})
	d.put("c", model.Diff{WorktreeID: "c"}) // over cap: evicts "a", the LRU
	if _, ok := d.get("a"); ok {
		t.Error("a should have been evicted")
	}
	if _, ok := d.get("b"); !ok {
		t.Error("b should have survived")
	}
	if _, ok := d.get("c"); !ok {
		t.Error("c should have survived")
	}
}

func TestDiffLRUPutOnExistingIDUpdatesAndPromotes(t *testing.T) {
	d := newDiffLRU(2)
	d.put("a", model.Diff{WorktreeID: "a", Hash: "h1"})
	d.put("b", model.Diff{WorktreeID: "b"})
	d.put("a", model.Diff{WorktreeID: "a", Hash: "h2"}) // re-put: promotes a, b is now LRU
	d.put("c", model.Diff{WorktreeID: "c"})             // evicts b, not a
	if _, ok := d.get("b"); ok {
		t.Error("b should have been evicted as the least-recently-used entry")
	}
	got, ok := d.get("a")
	if !ok || got.Hash != "h2" {
		t.Errorf("get(a) = %+v ok=%v, want the updated Hash h2", got, ok)
	}
}

// TestNewRadarViewDiffLRUCapMatchesDesignBudget pins the *production* diffLRU
// cap against P3-design.md §6's budget-table row ("plus max 4 cached
// model.Diffs") — the eviction tests above deliberately construct a
// newDiffLRU with a small synthetic cap directly, so nothing else asserts
// the real constant newRadarView() wires up hasn't drifted.
func TestNewRadarViewDiffLRUCapMatchesDesignBudget(t *testing.T) {
	r := newRadarView()
	if r.diffs.cap != 4 {
		t.Errorf("newRadarView().diffs.cap = %d, want 4 (P3-design.md §6)", r.diffs.cap)
	}
}

// ---- radarView.ensureDiff ----

func TestEnsureDiffDispatchesFetchOnCacheMiss(t *testing.T) {
	r := newRadarView()
	api := &fakeAPI{diff: model.Diff{WorktreeID: "w1", Hash: "h1"}}
	cmd := r.ensureDiff(context.Background(), api, "w1")
	if cmd == nil {
		t.Fatal("expected a fetch command on a cache miss")
	}
	msg := cmd()
	dm, ok := msg.(diffMsg)
	if !ok || dm.ID != "w1" {
		t.Fatalf("cmd() = %#v, want a diffMsg for w1", msg)
	}
}

func TestEnsureDiffSwapsInstantlyFromCacheWithoutACommand(t *testing.T) {
	r := newRadarView()
	r.diffs.put("w1", model.Diff{WorktreeID: "w1", Hash: "cached"})
	cmd := r.ensureDiff(context.Background(), &fakeAPI{}, "w1")
	if cmd != nil {
		t.Error("a cache hit should swap the pane in with no fetch command")
	}
	if r.pane.diff.WorktreeID != "w1" {
		t.Errorf("pane.diff.WorktreeID = %q, want w1 to be showing already", r.pane.diff.WorktreeID)
	}
}

func TestEnsureDiffNoOpWhenAlreadyCurrentAndNoError(t *testing.T) {
	r := newRadarView()
	api := &fakeAPI{diff: model.Diff{WorktreeID: "w1"}}
	first := r.ensureDiff(context.Background(), api, "w1")
	if first == nil {
		t.Fatal("first call should fetch")
	}
	second := r.ensureDiff(context.Background(), api, "w1")
	if second != nil {
		t.Error("re-checking the same, already-current, non-errored id should no-op")
	}
}

// TestEnsureDiffRetriesOnReselectionAfterAPriorError pins the pane's whole
// "retry hint": navigating back to a worktree whose last fetch failed must
// retry, not silently stay stuck on the error.
func TestEnsureDiffRetriesOnReselectionAfterAPriorError(t *testing.T) {
	r := newRadarView()
	r.currentID = "w1"
	r.diffErr = "boom"
	api := &fakeAPI{diff: model.Diff{WorktreeID: "w1"}}
	cmd := r.ensureDiff(context.Background(), api, "w1")
	if cmd == nil {
		t.Fatal("expected a retry fetch when currentID matches but the last attempt errored")
	}
}

func TestEnsureDiffEmptyIDIsNoOp(t *testing.T) {
	r := newRadarView()
	if cmd := r.ensureDiff(context.Background(), &fakeAPI{}, ""); cmd != nil {
		t.Error("empty id should never dispatch a fetch")
	}
}

// ---- applyDiffMsg / applyDiffErrMsg: rapid-navigation correctness ----

// TestApplyDiffMsgForStaleSelectionIsCachedButNotDisplayed pins §2.4's
// order-independence: a diff for a worktree the user has since navigated
// away from must not clobber what's on screen, but should still be cached
// for next time.
func TestApplyDiffMsgForStaleSelectionIsCachedButNotDisplayed(t *testing.T) {
	r := newRadarView()
	r.currentID = "w2" // user has already moved on to w2
	r.applyDiffMsg(diffMsg{ID: "w1", Diff: model.Diff{WorktreeID: "w1"}})

	if r.pane.diff.WorktreeID == "w1" {
		t.Error("a stale diffMsg must not overwrite the pane")
	}
	if _, ok := r.diffs.get("w1"); !ok {
		t.Error("the stale result should still be cached for a future revisit")
	}
}

func TestApplyDiffMsgForCurrentSelectionUpdatesPaneAndClearsError(t *testing.T) {
	r := newRadarView()
	r.currentID = "w1"
	r.diffErr = "previous failure"
	r.applyDiffMsg(diffMsg{ID: "w1", Diff: model.Diff{WorktreeID: "w1"}})
	if r.pane.diff.WorktreeID != "w1" {
		t.Error("pane should now show w1's diff")
	}
	if r.diffErr != "" {
		t.Errorf("diffErr = %q, want cleared on a successful load", r.diffErr)
	}
}

func TestApplyDiffErrMsgOnlySetsErrorForCurrentSelection(t *testing.T) {
	r := newRadarView()
	r.currentID = "w2"
	r.applyDiffErrMsg(diffErrMsg{ID: "w1", Err: errors.New("boom")})
	if r.diffErr != "" {
		t.Errorf("diffErr = %q, want empty (error was for a stale selection)", r.diffErr)
	}

	r.currentID = "w1"
	r.applyDiffErrMsg(diffErrMsg{ID: "w1", Err: errors.New("boom")})
	if r.diffErr != "boom" {
		t.Errorf("diffErr = %q, want boom", r.diffErr)
	}
}

// ---- applyDiffReady: hash-gated refetch ----

func TestApplyDiffReadyIgnoredForNonCurrentWorktree(t *testing.T) {
	r := newRadarView()
	r.currentID = "w1"
	cmd := r.applyDiffReady(context.Background(), &fakeAPI{}, model.Event{Type: model.EventDiffReady, ID: "other", Hash: "h2"})
	if cmd != nil {
		t.Error("diff.ready for a different worktree must not trigger a refetch")
	}
}

func TestApplyDiffReadyIgnoredWhenHashUnchanged(t *testing.T) {
	r := newRadarView()
	r.currentID = "w1"
	r.diffs.put("w1", model.Diff{WorktreeID: "w1", Hash: "h1"})
	cmd := r.applyDiffReady(context.Background(), &fakeAPI{}, model.Event{Type: model.EventDiffReady, ID: "w1", Hash: "h1"})
	if cmd != nil {
		t.Error("diff.ready with an unchanged hash must be debounced away")
	}
}

func TestApplyDiffReadyRefetchesOnHashChange(t *testing.T) {
	r := newRadarView()
	r.currentID = "w1"
	r.diffs.put("w1", model.Diff{WorktreeID: "w1", Hash: "h1"})
	api := &fakeAPI{diff: model.Diff{WorktreeID: "w1", Hash: "h2"}}
	cmd := r.applyDiffReady(context.Background(), api, model.Event{Type: model.EventDiffReady, ID: "w1", Hash: "h2"})
	if cmd == nil {
		t.Fatal("diff.ready with a changed hash must refetch")
	}
	msg := cmd()
	if dm, ok := msg.(diffMsg); !ok || dm.Diff.Hash != "h2" {
		t.Errorf("cmd() = %#v, want a diffMsg carrying the new hash", msg)
	}
}

// ---- guardrail banner / danger tag ----

func TestDangerFilesIncludesAnyHitRegardlessOfSeverity(t *testing.T) {
	got := dangerFiles([]model.GuardrailHit{
		{File: "a.go", Severity: "warn"},
		{File: "b.go", Severity: "danger"},
		{File: "", Severity: "danger"}, // no file named: not a per-file tag
	})
	if !got["a.go"] || !got["b.go"] {
		t.Errorf("dangerFiles = %v, want both a.go and b.go present", got)
	}
	if len(got) != 2 {
		t.Errorf("dangerFiles = %v, want exactly 2 entries", got)
	}
}

func TestRenderGuardrailBannerEmptyWhenNoHits(t *testing.T) {
	if got := renderGuardrailBanner(80, nil); got != "" {
		t.Errorf("banner with no hits = %q, want empty", got)
	}
}

func TestRenderGuardrailBannerShowsMessageVerbatimAndMoreCount(t *testing.T) {
	hits := []model.GuardrailHit{
		{Rule: "r1", Severity: "warn", Message: "touches migrations/"},
		{Rule: "r2", Severity: "danger", Message: "deletes too much"},
	}
	got := stripANSI(renderGuardrailBanner(80, hits))
	if !strings.Contains(got, "deletes too much") {
		t.Errorf("banner = %q, want the danger-severity hit's message verbatim", got)
	}
	if !strings.Contains(got, "+1 more") {
		t.Errorf("banner = %q, want a \"+1 more\" suffix for the second hit", got)
	}
}

// ---- diff pane header ----

func TestRenderDiffHeaderContainsRepoNameAndBase(t *testing.T) {
	w := model.Worktree{Repo: "api-server", Name: "auth-refactor", Base: "main", Stats: model.Stats{Files: 3, Add: 10, Del: 2}}
	got := stripANSI(renderDiffHeader(80, w))
	for _, want := range []string{"api-server", "auth-refactor", "main", "3 files"} {
		if !strings.Contains(got, want) {
			t.Errorf("header = %q, want it to contain %q", got, want)
		}
	}
}

// ---- radarView.view: loading / error / real content ----

func TestRadarViewShowsLoadingPlaceholderBeforeDiffArrives(t *testing.T) {
	r := newRadarView()
	w := model.Worktree{ID: "w1", Repo: "api", Name: "feature", Base: "main"}
	out := stripANSI(r.view(80, 24, w))
	if !strings.Contains(out, "loading diff") {
		t.Errorf("view = %q, want a loading placeholder before any diff has loaded", out)
	}
}

func TestRadarViewShowsErrorInsteadOfPane(t *testing.T) {
	r := newRadarView()
	r.currentID = "w1"
	r.diffErr = "unknown worktree id"
	w := model.Worktree{ID: "w1", Repo: "api", Name: "feature", Base: "main"}
	out := stripANSI(r.view(80, 24, w))
	if !strings.Contains(out, "unknown worktree id") {
		t.Errorf("view = %q, want the daemon's error message", out)
	}
}

func TestRadarViewRendersLoadedDiffWithGuardrailBanner(t *testing.T) {
	r := newRadarView()
	d := model.Diff{WorktreeID: "w1", Files: []model.DiffFile{
		{Path: "migrations/x.sql", Status: model.FileModified, Hash: "h1", Stats: model.Stats{Add: 1, Del: 90},
			Hunks: []model.Hunk{{Header: "@@ -1,90 +1,1 @@", Lines: []model.Line{line(model.LineDel, 1, 0, "DROP TABLE x")}}}},
	}}
	r.currentID = "w1"
	r.pane.setDiff(d)
	w := model.Worktree{
		ID: "w1", Repo: "infra", Name: "migrate", Base: "main",
		Guardrails: []model.GuardrailHit{{Rule: "big-delete", Severity: "danger", Message: "deletes too much", File: "migrations/x.sql"}},
	}
	out := stripANSI(r.view(80, 24, w))
	if !strings.Contains(out, "deletes too much") {
		t.Errorf("view = %q, want the guardrail banner", out)
	}
	if !strings.Contains(out, "migrations/") || !strings.Contains(out, "x.sql") {
		t.Errorf("view = %q, want the file card", out)
	}
	if !strings.Contains(out, "danger") {
		t.Errorf("view = %q, want the file's danger tag", out)
	}
}
