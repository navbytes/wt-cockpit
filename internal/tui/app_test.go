package tui

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/navbytes/wt-cockpit/internal/client"
	"github.com/navbytes/wt-cockpit/internal/model"
)

// updateGolden regenerates testdata/radar_frame.golden from the currently
// rendered frame instead of comparing against it: `go test ./internal/tui/
// -run TestRunRadarDiffPaneMatchesGoldenFrame -update`.
var updateGolden = flag.Bool("update", false, "update golden files")

// newTestModel builds a bare appModel for pure Update()/View() table tests —
// no conn.go goroutine involved (connCh stays nil; nothing in these tests
// invokes the Cmd that would block reading it). radar is initialized the
// same way runProgram does it (a real highlightCache, not a nil one) since
// WP2's Up/Down/listMsg/Enter paths all reach into it.
func newTestModel(api apiClient) appModel {
	return appModel{api: api, ctx: context.Background(), retryCh: make(chan struct{}, 1), radar: newRadarView()}
}

// ---- startup budget (P3-design.md §6: "< 150ms to first paint... Init only
// *dispatches* cmds") ----

// TestInitReturnsImmediatelyEvenWithASlowBackend pins the startup-budget
// mitigation literally: Init must hand back a tea.Cmd batch (deferred
// closures bubbletea's own runtime invokes later, concurrently, off this
// call) rather than ever calling the API synchronously inline. A fakeAPI
// whose Worktrees blocks for well over a "human-perceptible" delay proves
// the difference — a regression that inlined the fetch (defeating the
// whole "shell paints before any I/O" contract) would make this test time
// out, not just run slow.
func TestInitReturnsImmediatelyEvenWithASlowBackend(t *testing.T) {
	block := make(chan struct{})
	api := &fakeAPI{
		worktreesFn: func(ctx context.Context) ([]model.Worktree, error) {
			<-block // never closed in this test: Init must not wait on this
			return nil, nil
		},
	}
	m := newTestModel(api)

	start := time.Now()
	cmd := m.Init()
	if elapsed := time.Since(start); elapsed > 20*time.Millisecond {
		t.Errorf("Init() took %v to return, want well under 20ms regardless of backend latency", elapsed)
	}
	if cmd == nil {
		t.Fatal("Init() returned a nil Cmd")
	}
}

// TestViewRendersBeforeInitsCommandsHaveRun is the View half of the same
// budget: the very first frame (P3-design.md §1.4's "loading" state) must
// not depend on any data having arrived yet.
func TestViewRendersBeforeInitsCommandsHaveRun(t *testing.T) {
	m := newTestModel(&fakeAPI{})
	start := time.Now()
	out := m.View()
	if elapsed := time.Since(start); elapsed > 20*time.Millisecond {
		t.Errorf("View() took %v before any msg arrived, want well under 20ms", elapsed)
	}
	if out == "" {
		t.Error("View() before any size/data message rendered nothing, want the loading placeholder")
	}
}

// ---- resize / min-size guard ----

func TestUpdateWindowSizeMsgSetsDimensions(t *testing.T) {
	m := newTestModel(&fakeAPI{})
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	got := updated.(appModel)
	if got.width != 100 || got.height != 40 {
		t.Errorf("width/height = %d/%d, want 100/40", got.width, got.height)
	}
}

func TestViewBeforeAnySizeShowsLoadingNotTooSmall(t *testing.T) {
	m := newTestModel(&fakeAPI{})
	view := m.View()
	if strings.Contains(view, "too small") {
		t.Errorf("View() before any WindowSizeMsg = %q, must not show the too-small card", view)
	}
}

func TestViewBelowMinimumShowsTooSmallCard(t *testing.T) {
	m := newTestModel(&fakeAPI{})
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 20, Height: 5})
	view := updated.(appModel).View()
	if !strings.Contains(view, "too small") || !strings.Contains(view, "20x5") {
		t.Errorf("View() = %q, want a too-small card naming 20x5", view)
	}
}

func TestViewAtOrAboveMinimumNeverShowsTooSmallCard(t *testing.T) {
	m := newTestModel(&fakeAPI{})
	updated, _ := m.Update(tea.WindowSizeMsg{Width: minWidth, Height: minHeight})
	view := updated.(appModel).View()
	if strings.Contains(view, "too small") {
		t.Errorf("View() at exactly %dx%d = %q, must not show the too-small card", minWidth, minHeight, view)
	}
}

func TestViewRecoversInstantlyOnResizeBackAboveMinimum(t *testing.T) {
	m := newTestModel(&fakeAPI{})
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 20, Height: 5})
	updated, _ = updated.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	view := updated.(appModel).View()
	if strings.Contains(view, "too small") {
		t.Error("resizing back above the minimum should drop the too-small card immediately")
	}
}

// ---- quit ----

func TestHandleKeyQuitReturnsQuitCmdWithNilExitErr(t *testing.T) {
	m := newTestModel(&fakeAPI{})
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if !isQuitCmd(cmd) {
		t.Fatal("q should return tea.Quit")
	}
	if got := updated.(appModel); got.exitErr != nil {
		t.Errorf("exitErr = %v, want nil for a plain quit", got.exitErr)
	}
}

func TestHandleKeyCtrlCQuits(t *testing.T) {
	m := newTestModel(&fakeAPI{})
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if !isQuitCmd(cmd) {
		t.Fatal("ctrl+c should return tea.Quit")
	}
}

// TestAnyKeyQuitsNonZeroWhenFatal pins P3-design.md §1.4: "any key exits
// non-zero" once a fatal (protocol mismatch) card is showing.
func TestAnyKeyQuitsNonZeroWhenFatal(t *testing.T) {
	m := newTestModel(&fakeAPI{})
	m.fatal = "protocol mismatch: wt speaks 1, wtd speaks 2"

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	if !isQuitCmd(cmd) {
		t.Fatal("any key must quit once a fatal condition is set")
	}
	got := updated.(appModel)
	if got.exitErr == nil {
		t.Fatal("exitErr must be set so Run() reports a nonzero exit")
	}
	if !strings.Contains(got.exitErr.Error(), "protocol mismatch") {
		t.Errorf("exitErr = %v, want it to mention the mismatch", got.exitErr)
	}
}

func isQuitCmd(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	_, ok := cmd().(tea.QuitMsg)
	return ok
}

// ---- view routing (r / esc) ----

func TestHandleKeyReviewSwitchesScreenWhenWorktreeSelected(t *testing.T) {
	m := newTestModel(&fakeAPI{})
	m.sidebar.setWorktrees([]model.Worktree{{ID: "a1", Repo: "api", Name: "feature"}})

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if got := updated.(appModel); got.screen != screenReview {
		t.Errorf("screen = %v, want screenReview", got.screen)
	}
}

func TestHandleKeyReviewNoOpWhenNothingSelected(t *testing.T) {
	m := newTestModel(&fakeAPI{}) // empty sidebar: nothing selected
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if got := updated.(appModel); got.screen != screenRadar {
		t.Errorf("screen = %v, want screenRadar (no worktree to review)", got.screen)
	}
}

func TestHandleKeyEscReturnsToRadar(t *testing.T) {
	m := newTestModel(&fakeAPI{})
	m.sidebar.setWorktrees([]model.Worktree{{ID: "a1", Repo: "api", Name: "feature"}})
	m.screen = screenReview

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEscape})
	if got := updated.(appModel); got.screen != screenRadar {
		t.Errorf("screen = %v, want screenRadar after esc", got.screen)
	}
}

// ---- diff-pane focus (⏎ / esc) and scroll-key routing ----

func TestHandleKeyEnterFocusesDiffPaneWhenWorktreeSelected(t *testing.T) {
	m := newTestModel(&fakeAPI{})
	m.sidebar.setWorktrees([]model.Worktree{{ID: "a1", Repo: "api", Name: "feature"}})

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if got := updated.(appModel); !got.diffFocused {
		t.Error("⏎ should focus the diff pane when a worktree is selected")
	}
}

func TestHandleKeyEnterNoOpWhenNothingSelected(t *testing.T) {
	m := newTestModel(&fakeAPI{}) // empty sidebar
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if got := updated.(appModel); got.diffFocused {
		t.Error("⏎ with nothing selected must not focus the diff pane")
	}
}

func TestHandleKeyEnterNoOpInReviewScreen(t *testing.T) {
	m := newTestModel(&fakeAPI{})
	m.sidebar.setWorktrees([]model.Worktree{{ID: "a1", Repo: "api", Name: "feature"}})
	m.screen = screenReview

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if got := updated.(appModel); got.diffFocused {
		t.Error("⏎ is Radar's own focus toggle in WP2; Review's own is WP3")
	}
}

func TestHandleKeyEscWhileDiffFocusedUnfocusesWithoutChangingScreen(t *testing.T) {
	m := newTestModel(&fakeAPI{})
	m.sidebar.setWorktrees([]model.Worktree{{ID: "a1", Repo: "api", Name: "feature"}})
	m.diffFocused = true

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEscape})
	got := updated.(appModel)
	if got.diffFocused {
		t.Error("esc while diff-focused should un-focus first")
	}
	if got.screen != screenRadar {
		t.Errorf("screen = %v, want unchanged screenRadar", got.screen)
	}
}

func TestHandleKeyQuitWorksWhileDiffFocused(t *testing.T) {
	m := newTestModel(&fakeAPI{})
	m.diffFocused = true
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if !isQuitCmd(cmd) {
		t.Fatal("q must quit even while the diff pane is focused")
	}
}

func TestHandleKeyReviewResetsDiffFocused(t *testing.T) {
	m := newTestModel(&fakeAPI{})
	m.sidebar.setWorktrees([]model.Worktree{{ID: "a1", Repo: "api", Name: "feature"}})
	m.diffFocused = true

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	got := updated.(appModel)
	if got.screen != screenReview {
		t.Errorf("screen = %v, want screenReview", got.screen)
	}
	if got.diffFocused {
		t.Error("entering Review should reset diffFocused (its own focus model is WP3)")
	}
}

// TestDiffFocusedRoutesScrollKeysToThePaneNotTheSidebar pins the whole point
// of the focus model: j/k must scroll the diff, not move sidebar selection.
func TestDiffFocusedRoutesScrollKeysToThePaneNotTheSidebar(t *testing.T) {
	m := newTestModel(&fakeAPI{})
	m.sidebar.setWorktrees([]model.Worktree{
		{ID: "a1", Repo: "api", Name: "one", LastChange: fixedTime(2)},
		{ID: "a2", Repo: "api", Name: "two", LastChange: fixedTime(1)},
	})
	m.radar.pane.setDiff(manyLineDiff(50))
	m.radar.pane.setHeight(10)
	selectedBefore, _ := m.sidebar.selected()
	m.diffFocused = true

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	got := updated.(appModel)
	if got.radar.pane.offset != 1 {
		t.Errorf("pane offset = %d, want 1 after j while diff-focused", got.radar.pane.offset)
	}
	selectedAfter, _ := got.sidebar.selected()
	if selectedAfter.ID != selectedBefore.ID {
		t.Errorf("sidebar selection changed from %q to %q; j while diff-focused must not move it", selectedBefore.ID, selectedAfter.ID)
	}
}

// TestSidebarFocusedUpDownStillMovesSelectionNotThePane is the converse:
// unfocused, j/k must behave exactly as before WP2 (sidebar movement).
func TestSidebarFocusedUpDownStillMovesSelectionNotThePane(t *testing.T) {
	m := newTestModel(&fakeAPI{})
	m.sidebar.setWorktrees([]model.Worktree{
		{ID: "a1", Repo: "api", Name: "one", LastChange: fixedTime(2)},
		{ID: "a2", Repo: "api", Name: "two", LastChange: fixedTime(1)},
	})
	m.radar.pane.setDiff(manyLineDiff(50))
	m.radar.pane.setHeight(10)

	before, _ := m.sidebar.selected()
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	got := updated.(appModel)
	if got.radar.pane.offset != 0 {
		t.Errorf("pane offset = %d, want 0 (j moves the sidebar, not the pane, while unfocused)", got.radar.pane.offset)
	}
	sel, _ := got.sidebar.selected()
	if sel.ID == before.ID {
		t.Errorf("selected = %q, want it to have moved off the initial selection %q", sel.ID, before.ID)
	}
}

// ---- radar diff pane: selection -> fetch, diffMsg/diffErrMsg application ----

// TestSelectingAWorktreeDispatchesADiffFetch pins "radar screen completion:
// selection -> enter opens the diff pane for that worktree" at the fetch
// layer: even before ⏎, moving the sidebar selection primes the diff pane.
func TestSelectingAWorktreeDispatchesADiffFetch(t *testing.T) {
	api := &fakeAPI{diff: model.Diff{WorktreeID: "a1", Hash: "h1"}}
	m := newTestModel(api)
	updated, cmd := m.Update(listMsg{{ID: "a1", Repo: "api", Name: "one"}})
	got := updated.(appModel)
	if got.radar.currentID != "a1" {
		t.Fatalf("radar.currentID = %q, want a1 once it's the selection", got.radar.currentID)
	}
	if cmd == nil {
		t.Fatal("expected a non-nil command to fetch a1's diff")
	}

	// Drive the fetch and apply through Update(), exactly as bubbletea would.
	var dm diffMsg
	var found bool
	for _, msg := range runBatch(t, cmd) {
		if d, ok := msg.(diffMsg); ok {
			dm, found = d, true
		}
	}
	if !found || dm.ID != "a1" {
		t.Fatalf("expected a diffMsg for a1 among the dispatched commands")
	}
	updated2, _ := got.Update(dm)
	final := updated2.(appModel)
	if final.radar.pane.diff.WorktreeID != "a1" {
		t.Errorf("pane.diff.WorktreeID = %q, want a1 after applying the fetch result", final.radar.pane.diff.WorktreeID)
	}
}

func TestDiffMsgUpdatesRadarPane(t *testing.T) {
	m := newTestModel(&fakeAPI{})
	m.sidebar.setWorktrees([]model.Worktree{{ID: "a1", Repo: "api", Name: "one"}})
	m.radar.currentID = "a1"

	updated, _ := m.Update(diffMsg{ID: "a1", Diff: model.Diff{WorktreeID: "a1", Hash: "h1"}})
	got := updated.(appModel)
	if got.radar.pane.diff.WorktreeID != "a1" {
		t.Errorf("pane.diff.WorktreeID = %q, want a1", got.radar.pane.diff.WorktreeID)
	}
}

func TestDiffErrMsgSurfacesInRadarView(t *testing.T) {
	m := newTestModel(&fakeAPI{})
	m.sidebar.setWorktrees([]model.Worktree{{ID: "a1", Repo: "api", Name: "one"}})
	m.radar.currentID = "a1"

	updated, _ := m.Update(diffErrMsg{ID: "a1", Err: errUnknownWorktree})
	got := updated.(appModel)
	if got.radar.diffErr == "" {
		t.Error("diffErrMsg for the current selection should set radar.diffErr")
	}
}

var errUnknownWorktree = errors.New("unknown worktree id")

// fixedTime avoids relying on time.Now() ordering flakiness in sidebar
// most-recent-first sort assertions.
func fixedTime(minutesAgo int) time.Time {
	return time.Now().Add(-time.Duration(minutesAgo) * time.Minute)
}

// ---- version / protocol handshake ----

func TestVersionMsgSetsFatalOnProtocolMismatch(t *testing.T) {
	m := newTestModel(&fakeAPI{})
	updated, _ := m.Update(versionMsg{Protocol: model.ProtocolVersion + 1, Version: "future"})
	got := updated.(appModel)
	if got.fatal == "" {
		t.Fatal("fatal should be set on a protocol mismatch")
	}
	if !strings.Contains(got.fatal, "mismatch") {
		t.Errorf("fatal = %q, want it to mention the mismatch", got.fatal)
	}
}

func TestVersionMsgNoFatalOnMatchingProtocol(t *testing.T) {
	m := newTestModel(&fakeAPI{})
	updated, _ := m.Update(versionMsg{Protocol: model.ProtocolVersion, Version: "v0.3.0"})
	if got := updated.(appModel); got.fatal != "" {
		t.Errorf("fatal = %q, want empty for a matching protocol", got.fatal)
	}
}

func TestVersionErrMsgUnreachableDoesNotSetFatal(t *testing.T) {
	m := newTestModel(&fakeAPI{})
	updated, _ := m.Update(versionErrMsg{Err: &client.UnreachableError{Err: errors.New("connection refused")}})
	if got := updated.(appModel); got.fatal != "" {
		t.Errorf("fatal = %q, want empty — an unreachable daemon is the down/reconnecting state, not fatal", got.fatal)
	}
}

func TestVersionErrMsgNonUnreachableSetsFatal(t *testing.T) {
	m := newTestModel(&fakeAPI{})
	updated, _ := m.Update(versionErrMsg{Err: errors.New("wtd does not support the protocol handshake (pre-v0.2)")})
	got := updated.(appModel)
	if got.fatal == "" {
		t.Fatal("fatal should be set when wtd answers but refuses the handshake outright")
	}
}

// ---- conn state machine (Update side) ----

func TestConnMsgUpdatesConnStateAndRearmsWait(t *testing.T) {
	m := newTestModel(&fakeAPI{})
	m.connCh = make(chan connEvent) // never fires; only its presence matters
	updated, cmd := m.Update(connMsg{State: connReconnecting, Attempt: 3, Wait: 2 * time.Second})
	got := updated.(appModel)
	if got.conn != connReconnecting || got.connAttempt != 3 || got.connWait != 2*time.Second {
		t.Errorf("conn state = %+v, want connReconnecting/3/2s", got)
	}
	if cmd == nil {
		t.Error("Update on connMsg must re-arm waitForConnEvent")
	}
}

func TestDownCardShowsWhenNeverConnectedAndConnDown(t *testing.T) {
	m := newTestModel(&fakeAPI{})
	m.socket = "/tmp/wtd.sock"
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m2 := updated.(appModel)
	m2.conn = connDown
	m2.connAttempt = 3
	m2.connWait = 4 * time.Second

	view := m2.View()
	if !strings.Contains(view, "/tmp/wtd.sock") {
		t.Errorf("down card = %q, want it to name the socket path", view)
	}
	if !strings.Contains(view, "attempt 3") {
		t.Errorf("down card = %q, want the attempt count", view)
	}
}

func TestDownCardNeverShowsOnceDataHasArrived(t *testing.T) {
	m := newTestModel(&fakeAPI{})
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m2 := updated.(appModel)
	m2.sidebar.setWorktrees([]model.Worktree{{ID: "a1", Repo: "api", Name: "feature"}})
	m2.conn = connReconnecting // a later mid-session drop

	view := m2.View()
	if strings.Contains(view, "is it running") {
		t.Error("once real data has arrived, a later reconnect must not bring back the full-screen down card")
	}
	if !strings.Contains(view, "feature") {
		t.Errorf("view = %q, want the shell still showing the known worktree", view)
	}
}

// ---- event application ----

func TestApplyEventUpsertAddsWorktreeToSidebar(t *testing.T) {
	m := newTestModel(&fakeAPI{})
	m.connCh = make(chan connEvent)
	w := model.Worktree{ID: "a1", Repo: "api", Name: "feature"}
	updated, _ := m.Update(eventMsg{Type: model.EventWorktreeUpserted, Worktree: &w})
	got := updated.(appModel)
	if _, ok := got.sidebar.selected(); !ok {
		t.Fatal("upserted worktree should become selectable")
	}
}

func TestApplyEventRemoveDropsWorktreeFromSidebar(t *testing.T) {
	m := newTestModel(&fakeAPI{})
	m.connCh = make(chan connEvent)
	m.sidebar.setWorktrees([]model.Worktree{{ID: "a1", Repo: "api", Name: "feature"}})

	updated, _ := m.Update(eventMsg{Type: model.EventWorktreeRemoved, ID: "a1"})
	got := updated.(appModel)
	if _, ok := got.sidebar.selected(); ok {
		t.Error("removed worktree must no longer be selectable")
	}
}

// TestApplyEventSnapshotTriggersVersionAndListRefetch pins §2.4's resync
// story: a snapshot (sent on every SSE (re)connect) re-runs fetchVersion +
// fetchList, executed here by invoking the returned batch's sub-commands.
func TestApplyEventSnapshotTriggersVersionAndListRefetch(t *testing.T) {
	api := &fakeAPI{protocol: model.ProtocolVersion, version: "v0.3.0", worktrees: []model.Worktree{{ID: "a1"}}}
	m := newTestModel(api)

	// applyEvent directly, not Update: Update's eventMsg case also re-arms
	// waitForConnEvent(m.connCh), which blocks on the real connection
	// channel by design (nothing feeds it in this model-only test) — that
	// half is already covered by TestConnMsgUpdatesConnStateAndRearmsWait.
	cmd := m.applyEvent(model.Event{Type: eventTypeSnapshot})
	if cmd == nil {
		t.Fatal("snapshot must return a non-nil batched refetch command")
	}
	msgs := runBatch(t, cmd)

	var sawVersion, sawList bool
	for _, msg := range msgs {
		switch msg.(type) {
		case versionMsg:
			sawVersion = true
		case listMsg:
			sawList = true
		}
	}
	if !sawVersion || !sawList {
		t.Errorf("batch results = %#v, want both a versionMsg and a listMsg", msgs)
	}
}

// runBatch executes a tea.Cmd that's expected to be a tea.Batch (recursing
// into any nested batch), collecting every leaf result — bubbletea's own
// runtime does exactly this internally; tests do it explicitly since
// there's no live Program here. Callers must only pass commands known to
// terminate on their own (never e.g. waitForConnEvent, which blocks until
// the connection loop sends something, by design).
func runBatch(t *testing.T, cmd tea.Cmd) []tea.Msg {
	t.Helper()
	msg := cmd()
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		return []tea.Msg{msg}
	}
	var out []tea.Msg
	for _, sub := range batch {
		out = append(out, runBatch(t, sub)...)
	}
	return out
}

// ---- refresh (R) ----

func TestHandleKeyRefreshInvokesRefreshAndReportsResult(t *testing.T) {
	api := &fakeAPI{}
	m := newTestModel(api)
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("R")})
	if cmd == nil {
		t.Fatal("R must return a refresh command")
	}
	msg := cmd()
	done, ok := msg.(refreshDoneMsg)
	if !ok {
		t.Fatalf("R's command produced %#v, want refreshDoneMsg", msg)
	}
	if done.Err != nil {
		t.Errorf("Err = %v, want nil for a successful refresh", done.Err)
	}
}

// ---- review toggle (space, in Review view) ----

// reviewFixture builds a model with a1 selected, in Review, with a
// two-file diff already loaded into the shared pane — the common setup for
// the toggle/reconciliation tests below.
func reviewFixture(api apiClient) appModel {
	m := newTestModel(api)
	m.sidebar.setWorktrees([]model.Worktree{{ID: "a1", Repo: "api", Name: "feature", Base: "main"}})
	m.screen = screenReview
	m.radar.currentID = "a1"
	m.radar.pane.setDiff(model.Diff{WorktreeID: "a1", Files: []model.DiffFile{
		{Path: "a.go", Hash: "ha"}, {Path: "b.go", Hash: "hb"},
	}})
	return m
}

// TestHandleReviewKeySpaceOptimisticallyTogglesAndSendsDisplayedHash pins
// the whole point of the conflict contract: the request must carry the
// hash currently on screen, not an empty/stale one.
func TestHandleReviewKeySpaceOptimisticallyTogglesAndSendsDisplayedHash(t *testing.T) {
	api := &fakeAPI{}
	m := reviewFixture(api)

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(" ")})
	got := updated.(appModel)
	if !got.radar.pane.diff.Reviewed["a.go"] {
		t.Error("space should optimistically mark the file under the cursor (a.go) reviewed immediately")
	}
	if cmd == nil {
		t.Fatal("expected a SetReviewed command")
	}
	msg := cmd()
	ok, isOK := msg.(reviewOKMsg)
	if !isOK || ok.ID != "a1" || ok.File != "a.go" || !ok.Reviewed {
		t.Fatalf("cmd() = %#v, want reviewOKMsg{a1, a.go, true}", msg)
	}
	if len(api.setReviewedCalls) != 1 {
		t.Fatalf("SetReviewed calls = %v, want exactly 1", api.setReviewedCalls)
	}
	call := api.setReviewedCalls[0]
	if call.id != "a1" || call.file != "a.go" || !call.reviewed || call.hash != "ha" {
		t.Errorf("SetReviewed call = %+v, want {a1 a.go true ha} (the displayed hash)", call)
	}
}

func TestHandleReviewKeySpaceTogglesBackToUnreviewed(t *testing.T) {
	m := reviewFixture(&fakeAPI{})
	m.radar.pane.setReviewed("a.go", true)

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(" ")})
	if got := updated.(appModel); got.radar.pane.diff.Reviewed["a.go"] {
		t.Error("space on an already-reviewed file should toggle it back to unreviewed")
	}
}

func TestHandleReviewKeySpaceNoOpWhenNothingSelected(t *testing.T) {
	m := newTestModel(&fakeAPI{}) // empty sidebar, screenRadar by default
	m.screen = screenReview
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(" ")})
	if cmd != nil {
		t.Error("space with nothing selected should not dispatch a request")
	}
	_ = updated
}

// TestReviewOKMsgReconcilesPaneWithServerTruth covers the message actually
// landing (as opposed to just the optimistic flip pinned above).
func TestReviewOKMsgReconcilesPaneWithServerTruth(t *testing.T) {
	m := reviewFixture(&fakeAPI{})
	updated, _ := m.Update(reviewOKMsg{ID: "a1", File: "a.go", Reviewed: true})
	if got := updated.(appModel); !got.radar.pane.diff.Reviewed["a.go"] {
		t.Error("reviewOKMsg should leave the file marked reviewed")
	}
}

// TestReviewErrMsgConflictRevertsAndRefetchesDiffWithToast pins the P1
// conflict contract exercised for real: 409 -> revert the optimistic
// toggle, toast naming the file, and a diff refetch.
//
// Note: this deliberately does NOT run Update()'s own returned command
// through runBatch — it batches in expireToastAfter(4s), a real
// tea.Tick that would block this test for 4 real seconds if invoked
// synchronously. The diff-refetch half is exercised directly against
// radarView.applyReviewErr instead, which is the exact sub-command Update
// batches in.
func TestReviewErrMsgConflictRevertsAndRefetchesDiffWithToast(t *testing.T) {
	api := &fakeAPI{diff: model.Diff{WorktreeID: "a1", Hash: "h2", Files: []model.DiffFile{{Path: "a.go", Hash: "ha2"}}}}
	m := reviewFixture(api)
	m.radar.pane.setReviewed("a.go", true) // the optimistic flip that's about to be told it lost the race

	errMsg := reviewErrMsg{ID: "a1", File: "a.go", Conflict: true, Err: errors.New("file changed since it was viewed: stale hash")}
	updated, cmd := m.Update(errMsg)
	got := updated.(appModel)
	if got.radar.pane.diff.Reviewed["a.go"] {
		t.Error("a 409 conflict must revert the optimistic toggle")
	}
	if !strings.Contains(got.toast, "a.go") || !strings.Contains(got.toast, "changed since you viewed it") {
		t.Errorf("toast = %q, want it to name the file and explain the conflict", got.toast)
	}
	if cmd == nil {
		t.Fatal("expected a non-nil batched command (diff refetch + toast expiry)")
	}

	diffCmd := m.radar.applyReviewErr(m.ctx, api, errMsg)
	if diffCmd == nil {
		t.Fatal("expected the conflict to trigger a diff refetch")
	}
	if dm, ok := diffCmd().(diffMsg); !ok || dm.ID != "a1" {
		t.Errorf("applyReviewErr's command = %#v, want a diffMsg for a1", dm)
	}
}

// TestReviewErrMsgNonConflictRevertsWithoutForcingRefetch covers a plain
// failure (e.g. daemon unreachable): still reverts, still toasts, but
// doesn't need the extra diff round trip a content-changed conflict does.
func TestReviewErrMsgNonConflictRevertsWithoutForcingRefetch(t *testing.T) {
	m := reviewFixture(&fakeAPI{})
	m.radar.pane.setReviewed("a.go", true)

	updated, _ := m.Update(reviewErrMsg{ID: "a1", File: "a.go", Conflict: false, Err: errors.New("boom")})
	got := updated.(appModel)
	if got.radar.pane.diff.Reviewed["a.go"] {
		t.Error("any SetReviewed failure should revert the optimistic toggle")
	}
	if got.toast != "boom" {
		t.Errorf("toast = %q, want the plain error message", got.toast)
	}
}

// ---- Review j/k (file nav) vs arrows (line scroll) ----

func TestHandleReviewKeyJKWalkFilesNotLines(t *testing.T) {
	m := reviewFixture(&fakeAPI{})
	before := m.radar.pane.offset

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	got := updated.(appModel)
	if got.radar.pane.offset == before {
		t.Error("j in Review should move to the next file (offset should change)")
	}
	if got.radar.pane.offset != got.radar.pane.fileOffsets[1] {
		t.Errorf("offset = %d, want the second file's own header offset %d", got.radar.pane.offset, got.radar.pane.fileOffsets[1])
	}
}

func TestHandleReviewKeyArrowDownScrollsLinesNotFiles(t *testing.T) {
	m := reviewFixture(&fakeAPI{})
	m.radar.pane.setDiff(manyLineDiff(50))
	m.radar.pane.setHeight(10)

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if got := updated.(appModel); got.radar.pane.offset != 1 {
		t.Errorf("pane offset = %d, want 1 (arrow-down scrolls one line in Review)", got.radar.pane.offset)
	}
}

// ---- review.changed SSE -> whole-list refetch ----

// TestApplyEventReviewChangedTriggersListRefetch pins §2.4: the event
// carries no counts, so the aggregate Reviewed/unreviewed numbers can only
// come from refetching /api/worktrees.
func TestApplyEventReviewChangedTriggersListRefetch(t *testing.T) {
	api := &fakeAPI{worktrees: []model.Worktree{{ID: "a1", Reviewed: 1}}}
	m := newTestModel(api)
	cmd := m.applyEvent(model.Event{Type: model.EventReviewChanged, ID: "a1"})
	if cmd == nil {
		t.Fatal("review.changed must return a non-nil command")
	}
	msg := cmd()
	lm, ok := msg.(listMsg)
	if !ok || len(lm) != 1 || lm[0].Reviewed != 1 {
		t.Fatalf("cmd() = %#v, want a listMsg reflecting the refreshed aggregate", msg)
	}
}

// ---- filtering (/ and f) via handleKey ----

func TestHandleKeySearchOpensInputAndTypedRunesFilterLive(t *testing.T) {
	m := newTestModel(&fakeAPI{})
	m.sidebar.setWorktrees([]model.Worktree{
		{ID: "a1", Repo: "alpha", Name: "one"},
		{ID: "z1", Repo: "zeta", Name: "two"},
	})

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	got := updated.(appModel)
	if !got.sidebar.searching() {
		t.Fatal("/ should open the search input")
	}

	updated, _ = got.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("z")})
	got = updated.(appModel)
	if sel, ok := got.sidebar.selected(); !ok || sel.ID != "z1" {
		t.Errorf("typing \"z\" should live-filter down to zeta's z1, got selected=%+v ok=%v", sel, ok)
	}
}

func TestHandleKeySearchQIsInertButCtrlCStillQuitsWhileSearching(t *testing.T) {
	m := newTestModel(&fakeAPI{})
	m.sidebar.startSearch()

	// Not using isQuitCmd here: textinput.Update's returned Cmd (cursor
	// blink) is a bare tea.Tick, not wrapped in tea.Batch — calling it
	// synchronously would block for the real blink interval. The model
	// state below already proves "q" was fed to the input, not treated as
	// quit (a real quit would leave the value untouched).
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if got := updated.(appModel); got.sidebar.filterInput.Value() != "q" {
		t.Errorf("filterInput value = %q, want the literal \"q\" to have been typed", got.sidebar.filterInput.Value())
	}

	_, cmd2 := updated.(appModel).Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if !isQuitCmd(cmd2) {
		t.Error("ctrl+c must still quit even while searching")
	}
}

func TestHandleKeySearchEnterCommitsAndEscClears(t *testing.T) {
	m := newTestModel(&fakeAPI{})
	m.sidebar.setWorktrees([]model.Worktree{{ID: "a1", Repo: "alpha", Name: "one"}})
	m.sidebar.startSearch()
	m.sidebar.filterInput.SetValue("alpha")

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got := updated.(appModel)
	if got.sidebar.searching() {
		t.Error("enter should commit (exit input mode)")
	}
	if got.sidebar.filterInput.Value() != "alpha" {
		t.Error("enter should keep the committed query applied")
	}

	got.sidebar.startSearch()
	updated2, _ := got.Update(tea.KeyMsg{Type: tea.KeyEscape})
	got2 := updated2.(appModel)
	if got2.sidebar.searching() || got2.sidebar.filterInput.Value() != "" {
		t.Errorf("esc while searching should clear the query, got value=%q searching=%v", got2.sidebar.filterInput.Value(), got2.sidebar.searching())
	}
}

func TestHandleKeyFilterActiveTogglesToActiveOnly(t *testing.T) {
	m := newTestModel(&fakeAPI{})
	m.sidebar.setWorktrees([]model.Worktree{
		{ID: "a1", Repo: "alpha", Name: "one", State: model.StateActive},
		{ID: "a2", Repo: "alpha", Name: "two", State: model.StateIdle},
	})

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("f")})
	got := updated.(appModel)
	if !got.sidebar.activeOnly {
		t.Fatal("f should toggle active-only on")
	}
	if sel, ok := got.sidebar.selected(); !ok || sel.ID != "a1" {
		t.Errorf("selected = %+v ok=%v, want a1 (the only active worktree)", sel, ok)
	}

	updated2, _ := got.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("f")})
	if got2 := updated2.(appModel); got2.sidebar.activeOnly {
		t.Error("pressing f again should toggle active-only back off")
	}
}

// TestEscClearsActiveFilterBeforeAnythingElse pins the esc ladder's new
// rung: with a filter applied (but not currently typing), esc clears it
// rather than being a no-op.
func TestEscClearsActiveFilterBeforeAnythingElse(t *testing.T) {
	m := newTestModel(&fakeAPI{})
	m.sidebar.setWorktrees([]model.Worktree{{ID: "a1", Repo: "alpha", Name: "one"}})
	m.sidebar.activeOnly = true

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEscape})
	if got := updated.(appModel); got.sidebar.activeOnly {
		t.Error("esc should clear an active filter")
	}
}

// ---- tmux jump (t) ----

func TestHandleKeyTmuxJumpDispatchesCommandForSelectedWorktreePath(t *testing.T) {
	m := newTestModel(&fakeAPI{})
	m.sidebar.setWorktrees([]model.Worktree{{ID: "a1", Repo: "api", Name: "feature", Path: "/repo/wt"}})

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("t")})
	if cmd == nil {
		t.Fatal("t with a worktree selected should dispatch a tmux command")
	}
	msg, ok := cmd().(tmuxDoneMsg)
	if !ok {
		t.Fatalf("cmd() = %#v, want a tmuxDoneMsg", msg)
	}
}

func TestHandleKeyTmuxJumpNoOpWhenNothingSelected(t *testing.T) {
	m := newTestModel(&fakeAPI{})
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("t")})
	if cmd != nil {
		t.Error("t with nothing selected should not dispatch a command")
	}
}

func TestTmuxDoneMsgWithErrSetsToast(t *testing.T) {
	m := newTestModel(&fakeAPI{})
	updated, cmd := m.Update(tmuxDoneMsg{Err: errors.New("not inside tmux")})
	if got := updated.(appModel); got.toast != "not inside tmux" {
		t.Errorf("toast = %q, want the tmux error surfaced", got.toast)
	}
	if cmd == nil {
		t.Error("expected the toast-expiry command to be armed")
	}
}

func TestTmuxDoneMsgWithoutErrIsSilent(t *testing.T) {
	m := newTestModel(&fakeAPI{})
	m.toast = "" // precondition
	updated, cmd := m.Update(tmuxDoneMsg{})
	if got := updated.(appModel); got.toast != "" {
		t.Errorf("toast = %q, want empty on a successful jump", got.toast)
	}
	if cmd != nil {
		t.Error("a successful jump has nothing to toast, want a nil command")
	}
}

// ---- smoke: a real tea.Program, a fake client, fixtures ----

// TestRunSmokeStartsRendersFixturesAndQuitsCleanly drives an actual
// tea.Program (P3-design.md WP1's "one teatest smoke"), piping its own
// input/output rather than using a real TTY: Init dispatches against a fake
// client seeded with one worktree, the rendered frame names it and the
// brand, and sending "q" exits with a nil error.
func TestRunSmokeStartsRendersFixturesAndQuitsCleanly(t *testing.T) {
	api := &fakeAPI{
		protocol:  model.ProtocolVersion,
		version:   "v0.3.0-test",
		worktrees: []model.Worktree{{ID: "a1", Repo: "api-server", Name: "feature-x", Base: "main"}},
		events:    make(chan model.Event),
		errs:      make(chan error, 1),
	}

	inR, _ := io.Pipe() // never written to; the program is driven via p.Send below
	var outBuf safeBuffer
	done := make(chan error, 1)
	go func() {
		done <- runProgram("/tmp/does-not-matter.sock", api, func(p *tea.Program) {
			// A piped, non-TTY output has no real terminal to auto-detect a
			// size from (bubbletea's checkResize no-ops without one), so the
			// model would otherwise stay stuck at 0x0 forever.
			p.Send(tea.WindowSizeMsg{Width: 100, Height: 40})
			time.Sleep(150 * time.Millisecond) // let Init's fetches land and a frame render
			p.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
		},
			tea.WithInput(inR),
			tea.WithOutput(&outBuf),
			tea.WithoutSignalHandler(),
			tea.WithoutCatchPanics(),
		)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("run() returned %v, want nil after a plain q quit", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("program did not exit within 5s of sending q")
	}

	frame := stripANSI(outBuf.String())
	if !strings.Contains(frame, "wt cockpit") {
		t.Errorf("rendered output does not contain the brand %q:\n%s", "wt cockpit", frame)
	}
	if !strings.Contains(frame, "feature-x") {
		t.Errorf("rendered output does not contain the fixture worktree name %q:\n%s", "feature-x", frame)
	}
}

// TestRunRadarDiffPaneMatchesGoldenFrame is WP2's "teatest golden frame of a
// small fixed diff at 80×24 ascii profile" (P3-design.md's WP2 test list):
// drive a real tea.Program against a fake client seeded with a small
// two-file diff, focus the diff pane with ⏎, and byte-compare the rendered
// frame against a checked-in golden. Unlike the real-terminal expect/pty
// smoke (P3-design.md §5, which explicitly warns full-frame goldens there
// are flaky on escape-sequence timing), this harness is a plain buffer with
// no real tty involved — same deterministic fixtures in, same bytes out
// every run — so an exact comparison is the right bar here, not just
// substrings. Update the golden with `go test -run TestRunRadarDiffPane
// -update` after an intentional layout change.
func TestRunRadarDiffPaneMatchesGoldenFrame(t *testing.T) {
	d := model.Diff{WorktreeID: "auth", Base: "main", Files: []model.DiffFile{
		{
			Path: "internal/auth/token.go", Status: model.FileModified, Hash: "hgo",
			Stats: model.Stats{Add: 2, Del: 1},
			Hunks: []model.Hunk{{
				Header: "@@ -18,3 +18,4 @@ func NewToken(",
				Lines: []model.Line{
					line(model.LineContext, 18, 18, "func NewToken(uid string) (*Token, error) {"),
					line(model.LineDel, 19, 0, "\texp := time.Now().Add(15 * time.Minute)"),
					line(model.LineAdd, 0, 19, "\texp := time.Now().Add(30 * time.Minute)"),
					line(model.LineContext, 20, 20, "\treturn sign(claims)"),
				},
			}},
		},
		{
			Path: "README.md", Status: model.FileAdded, Hash: "hmd",
			Stats: model.Stats{Add: 2},
			Hunks: []model.Hunk{{
				Header: "@@ -0,0 +1,2 @@",
				Lines: []model.Line{
					line(model.LineAdd, 0, 1, "# wt cockpit"),
					line(model.LineAdd, 0, 2, ""),
				},
			}},
		},
	}}
	api := &fakeAPI{
		protocol:  model.ProtocolVersion,
		version:   "v0.3.0-test",
		worktrees: []model.Worktree{{ID: "auth", Repo: "api-server", Name: "auth-refactor", Base: "main", Stats: model.Stats{Files: 2, Add: 4, Del: 1}}},
		diff:      d,
		events:    make(chan model.Event),
		errs:      make(chan error, 1),
	}

	inR, _ := io.Pipe()
	var outBuf safeBuffer
	done := make(chan error, 1)
	go func() {
		done <- runProgram("/tmp/does-not-matter.sock", api, func(p *tea.Program) {
			p.Send(tea.WindowSizeMsg{Width: 80, Height: 24})
			time.Sleep(200 * time.Millisecond) // let Init's fetches + the diff fetch + async highlight land
			p.Send(tea.KeyMsg{Type: tea.KeyEnter})
			time.Sleep(150 * time.Millisecond)
			p.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
		},
			tea.WithInput(inR),
			tea.WithOutput(&outBuf),
			tea.WithoutSignalHandler(),
			tea.WithoutCatchPanics(),
		)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run() returned %v, want nil after a plain q quit", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("program did not exit within 5s of sending q")
	}

	frame := normalizeFrameForGolden(outBuf.String())
	const goldenPath = "testdata/radar_frame.golden"
	if *updateGolden {
		if err := os.WriteFile(goldenPath, []byte(frame), 0o644); err != nil {
			t.Fatalf("writing golden file %s: %v", goldenPath, err)
		}
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("reading golden file %s: %v", goldenPath, err)
	}
	if frame != string(want) {
		t.Errorf("rendered frame does not match %s.\ngot:\n%s\nwant:\n%s", goldenPath, frame, string(want))
	}
}

// TestRunReviewFlowMarkFileBackQuitCleanly is WP3's teatest flow (P3-design.md
// WP3 test list): radar -> `r` -> mark a file reviewed with `space` -> `esc`
// back to radar -> `q` quits clean. Drives a real tea.Program against a fake
// client, same harness as the WP1/WP2 smokes above.
func TestRunReviewFlowMarkFileBackQuitCleanly(t *testing.T) {
	d := model.Diff{WorktreeID: "auth", Base: "main", Files: []model.DiffFile{
		{
			Path: "internal/auth/token.go", Status: model.FileModified, Hash: "hgo",
			Stats: model.Stats{Add: 2, Del: 1},
			Hunks: []model.Hunk{{
				Header: "@@ -18,3 +18,4 @@ func NewToken(",
				Lines: []model.Line{
					line(model.LineContext, 18, 18, "func NewToken(uid string) (*Token, error) {"),
					line(model.LineDel, 19, 0, "\texp := time.Now().Add(15 * time.Minute)"),
					line(model.LineAdd, 0, 19, "\texp := time.Now().Add(30 * time.Minute)"),
				},
			}},
		},
	}}
	api := &fakeAPI{
		protocol:  model.ProtocolVersion,
		version:   "v0.3.0-test",
		worktrees: []model.Worktree{{ID: "auth", Repo: "api-server", Name: "auth-refactor", Base: "main", Stats: model.Stats{Files: 1, Add: 2, Del: 1}}},
		diff:      d,
		events:    make(chan model.Event),
		errs:      make(chan error, 1),
	}

	inR, _ := io.Pipe()
	var outBuf safeBuffer
	done := make(chan error, 1)
	go func() {
		done <- runProgram("/tmp/does-not-matter.sock", api, func(p *tea.Program) {
			p.Send(tea.WindowSizeMsg{Width: 100, Height: 30})
			time.Sleep(200 * time.Millisecond) // let Init's fetches + the diff fetch land
			p.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
			time.Sleep(150 * time.Millisecond)
			p.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(" ")}) // mark the file reviewed
			time.Sleep(150 * time.Millisecond)
			p.Send(tea.KeyMsg{Type: tea.KeyEscape}) // back to radar
			time.Sleep(100 * time.Millisecond)
			p.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
		},
			tea.WithInput(inR),
			tea.WithOutput(&outBuf),
			tea.WithoutSignalHandler(),
			tea.WithoutCatchPanics(),
		)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run() returned %v, want nil after a plain q quit", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("program did not exit within 5s of sending q")
	}

	frame := stripANSI(outBuf.String())
	if !strings.Contains(frame, "wt cockpit") {
		t.Errorf("rendered output does not contain the brand:\n%s", frame)
	}
	if !strings.Contains(frame, "auth-refactor") {
		t.Errorf("rendered output does not contain the fixture worktree name:\n%s", frame)
	}
	if len(api.setReviewedCalls) != 1 {
		t.Fatalf("SetReviewed calls = %v, want exactly 1 (space marked the file reviewed)", api.setReviewedCalls)
	}
	if call := api.setReviewedCalls[0]; call.id != "auth" || call.file != "internal/auth/token.go" || !call.reviewed {
		t.Errorf("SetReviewed call = %+v, want auth/internal/auth/token.go/true", call)
	}
}

// normalizeFrameForGolden strips ANSI (already-forced-Ascii-profile, so this
// is mostly cursor/screen control codes) and collapses bubbletea's "\r\n"/
// stray "\r" line-ending artifacts down to plain "\n" — an incidental detail
// of how the renderer paints a full frame, not part of what this test means
// to pin (the diff pane's actual content and column alignment).
func normalizeFrameForGolden(s string) string {
	s = stripANSI(s)
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "")
	return s
}

// safeBuffer is a concurrency-safe io.Writer: the Program renders on its own
// goroutine while the test still holds a reference to read the buffer back
// after Run returns.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// stripANSI removes escape sequences (color, cursor movement, alt-screen)
// so a substring check on rendered content isn't at the mercy of exactly
// where the renderer's diffing algorithm split lines.
func stripANSI(s string) string {
	var b strings.Builder
	inEsc := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inEsc {
			// A CSI/OSC sequence ends at its final byte (letter or ~); keep
			// consuming until then.
			if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '~' {
				inEsc = false
			}
			continue
		}
		if c == 0x1b {
			inEsc = true
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}
