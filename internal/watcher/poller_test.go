package watcher

import (
	"context"
	"testing"
	"time"
)

func TestPollerFiresImmediatelyThenOnInterval(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	calls := make(chan string, 8)
	p := &Poller{Interval: 30 * time.Millisecond}
	done := make(chan struct{})
	go func() {
		p.Run(ctx, func(path string) { calls <- path })
		close(done)
	}()

	select {
	case got := <-calls:
		if got != "" {
			t.Errorf("initial call path = %q, want empty (full rescan)", got)
		}
	case <-time.After(time.Second):
		t.Fatal("Poller did not fire immediately on start")
	}

	select {
	case <-calls:
	case <-time.After(time.Second):
		t.Fatal("Poller did not fire again on its interval")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return within 1s of ctx cancel")
	}
}

func TestPollerDefaultsIntervalWhenUnset(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	// Cancel almost immediately: with Interval unset (defaults to 2s) we should
	// still get exactly the initial call and then a prompt return, never a
	// 2-second hang.
	calls := make(chan string, 4)
	p := &Poller{}
	done := make(chan struct{})
	go func() {
		p.Run(ctx, func(path string) { calls <- path })
		close(done)
	}()

	select {
	case <-calls:
	case <-time.After(time.Second):
		t.Fatal("Poller did not fire immediately on start")
	}
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return promptly after ctx cancel (default interval must not block shutdown)")
	}
}
