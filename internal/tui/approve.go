package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/navbytes/wt-cockpit/internal/model"
)

// approveModal is the `a` confirm dialog's tiny state machine (P3-design.md
// §1.4/§1.5): closed until openApprove summons it ("summon only when the
// pane's state allows" — a worktree must be selected). Once open it shows
// the client's own gate knowledge (reviewed n/m) plus what the daemon
// itself enforces, and always allows submitting — "the daemon is the
// authority". A refused gate (409) or any other Approve failure keeps the
// modal open with the message rendered verbatim; success closes it (the
// toast + the row leaving the radar are handled by app.go's approveOKMsg
// case, riding the engine's existing worktree.removed SSE event).
type approveModal struct {
	open            bool
	id              string
	branch, base    string
	reviewed, total int
	dirty           bool // Worktree.State == StateDirty as of openApprove — the client's own pre-warn for the daemon's clean-tree gate (ux-expert P3-cheap)
	submitting      bool
	errMsg          string // daemon's 409/error body verbatim; non-empty keeps the modal open
}

// openApprove summons the modal for the selected worktree. No-ops (blocked)
// when nothing is selected, or when it's already open — re-pressing `a`
// must not wipe an in-flight submit or a shown error.
func (m appModel) openApprove() (tea.Model, tea.Cmd) {
	if m.approve.open {
		return m, nil
	}
	w, ok := m.sidebar.selected()
	if !ok {
		return m, nil
	}
	// Prefer the loaded diff's live per-file map (reviewProgress, the same
	// one the Review rail uses) over Worktree.Reviewed's aggregate count
	// when it's available for this worktree: the aggregate only updates via
	// the review.changed SSE round trip and can be momentarily stale right
	// after a space-toggle or an external edit, while the diff itself is
	// already current the instant it's loaded. Falls back to the aggregate
	// when the diff pane isn't (yet) showing this worktree.
	reviewed, total := w.Reviewed, w.Stats.Files
	if m.radar.pane.diff.WorktreeID == w.ID {
		reviewed, total = reviewProgress(m.radar.pane.diff)
	}
	m.approve = approveModal{
		open: true, id: w.ID,
		branch: w.Branch, base: w.Base,
		reviewed: reviewed, total: total,
		dirty: w.State == model.StateDirty,
	}
	return m, nil
}

// handleApproveKey routes keys while the modal is open: ⏎ submits (unless
// already in flight), esc cancels, ctrl+c still quits (the universal
// interrupt); every other key — including plain "q" — is swallowed, same
// spirit as the search input's "q inert while focused".
func (m appModel) handleApproveKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		m.approve = approveModal{}
		return m, nil
	case "enter":
		if m.approve.submitting {
			return m, nil
		}
		m.approve.submitting = true
		m.approve.errMsg = ""
		return m, approveCmd(m.ctx, m.api, m.approve.id)
	}
	return m, nil
}

// applyApproveErr keeps the modal open with the daemon's message verbatim
// (P3-design.md §1.4: "modal stays open"). Stale-modal guard: if the user
// already cancelled (or a different approve is now open), a late error for
// the old id is dropped rather than reopening/relabeling the modal.
func (m appModel) applyApproveErr(msg approveErrMsg) appModel {
	if !m.approve.open {
		return m
	}
	m.approve.submitting = false
	m.approve.errMsg = msg.Gate
	return m
}

func approveCmd(ctx context.Context, api apiClient, id string) tea.Cmd {
	return func() tea.Msg {
		reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		res, err := api.Approve(reqCtx, id)
		if err != nil {
			return approveErrMsg{Gate: err.Error()}
		}
		return approveOKMsg{Res: res}
	}
}

// renderApproveModal is the centered card (P3-design.md §1.4): the merge
// line, the client's own gate knowledge, what the daemon enforces, and —
// only once a submit has been refused — the daemon's message verbatim in
// red. Composed entirely from styles.go's existing named tokens (frozen);
// the outer box is styles.Card, a bordered card style (ux-expert P3-cheap —
// previously a bare, colorless Padding-only style with no visible edge).
//
// The reviewed line is joined by a second gate line, ✓/✗ clean tree, from
// the one dirty-tree signal the client already has (Worktree.State as of
// openApprove) — a pre-warn for the daemon's own clean-tree gate, not a new
// check of its own (the daemon still decides; this just tells the user what
// it's likely to say before they submit and wait on a round trip to find out).
func renderApproveModal(width, height int, a approveModal) string {
	mark := styles.Del.Render("✗")
	if a.total > 0 && a.reviewed >= a.total {
		mark = styles.Add.Render("✓")
	}
	cleanMark := styles.Add.Render("✓")
	if a.dirty {
		cleanMark = styles.Del.Render("✗")
	}
	lines := []string{
		styles.Txt.Bold(true).Render(fmt.Sprintf("merge %s → %s", a.branch, a.base)),
		"",
		fmt.Sprintf("%s reviewed %d/%d", mark, a.reviewed, a.total),
		fmt.Sprintf("%s clean tree", cleanMark),
		styles.Dim.Render("daemon enforces: full review · clean tree · clean merge"),
	}
	if a.submitting {
		lines = append(lines, "", styles.Dim.Render("approving…"))
	}
	if a.errMsg != "" {
		lines = append(lines, "", styles.Del.Render(a.errMsg))
	}
	lines = append(lines, "", styles.Dim.Render("⏎ submit   esc cancel"))

	card := styles.Card.Render(strings.Join(lines, "\n"))
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, card)
}
