package tui

import (
	"time"

	"github.com/navbytes/wt-cockpit/internal/model"
)

// Every message type in this file is frozen as of wt-cockpit v0.3 WP1
// (P3-design.md §2.4/§4): later work packages may add fields to their own
// new message types, but must not change these. They are the contract WP2
// (diff pane) and WP3 (review/approve/tmux/search) build against.

// versionMsg is the successful result of GET /api/version.
type versionMsg struct {
	Protocol int
	Version  string
}

// versionErrMsg is a failed version fetch. errors.As(Err, *client.UnreachableError)
// distinguishes "daemon down" (handled by the conn state machine) from a
// genuine handshake refusal (fatal — see app.go's applyVersionErr).
type versionErrMsg struct{ Err error }

// listMsg is the result of GET /api/worktrees.
type listMsg []model.Worktree

// listErrMsg is a failed worktree list fetch.
type listErrMsg struct{ Err error }

// eventMsg is one delivery off the SSE bus (internal/client.Events).
type eventMsg model.Event

// connState is the SSE connection's lifecycle, per P3-design.md §2.4/§1.4:
// connecting (a dial attempt in flight), live (streaming normally), down
// (waiting to retry the very first connection — never yet live: the
// full-screen "daemon down" card), reconnecting (waiting to retry after a
// mid-session drop — the small conn-chip treatment, stale data stays put).
type connState int

const (
	connConnecting connState = iota
	connLive
	connDown
	connReconnecting
)

// connMsg reports a connection-state transition from the background SSE
// loop (internal/tui/conn.go). Attempt/Wait are only meaningful for
// connDown/connReconnecting (the backoff card's "retrying in Ns… attempt N").
type connMsg struct {
	State   connState
	Attempt int
	Wait    time.Duration
}

// diffMsg is the result of GET /api/diff?id=. WP2.
type diffMsg struct {
	ID   string
	Diff model.Diff
}

// diffErrMsg is a failed diff fetch. WP2.
type diffErrMsg struct {
	ID  string
	Err error
}

// highlightedMsg carries one file's chroma-tokenized lines once async
// highlighting finishes. WP2.
type highlightedMsg struct {
	FileHash string
	Lines    []string
}

// reviewOKMsg is a successful POST /api/review. WP3.
type reviewOKMsg struct {
	ID, File string
	Reviewed bool
}

// reviewErrMsg is a failed POST /api/review. Conflict distinguishes the 409
// (file changed since viewed — optimistic-toggle revert) from any other
// failure. WP3.
type reviewErrMsg struct {
	ID, File string
	Conflict bool
	Err      error
}

// approveOKMsg is a successful POST /api/approve. WP3.
type approveOKMsg struct{ Res model.ApproveResult }

// approveErrMsg is a refused approve gate (409); Gate is the daemon's body
// verbatim. WP3.
type approveErrMsg struct{ Gate string }

// refreshDoneMsg is the result of POST /api/refresh (the `R` key).
type refreshDoneMsg struct{ Err error }

// tmuxDoneMsg is the result of a `t` jump attempt; nil Err means it jumped.
// WP3.
type tmuxDoneMsg struct{ Err error }

// tickMsg drives the sidebar's relative-time ("3s ago") refresh, every 5s.
type tickMsg time.Time

// toastExpiredMsg clears the keybar's transient status-line message after
// its display window (P3-design.md §1.5: 4s, one at a time, latest wins).
type toastExpiredMsg struct{}
