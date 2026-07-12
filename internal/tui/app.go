// Package tui is the Bubble Tea program: a thin client exactly like cmd/wt —
// it speaks only through internal/client (HTTP+SSE over wtd's Unix socket),
// holding zero git logic and importing nothing from internal/engine or
// below (docs/02-stack-decision.md's constitution).
package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/navbytes/wt-cockpit/internal/client"
	"github.com/navbytes/wt-cockpit/internal/model"
)

// Terminal-size thresholds (P3-design.md §1.2). Only the min-size guard is
// wired in WP1 — the 80/110-col breakpoints govern hiding the sidebar/rail,
// which don't exist before WP2's diff pane, so there's nothing yet for them
// to toggle.
const (
	minWidth  = 40
	minHeight = 10
)

const sidebarWidth = 34

// screenID selects which of the two views (P3-design.md §1.1) app.go routes
// to. Radar's real composition (radar.go) landed in WP2; Review's rail
// (review.go) is still WP3's stub.
type screenID int

const (
	screenRadar screenID = iota
	screenReview
)

// apiClient is every internal/client.Client method the TUI needs — a
// structural mirror of P3-design.md §4's frozen client contract, declared
// here (not in internal/client) purely so tests can inject a fake client
// feeding fixtures instead of dialing a real daemon.
type apiClient interface {
	Version(ctx context.Context) (protocol int, version string, err error)
	Worktrees(ctx context.Context) ([]model.Worktree, error)
	Diff(ctx context.Context, id string) (model.Diff, error)
	SetReviewed(ctx context.Context, id, file string, reviewed bool, hash string) error
	Approve(ctx context.Context, id string) (model.ApproveResult, error)
	Refresh(ctx context.Context) error
	Events(ctx context.Context) (<-chan model.Event, <-chan error)
}

var _ apiClient = (*client.Client)(nil)

// eventTypeSnapshot is the synthetic event wtd sends on every SSE (re)connect
// (cmd/wtd's handleEvents), ahead of any real bus event. It has no
// model.EventType const of its own (model.go's consts are the *engine's*
// business events); §2.4 names it as the resync trigger: reconnect cannot
// miss state because connect always snapshots.
const eventTypeSnapshot model.EventType = "snapshot"

// appModel is the one root Elm-architecture model: shared state plus a
// screen selector, per P3-design.md §2.3 ("one appModel owning shared state
// and two lightweight view structs" — radar.go's radarView is the first of
// those; review.go's reviewView is still WP3).
type appModel struct {
	api    apiClient
	ctx    context.Context
	socket string // display only (the down-card names it)

	width, height int
	screen        screenID

	conn        connState
	connAttempt int
	connWait    time.Duration
	connCh      <-chan connEvent
	retryCh     chan struct{}

	fatal   string // non-empty => fatal full-screen card; any key quits non-zero
	exitErr error

	sidebar     sidebar
	radar       radarView
	review      reviewView
	approve     approveModal
	diffFocused bool // true once ⏎ has focused the diff pane (scroll keys act on it)
	toast       string
}

// Run resolves the real client and runs the program until the user quits.
// A non-nil error means a fatal condition (protocol mismatch, or a genuine
// handshake refusal) drove the exit — cmd/wt reports it via fatal() and a
// nonzero process exit, matching every other daemon-touching command.
func Run(socket string) error {
	return run(socket, client.New(socket), tea.WithAltScreen())
}

// run is Run's testable core: api is the (possibly fake) client and opts
// lets tests swap in WithInput/WithOutput instead of a real TTY.
func run(socket string, api apiClient, opts ...tea.ProgramOption) error {
	return runProgram(socket, api, nil, opts...)
}

// runProgram is run's fully testable core. ready, if non-nil, runs in its
// own goroutine once the Program exists but before it (necessarily)
// blocks in Run — tests use it to drive the program via p.Send, e.g. an
// initial tea.WindowSizeMsg: a piped, non-TTY output (tea.WithOutput in a
// test) has no real terminal to query a size from at all (bubbletea's own
// checkResize is a no-op without a TTY), so nothing else would ever size
// the model.
func runProgram(socket string, api apiClient, ready func(*tea.Program), opts ...tea.ProgramOption) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m := appModel{api: api, ctx: ctx, socket: socket, retryCh: make(chan struct{}, 1), radar: newRadarView()}
	p := tea.NewProgram(m, opts...)
	if ready != nil {
		go ready(p)
	}
	final, err := p.Run()
	if err != nil {
		return err
	}
	if fm, ok := final.(appModel); ok && fm.exitErr != nil {
		return fm.exitErr
	}
	return nil
}

func (m appModel) Init() tea.Cmd {
	return tea.Batch(
		fetchVersion(m.ctx, m.api),
		fetchList(m.ctx, m.api),
		startConnCmd(m.ctx, m.api, m.retryCh),
		tick(),
	)
}

func (m appModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, m.radar.ensureHighlightsCmd() // resize can bring a new file card into view

	case tea.KeyMsg:
		return m.handleKey(msg)

	case connStartedMsg:
		m.connCh = msg.ch
		return m, waitForConnEvent(m.connCh)

	case connMsg:
		m.conn = msg.State
		m.connAttempt = msg.Attempt
		m.connWait = msg.Wait
		return m, waitForConnEvent(m.connCh)

	case eventMsg:
		cmd := m.applyEvent(model.Event(msg))
		return m, tea.Batch(cmd, waitForConnEvent(m.connCh))

	case versionMsg:
		if msg.Protocol != model.ProtocolVersion {
			m.fatal = fmt.Sprintf(
				"protocol mismatch: wt speaks %d, wtd speaks %d — rebuild both from the same checkout and restart wtd/wt",
				model.ProtocolVersion, msg.Protocol)
		}
		return m, nil

	case versionErrMsg:
		// A connection-level failure is already reflected via the conn state
		// machine (down/reconnecting) — nothing extra to do. Anything else
		// means wtd answered but refused the handshake outright (pre-v0.2, or
		// a malformed body): fatal, same as a protocol-number mismatch.
		if !isUnreachable(msg.Err) {
			m.fatal = fmt.Sprintf("wtd refused the protocol handshake: %v", msg.Err)
		}
		return m, nil

	case listMsg:
		m.sidebar.setWorktrees([]model.Worktree(msg))
		return m, m.ensureDiffForSelection()

	case diffMsg:
		m.radar.applyDiffMsg(msg)
		return m, m.radar.ensureHighlightsCmd()

	case diffErrMsg:
		m.radar.applyDiffErrMsg(msg)
		return m, nil

	case highlightedMsg:
		m.radar.applyHighlighted(msg)
		return m, nil

	case reviewOKMsg:
		m.radar.applyReviewOK(msg)
		return m, nil

	case reviewErrMsg:
		cmd := m.radar.applyReviewErr(m.ctx, m.api, msg)
		if msg.Conflict {
			m.toast = fmt.Sprintf("%s changed since you viewed it — diff refreshed", msg.File)
		} else {
			m.toast = msg.Err.Error()
		}
		return m, tea.Batch(cmd, expireToastAfter(4*time.Second))

	case approveOKMsg:
		m.approve = approveModal{}
		m.screen = screenRadar
		m.toast = fmt.Sprintf("✓ merged %s → %s, worktree removed", msg.Res.Merged, msg.Res.Into)
		return m, expireToastAfter(4 * time.Second)

	case approveErrMsg:
		m = m.applyApproveErr(msg)
		return m, nil

	case tmuxDoneMsg:
		if msg.Err != nil {
			m.toast = msg.Err.Error()
			return m, expireToastAfter(4 * time.Second)
		}
		return m, nil

	case listErrMsg:
		if !isUnreachable(msg.Err) {
			m.toast = msg.Err.Error()
			return m, expireToastAfter(4 * time.Second)
		}
		return m, nil

	case refreshDoneMsg:
		if msg.Err != nil {
			m.toast = msg.Err.Error()
		} else {
			m.toast = "refreshed"
		}
		return m, expireToastAfter(4 * time.Second)

	case toastExpiredMsg:
		m.toast = ""
		return m, nil

	case tickMsg:
		return m, tick()
	}
	return m, nil
}

// handleKey is Update's tea.KeyMsg branch. When a fatal condition is showing,
// every key exits non-zero (P3-design.md §1.4). The approve modal and the
// sidebar's search input each capture the keyboard entirely while active
// (checked first, in that order — search can only be entered from Radar, so
// the two never overlap). A few keys act the same regardless of screen/focus
// (quit, refresh, tmux jump, approve); Review routes to its own handler;
// everything else in Radar routes on m.diffFocused, WP2's focus model (⏎
// focuses the diff pane so scroll keys act on it; esc un-focuses before
// falling back to view routing, but clears an active sidebar filter first if
// one is set).
func (m appModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.fatal != "" {
		m.exitErr = errors.New(m.fatal)
		return m, tea.Quit
	}
	if m.approve.open {
		return m.handleApproveKey(msg)
	}
	if m.sidebar.searching() {
		return m.handleSearchKey(msg)
	}
	switch {
	case key.Matches(msg, keys.Quit):
		return m, tea.Quit
	case key.Matches(msg, keys.Refresh):
		// Also skip an in-progress reconnect backoff wait (the down-card's
		// "R retry now") — a non-blocking send since retryCh is 1-buffered
		// and only ever needs to wake the loop once.
		select {
		case m.retryCh <- struct{}{}:
		default:
		}
		return m, refreshCmd(m.ctx, m.api)
	case key.Matches(msg, keys.TmuxJump):
		if w, ok := m.sidebar.selected(); ok {
			return m, tmuxJumpCmd(w.Path)
		}
		return m, nil
	case key.Matches(msg, keys.Approve):
		return m.openApprove()
	}

	if m.screen == screenReview {
		return m.handleReviewKey(msg)
	}

	switch {
	case key.Matches(msg, keys.Review):
		if _, ok := m.sidebar.selected(); ok {
			m.screen = screenReview
			m.diffFocused = false
		}
		return m, nil
	case key.Matches(msg, keys.Search):
		m.sidebar.startSearch()
		m.diffFocused = false
		return m, nil
	case key.Matches(msg, keys.FilterActive):
		m.sidebar.activeOnly = !m.sidebar.activeOnly
		m.sidebar.fixSelection()
		return m, m.ensureDiffForSelection()
	}

	if m.diffFocused {
		return m.handleDiffKey(msg)
	}
	switch {
	case key.Matches(msg, keys.Up):
		m.sidebar.moveUp()
		return m, m.ensureDiffForSelection()
	case key.Matches(msg, keys.Down):
		m.sidebar.moveDown()
		return m, m.ensureDiffForSelection()
	case key.Matches(msg, keys.Enter):
		if _, ok := m.sidebar.selected(); ok && m.screen == screenRadar {
			m.diffFocused = true
			return m, m.radar.ensureHighlightsCmd()
		}
	case key.Matches(msg, keys.Esc):
		if m.sidebar.hasFilter() {
			m.sidebar.clearFilter()
			return m, m.ensureDiffForSelection()
		}
		m.screen = screenRadar
	}
	return m, nil
}

// handleSearchKey routes keys while the sidebar's search input has focus
// (P3-design.md §1.3's "/"): ⏎ commits the query (stays filtered, exits
// input mode), esc clears it entirely, ctrl+c still quits; every other
// key — including the literal rune "q", normally Quit — goes to the text
// input itself ("q inert while search input is focused").
func (m appModel) handleSearchKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "enter":
		m.sidebar.commitSearch()
		return m, m.ensureDiffForSelection()
	case "esc":
		m.sidebar.cancelSearch()
		return m, m.ensureDiffForSelection()
	}
	cmd := m.sidebar.updateSearchInput(msg)
	return m, tea.Batch(cmd, m.ensureDiffForSelection())
}

// handleReviewKey routes tea.KeyMsg while m.screen == screenReview
// (P3-design.md §1.3's Review keybindings): j/k walk files — reusing the
// same PrevFile/NextFile the diff pane already exposes for Radar's `[`/`]`
// — space optimistically toggles the file under the cursor, esc returns to
// Radar, and the remaining keys fine-scroll the same shared pane Radar
// uses. j/k are matched on the raw key string rather than key.Matches,
// since keys.Up/Down bind those same runes to line-scrolling for Radar's
// diff-focused mode — Review needs the two split apart.
func (m appModel) handleReviewKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "j":
		m.radar.pane.NextFile()
		return m, m.radar.ensureHighlightsCmd()
	case "k":
		m.radar.pane.PrevFile()
		return m, m.radar.ensureHighlightsCmd()
	}
	switch {
	case key.Matches(msg, keys.Esc):
		m.screen = screenRadar
		return m, nil
	case key.Matches(msg, keys.ToggleReview):
		return m.toggleReviewedCurrentFile()
	case key.Matches(msg, keys.Up):
		m.radar.pane.LineUp()
	case key.Matches(msg, keys.Down):
		m.radar.pane.LineDown()
	case key.Matches(msg, keys.HalfPageDown):
		m.radar.pane.HalfPageDown()
	case key.Matches(msg, keys.HalfPageUp):
		m.radar.pane.HalfPageUp()
	case key.Matches(msg, keys.PageDown):
		m.radar.pane.PageDown()
	case key.Matches(msg, keys.PageUp):
		m.radar.pane.PageUp()
	case key.Matches(msg, keys.Top):
		m.radar.pane.Top()
	case key.Matches(msg, keys.Bottom):
		m.radar.pane.Bottom()
	}
	return m, m.radar.ensureHighlightsCmd()
}

// toggleReviewedCurrentFile optimistically flips the file under the diff
// pane's cursor and fires the request carrying the currently-displayed hash
// (P3-design.md §1.5's conflict contract: a 409 means the file changed
// since it was viewed). No-ops if nothing is selected or the diff has no
// files under the cursor (e.g. still loading).
func (m appModel) toggleReviewedCurrentFile() (tea.Model, tea.Cmd) {
	w, ok := m.sidebar.selected()
	if !ok {
		return m, nil
	}
	f, ok := m.radar.pane.currentFile()
	if !ok {
		return m, nil
	}
	next := !m.radar.pane.diff.Reviewed[f.Path]
	m.radar.pane.setReviewed(f.Path, next)
	return m, setReviewedCmd(m.ctx, m.api, w.ID, f.Path, next, f.Hash)
}

func setReviewedCmd(ctx context.Context, api apiClient, id, file string, reviewed bool, hash string) tea.Cmd {
	return func() tea.Msg {
		reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		err := api.SetReviewed(reqCtx, id, file, reviewed, hash)
		if err == nil {
			return reviewOKMsg{ID: id, File: file, Reviewed: reviewed}
		}
		var conflict *client.ConflictError
		return reviewErrMsg{ID: id, File: file, Conflict: errors.As(err, &conflict), Err: err}
	}
}

// handleDiffKey is P3-design.md §1.3's diff-pane scrolling set, active only
// while m.diffFocused (Radar view; Review's own rail/file-nav is WP3).
func (m appModel) handleDiffKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, keys.Esc):
		m.diffFocused = false
		return m, nil
	case key.Matches(msg, keys.Up):
		m.radar.pane.LineUp()
	case key.Matches(msg, keys.Down):
		m.radar.pane.LineDown()
	case key.Matches(msg, keys.HalfPageDown):
		m.radar.pane.HalfPageDown()
	case key.Matches(msg, keys.HalfPageUp):
		m.radar.pane.HalfPageUp()
	case key.Matches(msg, keys.PageDown):
		m.radar.pane.PageDown()
	case key.Matches(msg, keys.PageUp):
		m.radar.pane.PageUp()
	case key.Matches(msg, keys.Top):
		m.radar.pane.Top()
	case key.Matches(msg, keys.Bottom):
		m.radar.pane.Bottom()
	case key.Matches(msg, keys.PrevFile):
		m.radar.pane.PrevFile()
	case key.Matches(msg, keys.NextFile):
		m.radar.pane.NextFile()
	case key.Matches(msg, keys.ToggleFold):
		m.radar.pane.ToggleFold()
	}
	return m, m.radar.ensureHighlightsCmd()
}

// ensureDiffForSelection keeps the Radar diff pane in sync with the sidebar:
// called after anything that can change which worktree is selected. A no-op
// when nothing is selected (empty workspace).
func (m *appModel) ensureDiffForSelection() tea.Cmd {
	w, ok := m.sidebar.selected()
	if !ok {
		return nil
	}
	fetch := m.radar.ensureDiff(m.ctx, m.api, w.ID)
	return tea.Batch(fetch, m.radar.ensureHighlightsCmd())
}

// applyEvent applies one SSE delta to the shared model (P3-design.md §2.4):
// upsert/remove update the sidebar in place (and, since either can change
// which worktree is selected, re-check the diff pane); a snapshot (sent on
// every (re)connect) re-runs the startup fetches — the resync story:
// reconnect cannot miss state because connect always snapshots. diff.ready
// is the diff pane's own hash-gated refetch (radar.go). review.changed
// refetches the whole worktree list — it's the only source of the aggregate
// Reviewed/Stats.Files counts the topbar and approve modal show, and the
// event itself carries no counts. guardrail.tripped and any future type are
// forward-compat-ignored (§2.8).
func (m *appModel) applyEvent(e model.Event) tea.Cmd {
	switch e.Type {
	case model.EventWorktreeUpserted:
		if e.Worktree != nil {
			m.sidebar.upsert(*e.Worktree)
		}
	case model.EventWorktreeRemoved:
		m.sidebar.remove(e.ID)
	case model.EventDiffReady:
		return m.radar.applyDiffReady(m.ctx, m.api, e)
	case model.EventReviewChanged:
		return fetchList(m.ctx, m.api)
	case eventTypeSnapshot:
		return tea.Batch(fetchVersion(m.ctx, m.api), fetchList(m.ctx, m.api))
	}
	return m.ensureDiffForSelection()
}

func isUnreachable(err error) bool {
	var unreachable *client.UnreachableError
	return errors.As(err, &unreachable)
}

// ---- tea.Cmd factories ----

func fetchVersion(ctx context.Context, api apiClient) tea.Cmd {
	return func() tea.Msg {
		reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		protocol, version, err := api.Version(reqCtx)
		if err != nil {
			return versionErrMsg{Err: err}
		}
		return versionMsg{Protocol: protocol, Version: version}
	}
}

func fetchList(ctx context.Context, api apiClient) tea.Cmd {
	return func() tea.Msg {
		reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		wts, err := api.Worktrees(reqCtx)
		if err != nil {
			return listErrMsg{Err: err}
		}
		return listMsg(wts)
	}
}

func refreshCmd(ctx context.Context, api apiClient) tea.Cmd {
	return func() tea.Msg {
		reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		return refreshDoneMsg{Err: api.Refresh(reqCtx)}
	}
}

// tick drives the sidebar's relative-time ("3s ago") refresh.
func tick() tea.Cmd {
	return tea.Tick(5*time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func expireToastAfter(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(time.Time) tea.Msg { return toastExpiredMsg{} })
}

// ---- View ----

func (m appModel) View() string {
	switch {
	case m.width == 0 && m.height == 0:
		// First frame, rendered before Bubble Tea's own initial
		// tea.WindowSizeMsg arrives (P3-design.md §1.4's "loading" state: the
		// shell paints immediately, data pops in as messages arrive).
		return styles.Dim.Render("connecting to wtd…")
	case m.width < minWidth || m.height < minHeight:
		msg := fmt.Sprintf("terminal too small — need ≥ %dx%d (have %dx%d)", minWidth, minHeight, m.width, m.height)
		return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, msg)
	case m.fatal != "":
		card := styles.FatalCard.Render("protocol mismatch\n\n" + m.fatal + "\n\nany key exits")
		return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, card)
	case m.approve.open:
		return renderApproveModal(m.width, m.height, m.approve)
	case !m.sidebar.hasData && (m.conn == connDown || m.conn == connReconnecting):
		return m.downCardView()
	default:
		return m.shellView()
	}
}

func (m appModel) downCardView() string {
	lines := []string{fmt.Sprintf("cannot reach wtd at %s — is it running? (wtd -root ~/code)", m.socket)}
	switch m.conn {
	case connConnecting:
		lines = append(lines, "", fmt.Sprintf("connecting… attempt %d", m.connAttempt))
	case connDown, connReconnecting:
		lines = append(lines, "", fmt.Sprintf("retrying in %s… attempt %d", m.connWait.Round(time.Second), m.connAttempt))
	}
	lines = append(lines, "", "R retry now   q quit")
	card := styles.DownCard.Render(strings.Join(lines, "\n"))
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, card)
}

func (m appModel) shellView() string {
	const topH, keybarH = 1, 1
	bodyH := m.height - topH - keybarH
	if bodyH < 1 {
		bodyH = 1
	}

	top := renderTopbar(m.width, m.sidebar.worktrees(), m.conn, m.screen)
	side := m.sidebar.view(sidebarWidth, bodyH)
	mainWidth := m.width - sidebarWidth
	if mainWidth < 0 {
		mainWidth = 0
	}
	main := m.mainPaneView(mainWidth, bodyH)
	body := lipgloss.JoinHorizontal(lipgloss.Top, side, main)
	keybar := renderKeybar(m.width, m.screen, m.toast)

	return lipgloss.JoinVertical(lipgloss.Left, top, body, keybar)
}

// mainPaneView renders Radar's diff pane (flatten.go/diffview.go/radar.go)
// or, in Review, the same diff plus the right rail (review.go).
func (m appModel) mainPaneView(width, height int) string {
	w, ok := m.sidebar.selected()
	if !ok {
		style := lipgloss.NewStyle().Width(width).Height(height)
		return style.Render(styles.Dim.Render("no worktree selected"))
	}
	if m.screen == screenReview {
		return m.review.view(width, height, w, &m.radar)
	}
	return m.radar.view(width, height, w)
}
