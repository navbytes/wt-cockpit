// Package watcher triggers engine refreshes. The Watcher interface lets the engine
// stay ignorant of *how* change is detected. The MVP ships a Poller (a periodic
// safety-net tick that works everywhere, including flaky network mounts). A
// git-state-first fsnotify watcher can be dropped in later behind the same
// interface — the engine's Refresh loop does not change.
package watcher

import (
	"context"
	"time"
)

// Watcher drives refreshes by invoking onChange. An empty change path means
// "rescan everything"; a specific path is a hint the engine may use to refresh
// just that worktree.
type Watcher interface {
	Run(ctx context.Context, onChange func(path string))
}

// Poller ticks at a fixed interval and requests a full rescan each time.
type Poller struct {
	Interval time.Duration
}

// Run blocks until ctx is cancelled, invoking onChange("") every Interval. It also
// fires once immediately so startup is not delayed by a full interval.
func (p *Poller) Run(ctx context.Context, onChange func(path string)) {
	interval := p.Interval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	onChange("") // initial scan
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			onChange("")
		}
	}
}
