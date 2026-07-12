package tui

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/navbytes/wt-cockpit/internal/client"
	"github.com/navbytes/wt-cockpit/internal/model"
)

// newTestModel builds a bare appModel for pure Update()/View() table tests —
// no conn.go goroutine involved (connCh stays nil; nothing in these tests
// invokes the Cmd that would block reading it).
func newTestModel(api apiClient) appModel {
	return appModel{api: api, ctx: context.Background(), retryCh: make(chan struct{}, 1)}
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
