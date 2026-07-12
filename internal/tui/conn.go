package tui

import (
	"context"
	"math/rand"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/navbytes/wt-cockpit/internal/model"
)

// connEvent is what the background connection goroutine posts for the model
// to pick up via waitForConnEvent: either a delivered model.Event or a
// connState transition. Exactly one of the two fields is set.
type connEvent struct {
	event *model.Event
	state *connMsg
}

// connStartedMsg carries the channel startConnCmd just created so Update can
// store it and arm the first waitForConnEvent — P3-design.md §2.4's
// "canonical v1 channel pattern": a goroutine writes to a buffered chan, the
// model re-arms a waitForEvent(ch) tea.Cmd after each receive.
type connStartedMsg struct{ ch <-chan connEvent }

// startConnCmd launches the single long-lived SSE goroutine (called once
// from Init) and returns its channel wrapped in a message. retry lets the
// `R` key skip an in-progress backoff wait (down-card's "retry now").
func startConnCmd(ctx context.Context, api apiClient, retry <-chan struct{}) tea.Cmd {
	return func() tea.Msg {
		ch := make(chan connEvent, 64)
		go runConn(ctx, api, ch, retry)
		return connStartedMsg{ch: ch}
	}
}

// waitForConnEvent re-arms after every receive: the model calls this again
// each time it processes a connMsg/eventMsg so the connection loop's next
// delivery always has somewhere to land. Returns nil (a no-op Msg) once the
// channel closes (program shutdown), rather than looping forever.
func waitForConnEvent(ch <-chan connEvent) tea.Cmd {
	return func() tea.Msg {
		e, ok := <-ch
		if !ok {
			return nil
		}
		if e.event != nil {
			return eventMsg(*e.event)
		}
		return *e.state
	}
}

// runConn owns dial -> read -> deliver for the SSE stream (internal/client.
// Events), reconnecting on any failure with capped exponential backoff:
// immediate first retry (daemon restarts are the common case), then 500ms
// doubling to an 8s cap, ±20% jitter, forever (P3-design.md §2.4/§6).
// Attempt is per reconnect *episode*: it counts consecutive failures since
// the connection was last live, not a lifetime total — a connection that
// eventually drops after being live for hours starts its next episode back
// at attempt 1 with the immediate-first-retry treatment, rather than
// inheriting whatever backoff a much earlier outage had climbed to. It
// never returns until ctx is cancelled.
func runConn(ctx context.Context, api apiClient, ch chan<- connEvent, retry <-chan struct{}) {
	attempt := 0
	everLive := false
	for ctx.Err() == nil {
		attempt++
		sendState(ctx, ch, connMsg{State: connConnecting, Attempt: attempt})

		wentLive := drainOnce(ctx, api, ch, &everLive)
		if ctx.Err() != nil {
			return
		}

		wait := backoff(attempt)
		state := connReconnecting
		if !everLive {
			state = connDown
		}
		sendState(ctx, ch, connMsg{State: state, Attempt: attempt, Wait: wait})

		if wentLive {
			attempt = 0 // this episode succeeded at least once; the next one gets a fresh count
		}

		select {
		case <-ctx.Done():
			return
		case <-retry:
		case <-time.After(wait):
		}
	}
}

// drainOnce runs one connection attempt to completion (until the stream
// ends or errors), delivering events and flipping to "live" the moment the
// first event arrives. everLive persists across attempts (down-vs-
// reconnecting labeling for the whole runConn lifetime); the returned
// wentLive is scoped to just *this* attempt, which is what the caller needs
// to decide whether to reset its own backoff counter — an attempt that
// never itself connected shouldn't reset the count just because some
// earlier, long-since-dropped attempt once did.
func drainOnce(ctx context.Context, api apiClient, ch chan<- connEvent, everLive *bool) (wentLive bool) {
	events, errs := api.Events(ctx)
	for {
		select {
		case <-ctx.Done():
			return wentLive
		case e, ok := <-events:
			if !ok {
				<-errs // drain the terminal error; the caller reconnects regardless of its value
				return wentLive
			}
			if !wentLive {
				wentLive = true
				*everLive = true
				sendState(ctx, ch, connMsg{State: connLive})
			}
			ev := e
			select {
			case ch <- connEvent{event: &ev}:
			case <-ctx.Done():
				return wentLive
			}
		}
	}
}

// sendState posts a connState transition, respecting ctx cancellation so the
// goroutine can't leak past shutdown waiting on a channel nobody reads
// anymore.
func sendState(ctx context.Context, ch chan<- connEvent, m connMsg) {
	select {
	case ch <- connEvent{state: &m}:
	case <-ctx.Done():
	}
}

// backoff is the wait before reconnect attempt n (1-indexed): immediate for
// the very first retry, then 500ms doubling to an 8s cap, ±20% jitter.
func backoff(attempt int) time.Duration {
	if attempt <= 1 {
		return 0
	}
	base := 500 * time.Millisecond
	for i := 0; i < attempt-2 && base < 8*time.Second; i++ {
		base *= 2
	}
	if base > 8*time.Second {
		base = 8 * time.Second
	}
	jitter := (rand.Float64()*0.4 - 0.2) * float64(base) //nolint:gosec // cosmetic backoff jitter, not security-sensitive
	return base + time.Duration(jitter)
}
