package tui

import "github.com/charmbracelet/bubbles/key"

// keyMap is every binding in P3-design.md §1.3, frozen as of WP1 (§4): later
// work packages wire the behavior behind a binding (e.g. Approve, TmuxJump)
// but must not rename or repurpose one. WP1 itself only acts on a subset
// (Up/Down, Enter/Esc view routing, Review, Quit, Refresh) — the rest exist
// now so the keybar text (statusbar.go) already matches the mock, and so
// key.Matches has a single, stable place to check against once WP2/WP3 wire
// the remaining behavior.
type keyMap struct {
	Up, Down     key.Binding // sidebar / file nav
	Enter        key.Binding // focus diff pane / single-pane swap-in
	Esc          key.Binding // back: diff focus -> sidebar, review -> radar, clear search
	Review       key.Binding // r: open Review for the selected worktree
	TmuxJump     key.Binding // t
	Approve      key.Binding // a: confirm-modal
	Search       key.Binding // /
	FilterActive key.Binding // f
	Refresh      key.Binding // R
	Quit         key.Binding // q, ctrl+c
	ToggleReview key.Binding // space, in Review view
	HalfPageDown key.Binding // ctrl+d
	HalfPageUp   key.Binding // ctrl+u
	PageDown     key.Binding // pgdn / space, in diff focus
	PageUp       key.Binding // pgup
	Top          key.Binding // g
	Bottom       key.Binding // G
	PrevFile     key.Binding // [
	NextFile     key.Binding // ]
	ToggleFold   key.Binding // o: expand/collapse a file card
}

var keys = keyMap{
	Up:    key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "up")),
	Down:  key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "down")),
	Enter: key.NewBinding(key.WithKeys("enter"), key.WithHelp("⏎", "focus diff")),
	Esc:   key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "back")),

	Review:       key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "review")),
	TmuxJump:     key.NewBinding(key.WithKeys("t"), key.WithHelp("t", "tmux")),
	Approve:      key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "approve")),
	Search:       key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "search")),
	FilterActive: key.NewBinding(key.WithKeys("f"), key.WithHelp("f", "active-only")),
	Refresh:      key.NewBinding(key.WithKeys("R"), key.WithHelp("R", "refresh")),
	Quit:         key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),

	ToggleReview: key.NewBinding(key.WithKeys(" ", "space"), key.WithHelp("space", "toggle reviewed")),

	HalfPageDown: key.NewBinding(key.WithKeys("ctrl+d"), key.WithHelp("ctrl+d", "½pg down")),
	HalfPageUp:   key.NewBinding(key.WithKeys("ctrl+u"), key.WithHelp("ctrl+u", "½pg up")),
	PageDown:     key.NewBinding(key.WithKeys("pgdown", " ", "space"), key.WithHelp("pgdn", "page down")),
	PageUp:       key.NewBinding(key.WithKeys("pgup"), key.WithHelp("pgup", "page up")),
	Top:          key.NewBinding(key.WithKeys("g"), key.WithHelp("g", "top")),
	Bottom:       key.NewBinding(key.WithKeys("G"), key.WithHelp("G", "bottom")),
	PrevFile:     key.NewBinding(key.WithKeys("["), key.WithHelp("[", "prev file")),
	NextFile:     key.NewBinding(key.WithKeys("]"), key.WithHelp("]", "next file")),
	ToggleFold:   key.NewBinding(key.WithKeys("o"), key.WithHelp("o", "expand/collapse")),
}

// radarKeybar is the Radar view's context-sensitive keybar text (mock's
// keybar-radar). WP1 renders this verbatim; it does not yet reflect
// dynamic state (e.g. an active filter) — that's WP3 (search/filter).
const radarKeybar = "↑↓/jk move  ⏎ diff  r review  t tmux  a approve  / search  f active  R refresh  q quit"

// reviewKeybar is the Review view's context-sensitive keybar text (mock's
// keybar-review). j/k walk files; plain ↑/↓ (and ctrl-d/u, pgup/pgdn, g/G,
// not all spelled out here) fine-scroll the same diff pane (P3-design.md
// §1.3's Review table splits the two apart, unlike Radar's diff-focused
// mode where j/k themselves scroll).
const reviewKeybar = "j/k file  ↑↓ scroll  space toggle  a approve  t tmux  esc back  q quit"
