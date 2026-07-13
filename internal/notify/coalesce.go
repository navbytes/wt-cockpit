package notify

import (
	"time"

	"github.com/navbytes/wt-cockpit/internal/model"
)

// coalesceWindow, defaultCooldown, and stormThreshold are P5-design.md
// §1.5's frozen debounce/coalescing constants: a 5s accumulation window per
// batch, a 10m per-key re-notify suppression floor (config can override via
// notify.Config.Cooldown), and a >3-pending-worktree storm collapse.
const (
	coalesceWindow  = 5 * time.Second
	defaultCooldown = 10 * time.Minute
	stormThreshold  = 3
)

// hitKey identifies one (worktree, rule, file) guardrail hit for the
// coalescer's own dedupe+cooldown. It is shaped like (but independent of)
// engine.go's own hitKey: the engine's key decides whether a hit is "new" at
// all; this one additionally suppresses a *notification* re-fire when the
// engine legitimately republishes the same key later — e.g. an agent
// removes then reintroduces the same finding (P5-design.md §1.5 point 1).
type hitKey struct {
	worktreeID string
	rule       string
	file       string
}

// group is one worktree's accumulated hits within the current coalescing
// window. first is literally the first hit that landed for this worktree in
// this window — its Message is the notification body, verbatim per
// P5-design.md §1.5 point 2 ("body = the (first) hit's message"); count
// (always >=1) is how many hits landed in total, so count-1 is the "…and N
// more" suffix.
type group struct {
	worktreeID string
	first      model.GuardrailHit
	count      int
}

// flushResult is what flush hands the caller to turn into actual OS
// notifications: either up to stormThreshold per-worktree groups (in
// first-seen order), or — when more worktrees than that were pending — a
// single storm summary naming every pending worktree id instead. Storm is
// non-empty exactly in that case, and Groups is empty then (P5-design.md
// §1.5 points 2-3).
type flushResult struct {
	Groups []group
	Storm  []string
}

// coalescer implements P5-design.md §1.5's debounce/coalescing in full:
// per-key dedupe+cooldown, the 5s accumulation window, and the storm
// collapse. It knows nothing about OS notifications, Lookup, or exec —
// notify.go's Notifier.Run turns a flushResult into fired notifications;
// keeping this type pure is what makes it directly unit-testable with an
// injected clock, no sleeps and no stub binaries needed.
//
// Not safe for concurrent use: Run drives it from its own single goroutine,
// which is also what gives the notifier its "one exec at a time" execution
// discipline.
type coalescer struct {
	cooldown time.Duration
	now      func() time.Time

	cooldownUntil map[hitKey]time.Time
	pending       map[string]*group
	order         []string // worktree ids, first-seen order within the CURRENT window
}

// newCoalescer builds a coalescer. cooldown<=0 falls back to the frozen 10m
// default (mirrors config.NotificationsConfig.CooldownOr's default, kept
// independently here since this package must not import internal/config).
func newCoalescer(cooldown time.Duration, now func() time.Time) *coalescer {
	if cooldown <= 0 {
		cooldown = defaultCooldown
	}
	return &coalescer{
		cooldown:      cooldown,
		now:           now,
		cooldownUntil: map[hitKey]time.Time{},
		pending:       map[string]*group{},
	}
}

// accept folds one guardrail hit (already severity-filtered by the caller)
// into the current window. It reports windowArmed=true exactly when this
// call transitions the coalescer from an empty pending set to a non-empty
// one — the caller's signal to start the 5s window timer; every later
// accept in the same window reports false, since the window is already
// running. A hit whose key is still within its own cooldown is dropped
// before it can either arm or extend the window (P5-design.md §1.5 point
// 1) — an agent flapping the same hit in and out does not re-fire.
func (c *coalescer) accept(id string, hit model.GuardrailHit) (windowArmed bool) {
	key := hitKey{worktreeID: id, rule: hit.Rule, file: hit.File}
	now := c.now()
	if until, ok := c.cooldownUntil[key]; ok && now.Before(until) {
		return false
	}
	c.cooldownUntil[key] = now.Add(c.cooldown)

	windowArmed = len(c.pending) == 0
	g, ok := c.pending[id]
	if !ok {
		g = &group{worktreeID: id, first: hit}
		c.pending[id] = g
		c.order = append(c.order, id)
	}
	g.count++
	return windowArmed
}

// flush drains the current window into a flushResult and resets pending
// state for the next window. cooldownUntil is deliberately NOT reset here —
// P5-design.md §1.5's per-key cooldown spans windows; that's the point of a
// 10-minute suppression versus a 5-second one.
func (c *coalescer) flush() flushResult {
	defer func() {
		c.pending = map[string]*group{}
		c.order = nil
	}()

	if len(c.order) > stormThreshold {
		storm := make([]string, len(c.order))
		copy(storm, c.order)
		return flushResult{Storm: storm}
	}
	groups := make([]group, 0, len(c.order))
	for _, id := range c.order {
		groups = append(groups, *c.pending[id])
	}
	return flushResult{Groups: groups}
}
