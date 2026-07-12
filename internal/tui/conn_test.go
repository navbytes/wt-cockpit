package tui

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/navbytes/wt-cockpit/internal/model"
)

func TestBackoffFirstAttemptIsImmediate(t *testing.T) {
	if got := backoff(1); got != 0 {
		t.Errorf("backoff(1) = %v, want 0 (immediate first retry — daemon restarts are the common case)", got)
	}
	if got := backoff(0); got != 0 {
		t.Errorf("backoff(0) = %v, want 0", got)
	}
}

// TestBackoffDoublesTowardTheCapWithJitterBounds pins P3-design.md §2.4/§6:
// 500ms doubling to an 8s cap, ±20% jitter.
func TestBackoffDoublesTowardTheCapWithJitterBounds(t *testing.T) {
	cases := []struct {
		attempt  int
		wantBase time.Duration
	}{
		{2, 500 * time.Millisecond},
		{3, 1 * time.Second},
		{4, 2 * time.Second},
		{5, 4 * time.Second},
		{6, 8 * time.Second},
		{7, 8 * time.Second},  // capped
		{50, 8 * time.Second}, // stays capped, no runaway growth
	}
	for _, c := range cases {
		got := backoff(c.attempt)
		lo := time.Duration(float64(c.wantBase) * 0.8)
		hi := time.Duration(float64(c.wantBase) * 1.2)
		if got < lo || got > hi {
			t.Errorf("backoff(%d) = %v, want within ±20%% of %v (i.e. [%v, %v])", c.attempt, got, c.wantBase, lo, hi)
		}
	}
}

// drainConnStates collects want connMsg state transitions (ignoring any
// event-carrying connEvents, none of which these tests' fakes produce),
// failing the test if timeout elapses first.
func drainConnStates(t *testing.T, ch <-chan connEvent, want int, timeout time.Duration) []connMsg {
	t.Helper()
	var got []connMsg
	deadline := time.After(timeout)
	for len(got) < want {
		select {
		case e := <-ch:
			if e.state != nil {
				got = append(got, *e.state)
			}
		case <-deadline:
			t.Fatalf("timed out waiting for connMsg states: got %d/%d %+v", len(got), want, got)
		}
	}
	return got
}

func TestRunConnEmitsConnectingThenLiveOnFirstEvent(t *testing.T) {
	api := &fakeAPI{eventsFn: func() (<-chan model.Event, <-chan error) {
		events := make(chan model.Event, 1)
		events <- model.Event{Type: "snapshot"}
		return events, make(chan error, 1) // never fires within this test's window
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan connEvent, 64)
	go runConn(ctx, api, ch, make(chan struct{}))

	states := drainConnStates(t, ch, 2, 2*time.Second)
	if states[0].State != connConnecting || states[0].Attempt != 1 {
		t.Errorf("state[0] = %+v, want connConnecting attempt 1", states[0])
	}
	if states[1].State != connLive {
		t.Errorf("state[1] = %+v, want connLive", states[1])
	}
}

// TestRunConnEmitsDownWhenNeverLiveAndConnectionKeepsFailing pins the
// full-screen "daemon down" card's trigger: a connection that has *never*
// gone live reports connDown (not connReconnecting) on every failed retry.
func TestRunConnEmitsDownWhenNeverLiveAndConnectionKeepsFailing(t *testing.T) {
	api := &fakeAPI{eventsFn: func() (<-chan model.Event, <-chan error) {
		return closedEvents(errors.New("refused"))
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan connEvent, 64)
	go runConn(ctx, api, ch, make(chan struct{}))

	// connecting(1) -> down(1, 0-wait) -> connecting(2) -> down(2, ~500ms
	// wait) — none of these 4 require waiting through a real backoff sleep
	// to *observe* (the sleep happens after emitting down(2)), so this
	// resolves fast and deterministically.
	states := drainConnStates(t, ch, 4, 2*time.Second)
	if states[0].State != connConnecting {
		t.Errorf("state[0] = %+v, want connConnecting", states[0])
	}
	if states[1].State != connDown {
		t.Errorf("state[1] = %+v, want connDown (never yet live)", states[1])
	}
	if states[2].State != connConnecting || states[2].Attempt != 2 {
		t.Errorf("state[2] = %+v, want connConnecting attempt 2", states[2])
	}
	if states[3].State != connDown {
		t.Errorf("state[3] = %+v, want connDown again", states[3])
	}
}

// TestRunConnEmitsReconnectingAfterADropOnceLive pins the *other* half of
// the down/reconnecting split: once we've been live, a later drop reports
// connReconnecting, not connDown (P3-design.md §1.4: stale data stays
// rendered, only a small conn-chip changes).
func TestRunConnEmitsReconnectingAfterADropOnceLive(t *testing.T) {
	calls := 0
	api := &fakeAPI{eventsFn: func() (<-chan model.Event, <-chan error) {
		calls++
		if calls == 1 {
			events := make(chan model.Event, 1)
			events <- model.Event{Type: "snapshot"}
			close(events)
			errs := make(chan error, 1)
			errs <- errors.New("dropped")
			close(errs)
			return events, errs
		}
		// Every later attempt just holds the connection open with nothing
		// to say, so the test can assert on what came before without racing
		// a second drop.
		return make(chan model.Event), make(chan error)
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan connEvent, 64)
	go runConn(ctx, api, ch, make(chan struct{}))

	// connecting(1) -> live -> reconnecting(1, 0-wait since backoff(1)==0) ->
	// connecting(1): a new episode starts back at attempt 1 (per-episode
	// counting, not a lifetime total) since this attempt did go live before
	// dropping.
	states := drainConnStates(t, ch, 4, 2*time.Second)
	if states[1].State != connLive {
		t.Fatalf("state[1] = %+v, want connLive", states[1])
	}
	if states[2].State != connReconnecting || states[2].Attempt != 1 {
		t.Errorf("state[2] = %+v, want connReconnecting attempt 1 (was live before the drop, not connDown)", states[2])
	}
	if states[3].State != connConnecting || states[3].Attempt != 1 {
		t.Errorf("state[3] = %+v, want connConnecting attempt 1 (fresh episode after a live drop)", states[3])
	}
}

// TestRunConnRetryChannelSkipsBackoffWait pins the `R`-key "retry now"
// affordance: sending on retry must unblock an in-progress backoff sleep
// immediately rather than waiting out the full ~500ms+ base wait.
func TestRunConnRetryChannelSkipsBackoffWait(t *testing.T) {
	api := &fakeAPI{eventsFn: func() (<-chan model.Event, <-chan error) {
		return closedEvents(errors.New("refused"))
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan connEvent, 64)
	retry := make(chan struct{}, 1)
	go runConn(ctx, api, ch, retry)

	// Drain through connecting(1)/down(1, 0-wait)/connecting(2)/down(2): the
	// loop is now asleep on attempt 3's real ~500ms±jitter backoff.
	drainConnStates(t, ch, 4, 2*time.Second)

	start := time.Now()
	retry <- struct{}{}
	states := drainConnStates(t, ch, 1, 2*time.Second)
	if elapsed := time.Since(start); elapsed > 400*time.Millisecond {
		t.Errorf("retry took %v to unblock the backoff wait, want well under the ~500ms base wait it preempted", elapsed)
	}
	if states[0].State != connConnecting || states[0].Attempt != 3 {
		t.Errorf("state after retry = %+v, want connConnecting attempt 3", states[0])
	}
}

// TestRunConnStopsOnContextCancellation ensures the goroutine actually exits
// rather than leaking once ctx is cancelled — every other test in this file
// relies on that happening at defer-cancel time between tests.
func TestRunConnStopsOnContextCancellation(t *testing.T) {
	api := &fakeAPI{eventsFn: func() (<-chan model.Event, <-chan error) {
		return make(chan model.Event), make(chan error) // blocks until ctx is done
	}}
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan connEvent, 64)
	done := make(chan struct{})
	go func() {
		runConn(ctx, api, ch, make(chan struct{}))
		close(done)
	}()

	drainConnStates(t, ch, 1, 2*time.Second) // connecting(1)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runConn did not return within 2s of context cancellation")
	}
}
