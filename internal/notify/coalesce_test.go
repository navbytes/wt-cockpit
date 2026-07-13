package notify

// Pure coalescer tests: an injected clock, no exec, no Notifier at all —
// P5-design.md §1.5's debounce/coalescing constants (5s window, 10m
// cooldown, >3-worktree storm collapse) exercised deterministically.

import (
	"testing"
	"time"

	"github.com/navbytes/wt-cockpit/internal/model"
)

func fakeNow(t *time.Time) func() time.Time {
	return func() time.Time { return *t }
}

func hit(rule, file, sev string) model.GuardrailHit {
	return model.GuardrailHit{Rule: rule, File: file, Severity: sev, Message: rule + " tripped"}
}

// TestCoalescerFirstAcceptArmsWindowSubsequentDoNot pins the "first passing
// event arms a 5s timer; further events accumulate" rule (P5-design.md §1.5
// point 2).
func TestCoalescerFirstAcceptArmsWindowSubsequentDoNot(t *testing.T) {
	now := time.Now()
	c := newCoalescer(10*time.Minute, fakeNow(&now))

	if armed := c.accept("wt1", hit("r1", "f1", "danger")); !armed {
		t.Error("first accept in an empty window should arm the timer")
	}
	if armed := c.accept("wt1", hit("r2", "f2", "danger")); armed {
		t.Error("a second accept in the same (still-pending) window must not re-arm it")
	}
	if armed := c.accept("wt2", hit("r1", "f1", "danger")); armed {
		t.Error("an accept for a different worktree in the same window still must not re-arm it")
	}
}

// TestCoalescerCollapsesBurstIntoOneGroupPerWorktree: several hits for the
// same worktree within one window collapse into a single group with the
// first hit's message and the right count for "…and N more".
func TestCoalescerCollapsesBurstIntoOneGroupPerWorktree(t *testing.T) {
	now := time.Now()
	c := newCoalescer(10*time.Minute, fakeNow(&now))

	c.accept("wt1", hit("secrets-pattern", "a.go", "danger"))
	c.accept("wt1", hit("ci-workflow-delete", "b.yml", "danger"))
	c.accept("wt1", hit("touches-migrations", "c.sql", "danger"))

	r := c.flush()
	if len(r.Storm) != 0 {
		t.Fatalf("expected no storm collapse for 1 worktree, got %+v", r)
	}
	if len(r.Groups) != 1 {
		t.Fatalf("Groups = %+v, want exactly 1 group", r.Groups)
	}
	g := r.Groups[0]
	if g.worktreeID != "wt1" {
		t.Errorf("worktreeID = %q, want wt1", g.worktreeID)
	}
	if g.count != 3 {
		t.Errorf("count = %d, want 3 (…and 2 more)", g.count)
	}
	if g.first.Rule != "secrets-pattern" {
		t.Errorf("first hit = %+v, want the FIRST hit that landed (secrets-pattern), not the last", g.first)
	}
}

// TestCoalescerGroupsSeveralWorktreesUpToThreshold: up to stormThreshold
// (3) distinct worktrees each get their own group, no collapse.
func TestCoalescerGroupsSeveralWorktreesUpToThreshold(t *testing.T) {
	now := time.Now()
	c := newCoalescer(10*time.Minute, fakeNow(&now))

	for _, wt := range []string{"wt1", "wt2", "wt3"} {
		c.accept(wt, hit("r1", "f1", "danger"))
	}
	r := c.flush()
	if len(r.Storm) != 0 {
		t.Fatalf("expected no storm collapse at exactly the threshold (3), got %+v", r)
	}
	if len(r.Groups) != 3 {
		t.Fatalf("Groups = %+v, want 3", r.Groups)
	}
	// First-seen order preserved.
	for i, wt := range []string{"wt1", "wt2", "wt3"} {
		if r.Groups[i].worktreeID != wt {
			t.Errorf("Groups[%d].worktreeID = %q, want %q (first-seen order)", i, r.Groups[i].worktreeID, wt)
		}
	}
}

// TestCoalescerStormCollapseAboveThreshold: more than 3 pending worktrees at
// flush collapses into a single storm summary — no per-worktree groups.
func TestCoalescerStormCollapseAboveThreshold(t *testing.T) {
	now := time.Now()
	c := newCoalescer(10*time.Minute, fakeNow(&now))

	ids := []string{"wt1", "wt2", "wt3", "wt4"}
	for _, wt := range ids {
		c.accept(wt, hit("r1", "f1", "danger"))
	}
	r := c.flush()
	if len(r.Groups) != 0 {
		t.Fatalf("expected storm collapse (no per-worktree groups), got Groups=%+v", r.Groups)
	}
	if len(r.Storm) != 4 {
		t.Fatalf("Storm = %+v, want all 4 pending worktree ids", r.Storm)
	}
	for i, wt := range ids {
		if r.Storm[i] != wt {
			t.Errorf("Storm[%d] = %q, want %q (first-seen order)", i, r.Storm[i], wt)
		}
	}
}

// TestCoalescerCooldownSuppresssRepeatKey: a re-trip of the identical
// (worktree,rule,file) key inside its cooldown window is dropped outright —
// it neither arms a window nor counts toward a group.
func TestCoalescerCooldownSuppressesRepeatKey(t *testing.T) {
	now := time.Now()
	c := newCoalescer(10*time.Minute, fakeNow(&now))

	c.accept("wt1", hit("secrets-pattern", "a.go", "danger"))
	c.flush() // first window fires and resets pending, but NOT cooldownUntil

	now = now.Add(1 * time.Minute) // well within the 10m cooldown
	armed := c.accept("wt1", hit("secrets-pattern", "a.go", "danger"))
	if armed {
		t.Error("a repeat of the same key inside its cooldown must not arm a new window")
	}
	r := c.flush()
	if len(r.Groups) != 0 || len(r.Storm) != 0 {
		t.Errorf("expected nothing pending (the repeat was dropped), got %+v", r)
	}
}

// TestCoalescerCooldownExpiresAndAllowsRefire: once the cooldown has fully
// elapsed, the same key can fire again.
func TestCoalescerCooldownExpiresAndAllowsRefire(t *testing.T) {
	now := time.Now()
	c := newCoalescer(10*time.Minute, fakeNow(&now))

	c.accept("wt1", hit("secrets-pattern", "a.go", "danger"))
	c.flush()

	now = now.Add(11 * time.Minute) // past the 10m cooldown
	armed := c.accept("wt1", hit("secrets-pattern", "a.go", "danger"))
	if !armed {
		t.Error("a repeat of the same key AFTER its cooldown elapsed should arm a fresh window")
	}
	r := c.flush()
	if len(r.Groups) != 1 || r.Groups[0].count != 1 {
		t.Errorf("Groups = %+v, want exactly 1 group with count 1", r.Groups)
	}
}

// TestCoalescerCooldownExactBoundaryAllowsRefire pins the boundary itself
// (accept's check is `now.Before(until)`, so equality is NOT "still in
// cooldown"): at the exact instant the cooldown deadline elapses, a repeat
// must be allowed; one nanosecond earlier it must still be suppressed.
func TestCoalescerCooldownExactBoundaryAllowsRefire(t *testing.T) {
	now := time.Now()
	c := newCoalescer(10*time.Minute, fakeNow(&now))

	c.accept("wt1", hit("secrets-pattern", "a.go", "danger"))
	c.flush()
	deadline := now.Add(10 * time.Minute) // the exact cooldownUntil value accept() set

	now = deadline.Add(-1 * time.Nanosecond)
	if armed := c.accept("wt1", hit("secrets-pattern", "a.go", "danger")); armed {
		t.Error("one nanosecond before the cooldown deadline, a repeat must still be suppressed")
	}

	now = deadline
	if armed := c.accept("wt1", hit("secrets-pattern", "a.go", "danger")); !armed {
		t.Error("exactly at the cooldown deadline, a repeat must be allowed (accept uses Before, not Before-or-equal)")
	}
}

// TestCoalescerFlushPrunesExpiredCooldownKeys is the NIT fix pin:
// cooldownUntil is otherwise never reset (it deliberately spans windows —
// see flush's own doc comment), so over a long daemon uptime with many
// distinct (worktree,rule,file) keys cycling through, it would grow
// unboundedly even though most entries are long expired. flush now prunes
// any entry whose deadline has already passed.
func TestCoalescerFlushPrunesExpiredCooldownKeys(t *testing.T) {
	now := time.Now()
	c := newCoalescer(1*time.Minute, fakeNow(&now))

	c.accept("wt1", hit("r1", "f1", "danger"))
	c.flush()
	if _, ok := c.cooldownUntil[hitKey{worktreeID: "wt1", rule: "r1", file: "f1"}]; !ok {
		t.Fatal("precondition: accept should have set a cooldown entry")
	}

	now = now.Add(2 * time.Minute) // well past the 1-minute cooldown
	c.accept("wt2", hit("r2", "f2", "danger"))
	c.flush()

	if _, ok := c.cooldownUntil[hitKey{worktreeID: "wt1", rule: "r1", file: "f1"}]; ok {
		t.Error("flush should prune a cooldown entry whose deadline has already passed")
	}
	if _, ok := c.cooldownUntil[hitKey{worktreeID: "wt2", rule: "r2", file: "f2"}]; !ok {
		t.Error("flush must not prune a cooldown entry that's still active")
	}
}

// TestCoalescerDistinctKeysIndependent: cooldown on one (worktree,rule,file)
// key must not suppress a different key, even for the same worktree.
func TestCoalescerDistinctKeysIndependent(t *testing.T) {
	now := time.Now()
	c := newCoalescer(10*time.Minute, fakeNow(&now))

	c.accept("wt1", hit("secrets-pattern", "a.go", "danger"))
	c.flush()

	// Same worktree, different rule AND different file — must fire independently.
	if armed := c.accept("wt1", hit("ci-workflow-delete", "b.yml", "danger")); !armed {
		t.Error("a distinct (rule,file) key must arm its own window regardless of an unrelated key's cooldown")
	}
	// Different worktree, same rule+file as the cooled-down key — also
	// independent: the window is already pending by this point (armed by the
	// ci-workflow-delete accept above), so this call itself won't re-arm it,
	// but the hit must still land in the pending set rather than being
	// dropped by wt1's unrelated secrets-pattern cooldown.
	c.accept("wt2", hit("secrets-pattern", "a.go", "danger"))
	r := c.flush()
	if len(r.Groups) != 2 {
		t.Fatalf("Groups = %+v, want 2 (wt1/ci-workflow-delete and wt2/secrets-pattern both independent of wt1/secrets-pattern's cooldown)", r.Groups)
	}
}

// TestCoalescerFlushResetsPendingButKeepsCooldown: after flush, a brand new
// key can immediately arm a fresh window (pending state cleared), while an
// old key already in cooldown stays suppressed (cooldown state persists).
func TestCoalescerFlushResetsPendingButKeepsCooldown(t *testing.T) {
	now := time.Now()
	c := newCoalescer(10*time.Minute, fakeNow(&now))

	c.accept("wt1", hit("r1", "f1", "danger"))
	first := c.flush()
	if len(first.Groups) != 1 {
		t.Fatalf("precondition: first flush should carry 1 group, got %+v", first)
	}

	if armed := c.accept("wt1", hit("r2", "f2", "danger")); !armed {
		t.Error("a brand new key right after a flush should arm a fresh window (pending was reset)")
	}
	second := c.flush()
	if len(second.Groups) != 1 || second.Groups[0].first.Rule != "r2" {
		t.Errorf("second flush = %+v, want exactly the new r2 hit (old pending state must not leak across flushes)", second.Groups)
	}
}
