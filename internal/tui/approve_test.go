package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/navbytes/wt-cockpit/internal/model"
)

// ---- openApprove: "summon only when the pane's state allows" ----

func TestOpenApproveBlockedWhenNothingSelected(t *testing.T) {
	m := newTestModel(&fakeAPI{}) // empty sidebar
	updated, _ := m.openApprove()
	if got := updated.(appModel); got.approve.open {
		t.Error("approve.open = true, want blocked with nothing selected")
	}
}

func TestOpenApprovePopulatesFieldsFromSelectedWorktree(t *testing.T) {
	m := newTestModel(&fakeAPI{})
	m.sidebar.setWorktrees([]model.Worktree{
		{ID: "a1", Repo: "api", Name: "feature", Branch: "feature", Base: "main", Reviewed: 2, Stats: model.Stats{Files: 3}},
	})
	updated, _ := m.openApprove()
	got := updated.(appModel).approve
	if !got.open || got.id != "a1" || got.branch != "feature" || got.base != "main" || got.reviewed != 2 || got.total != 3 {
		t.Errorf("approve modal = %+v, want open for a1/feature/main 2/3", got)
	}
}

// TestOpenApprovePrefersLoadedDiffOverStaleAggregateCount pins a real
// discrepancy caught in live smoke: right after a space-toggle or an
// external edit, Worktree.Reviewed (SSE-lagged) can disagree with the
// diff's own per-file map (already current). The modal should show the
// fresher number when the diff for this worktree is already loaded.
func TestOpenApprovePrefersLoadedDiffOverStaleAggregateCount(t *testing.T) {
	m := newTestModel(&fakeAPI{})
	m.sidebar.setWorktrees([]model.Worktree{
		{ID: "a1", Repo: "api", Name: "feature", Reviewed: 1, Stats: model.Stats{Files: 2}}, // stale: says 1/2
	})
	m.radar.currentID = "a1"
	m.radar.pane.setDiff(model.Diff{
		WorktreeID: "a1",
		Files:      []model.DiffFile{{Path: "a.go"}, {Path: "b.go"}},
		Reviewed:   map[string]bool{}, // fresh: actually 0/2
	})

	updated, _ := m.openApprove()
	got := updated.(appModel).approve
	if got.reviewed != 0 || got.total != 2 {
		t.Errorf("approve modal reviewed/total = %d/%d, want the fresher 0/2 from the loaded diff, not the stale aggregate 1/2", got.reviewed, got.total)
	}
}

// TestOpenApproveFallsBackToAggregateWhenDiffNotLoadedForThisWorktree covers
// opening approve from Radar before its diff has (re)loaded, or for a
// different worktree than whatever the pane last showed.
func TestOpenApproveFallsBackToAggregateWhenDiffNotLoadedForThisWorktree(t *testing.T) {
	m := newTestModel(&fakeAPI{})
	m.sidebar.setWorktrees([]model.Worktree{{ID: "a1", Repo: "api", Name: "feature", Reviewed: 2, Stats: model.Stats{Files: 3}}})
	// m.radar.pane.diff is the zero value: WorktreeID "" != "a1".

	updated, _ := m.openApprove()
	got := updated.(appModel).approve
	if got.reviewed != 2 || got.total != 3 {
		t.Errorf("approve modal reviewed/total = %d/%d, want the aggregate 2/3 when no matching diff is loaded", got.reviewed, got.total)
	}
}

func TestOpenApproveAlreadyOpenDoesNotResetInFlightState(t *testing.T) {
	m := newTestModel(&fakeAPI{})
	m.sidebar.setWorktrees([]model.Worktree{{ID: "a1", Repo: "api", Name: "feature"}})
	m.approve = approveModal{open: true, id: "a1", submitting: true, errMsg: "boom"}

	updated, _ := m.openApprove()
	got := updated.(appModel).approve
	if !got.submitting || got.errMsg != "boom" {
		t.Errorf("re-pressing `a` while already open must not reset in-flight state, got %+v", got)
	}
}

// TestHandleKeyApproveWorksFromRadarAndReviewScreens pins "same confirm
// modal" reachability from both screens.
func TestHandleKeyApproveWorksFromRadarAndReviewScreens(t *testing.T) {
	for _, screen := range []screenID{screenRadar, screenReview} {
		m := newTestModel(&fakeAPI{})
		m.sidebar.setWorktrees([]model.Worktree{{ID: "a1", Repo: "api", Name: "feature"}})
		m.screen = screen

		updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
		if got := updated.(appModel); !got.approve.open {
			t.Errorf("screen=%v: approve.open = false, want true after `a`", screen)
		}
	}
}

// ---- handleApproveKey: submit / cancel / quit-still-works / q-inert ----

func TestHandleApproveKeyEnterSubmitsAndDispatchesApproveCmd(t *testing.T) {
	api := &fakeAPI{approveResult: model.ApproveResult{WorktreeID: "a1", Merged: "feature", Into: "main"}}
	m := newTestModel(api)
	m.approve = approveModal{open: true, id: "a1"}

	updated, cmd := m.handleApproveKey(tea.KeyMsg{Type: tea.KeyEnter})
	got := updated.(appModel)
	if !got.approve.submitting {
		t.Error("submitting should be true right after pressing enter")
	}
	if cmd == nil {
		t.Fatal("expected a non-nil approve command")
	}
	msg := cmd()
	ok, isOK := msg.(approveOKMsg)
	if !isOK || ok.Res.Merged != "feature" {
		t.Fatalf("cmd() = %#v, want approveOKMsg{Merged: feature}", msg)
	}
	if len(api.approveCalls) != 1 || api.approveCalls[0] != "a1" {
		t.Errorf("Approve calls = %v, want exactly [a1]", api.approveCalls)
	}
}

func TestHandleApproveKeyEnterNoOpWhileAlreadySubmitting(t *testing.T) {
	api := &fakeAPI{}
	m := newTestModel(api)
	m.approve = approveModal{open: true, id: "a1", submitting: true}

	_, cmd := m.handleApproveKey(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil {
		t.Error("a second enter while already submitting must not re-dispatch")
	}
	if len(api.approveCalls) != 0 {
		t.Errorf("Approve calls = %v, want none (already in flight)", api.approveCalls)
	}
}

func TestHandleApproveKeyEscClosesModal(t *testing.T) {
	m := newTestModel(&fakeAPI{})
	m.approve = approveModal{open: true, id: "a1", errMsg: "some earlier failure"}

	updated, _ := m.handleApproveKey(tea.KeyMsg{Type: tea.KeyEscape})
	if got := updated.(appModel); got.approve.open {
		t.Error("esc should close the modal")
	}
}

func TestHandleApproveKeyPlainQIsInertModalStaysOpen(t *testing.T) {
	m := newTestModel(&fakeAPI{})
	m.approve = approveModal{open: true, id: "a1"}

	updated, cmd := m.handleApproveKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if cmd != nil {
		if _, quit := cmd().(tea.QuitMsg); quit {
			t.Error("plain q must not quit while the approve modal is open")
		}
	}
	if got := updated.(appModel); !got.approve.open {
		t.Error("plain q must not close the approve modal")
	}
}

func TestHandleApproveKeyCtrlCStillQuits(t *testing.T) {
	m := newTestModel(&fakeAPI{})
	m.approve = approveModal{open: true, id: "a1"}

	_, cmd := m.handleApproveKey(tea.KeyMsg{Type: tea.KeyCtrlC})
	if !isQuitCmd(cmd) {
		t.Error("ctrl+c must still quit even while the approve modal is open")
	}
}

// ---- approveErrMsg / approveOKMsg via Update() ----

// TestApproveErrMsgKeepsModalOpenWithGateMessageVerbatim pins the
// gate-error-render state.
func TestApproveErrMsgKeepsModalOpenWithGateMessageVerbatim(t *testing.T) {
	m := newTestModel(&fakeAPI{})
	m.approve = approveModal{open: true, id: "a1", submitting: true}

	updated, _ := m.Update(approveErrMsg{Gate: "cannot approve: 1 of 3 files not yet reviewed"})
	got := updated.(appModel).approve
	if !got.open {
		t.Error("modal must stay open on a refused gate")
	}
	if got.submitting {
		t.Error("submitting should reset to false once the error lands")
	}
	if got.errMsg != "cannot approve: 1 of 3 files not yet reviewed" {
		t.Errorf("errMsg = %q, want the daemon's message verbatim", got.errMsg)
	}
}

// TestApproveOKMsgClosesModalTostsSuccessAndReturnsToRadar pins the success
// state: modal closes, toast fires, and Review (if that's where approve was
// summoned from) doesn't linger on a worktree that's about to disappear.
func TestApproveOKMsgClosesModalTostsSuccessAndReturnsToRadar(t *testing.T) {
	m := newTestModel(&fakeAPI{})
	m.approve = approveModal{open: true, id: "a1", submitting: true}
	m.screen = screenReview

	updated, cmd := m.Update(approveOKMsg{Res: model.ApproveResult{WorktreeID: "a1", Merged: "feature", Into: "main", Removed: "/repo/a1"}})
	got := updated.(appModel)
	if got.approve.open {
		t.Error("modal must close on success")
	}
	if got.screen != screenRadar {
		t.Errorf("screen = %v, want screenRadar after a successful approve", got.screen)
	}
	if !strings.Contains(got.toast, "feature") || !strings.Contains(got.toast, "main") {
		t.Errorf("toast = %q, want it to name the merged branch and target", got.toast)
	}
	if cmd == nil {
		t.Error("expected the toast-expiry command to be armed")
	}
}

// ---- rendering ----

func TestRenderApproveModalShowsMergeLineAndGateInfo(t *testing.T) {
	a := approveModal{id: "a1", branch: "feature", base: "main", reviewed: 2, total: 3}
	out := stripANSI(renderApproveModal(80, 24, a))
	for _, want := range []string{"feature", "main", "reviewed 2/3", "full review", "clean tree", "clean merge"} {
		if !strings.Contains(out, want) {
			t.Errorf("modal = %q, want it to contain %q", out, want)
		}
	}
}

func TestRenderApproveModalShowsErrorVerbatim(t *testing.T) {
	a := approveModal{id: "a1", branch: "feature", base: "main", errMsg: "cannot approve: worktree has uncommitted changes"}
	out := stripANSI(renderApproveModal(80, 24, a))
	if !strings.Contains(out, "cannot approve: worktree has uncommitted changes") {
		t.Errorf("modal = %q, want the daemon's error verbatim", out)
	}
}

func TestApproveCmdWrapsNonGateErrorsToo(t *testing.T) {
	api := &fakeAPI{approveErr: errors.New("cannot reach wtd (is it running?): dial unix: no such file")}
	cmd := approveCmd(context.Background(), api, "a1")
	msg, ok := cmd().(approveErrMsg)
	if !ok || !strings.Contains(msg.Gate, "cannot reach wtd") {
		t.Errorf("cmd() = %#v, want approveErrMsg carrying the unreachable error's text", msg)
	}
}
