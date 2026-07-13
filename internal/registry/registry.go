// Package registry is the in-memory source of truth for tracked worktrees plus a
// small pub/sub event bus. It emits incremental deltas (not full-state resends) so
// a UI repaint only touches changed rows — this is what lets the cockpit scale to
// hundreds of worktrees without the bus or the render becoming a bottleneck.
package registry

import (
	"slices"
	"sort"
	"sync"

	"github.com/navbytes/wt-cockpit/internal/model"
)

// Registry holds worktree state and fans events out to subscribers.
type Registry struct {
	mu   sync.RWMutex
	wts  map[string]model.Worktree
	subs map[int]chan model.Event
	next int
}

// New returns an empty Registry.
func New() *Registry {
	return &Registry{
		wts:  map[string]model.Worktree{},
		subs: map[int]chan model.Event{},
	}
}

// Subscribe returns a channel of events plus a cancel func. buffer sizes the
// channel; if a subscriber falls behind and the buffer fills, further events for
// that subscriber are dropped rather than blocking the whole engine (the UI can
// resync via List()).
func (r *Registry) Subscribe(buffer int) (<-chan model.Event, func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := r.next
	r.next++
	ch := make(chan model.Event, buffer)
	r.subs[id] = ch
	return ch, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if c, ok := r.subs[id]; ok {
			delete(r.subs, id)
			close(c)
		}
	}
}

// publish sends to all subscribers without blocking. Caller must NOT hold the lock
// in a way that deadlocks; we take a read lock on subs here.
func (r *Registry) publish(e model.Event) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, ch := range r.subs {
		select {
		case ch <- e:
		default: // slow consumer: drop; it can resync from List()
		}
	}
}

// Upsert inserts or updates a worktree. It only emits an event when something a
// client cares about actually changed (stats, state, diff hash, guardrails,
// reviewed count) — identical upserts are silently coalesced.
func (r *Registry) Upsert(w model.Worktree) {
	r.mu.Lock()
	prev, existed := r.wts[w.ID]
	changed := !existed || meaningfullyDiffers(prev, w)
	r.wts[w.ID] = w
	r.mu.Unlock()

	if changed {
		wc := w
		r.publish(model.Event{Type: model.EventWorktreeUpserted, Worktree: &wc, ID: w.ID, At: w.LastChange})
	}
}

// meaningfullyDiffers reports whether two worktree snapshots differ in a way a UI
// would need to repaint for. Guardrails is compared element-wise (not just by
// length): a same-count hit change — one rule swapping for another, or a
// Line moving — must still repaint (model.GuardrailHit is comparable, so
// slices.Equal is exact, no false negatives from a length-only check).
func meaningfullyDiffers(a, b model.Worktree) bool {
	return a.DiffHash != b.DiffHash ||
		a.State != b.State ||
		a.Stats != b.Stats ||
		a.Branch != b.Branch ||
		a.Base != b.Base ||
		a.Agent != b.Agent ||
		a.Reviewed != b.Reviewed ||
		!slices.Equal(a.Guardrails, b.Guardrails)
}

// Remove deletes a worktree and emits a removal event if it existed.
func (r *Registry) Remove(id string) {
	r.mu.Lock()
	_, existed := r.wts[id]
	delete(r.wts, id)
	r.mu.Unlock()
	if existed {
		r.publish(model.Event{Type: model.EventWorktreeRemoved, ID: id})
	}
}

// Get returns a copy of a worktree, or nil if absent.
func (r *Registry) Get(id string) *model.Worktree {
	r.mu.RLock()
	defer r.mu.RUnlock()
	w, ok := r.wts[id]
	if !ok {
		return nil
	}
	return &w
}

// List returns all worktrees sorted by most-recent change first (active work
// floats to the top), with repo+name as a stable tiebreaker.
func (r *Registry) List() []model.Worktree {
	r.mu.RLock()
	out := make([]model.Worktree, 0, len(r.wts))
	for _, w := range r.wts {
		out = append(out, w)
	}
	r.mu.RUnlock()

	sort.Slice(out, func(i, j int) bool {
		if !out[i].LastChange.Equal(out[j].LastChange) {
			return out[i].LastChange.After(out[j].LastChange)
		}
		if out[i].Repo != out[j].Repo {
			return out[i].Repo < out[j].Repo
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// IDs returns the set of currently tracked worktree IDs (used by the engine to
// detect worktrees that have disappeared).
func (r *Registry) IDs() map[string]bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ids := make(map[string]bool, len(r.wts))
	for id := range r.wts {
		ids[id] = true
	}
	return ids
}

// Publish exposes the bus for engine-level events (diff.ready, guardrail, review).
func (r *Registry) Publish(e model.Event) { r.publish(e) }
