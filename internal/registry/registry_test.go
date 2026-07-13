package registry

import (
	"testing"
	"time"

	"github.com/navbytes/wt-cockpit/internal/model"
)

func wt(id, repo string, add int) model.Worktree {
	return model.Worktree{ID: id, Repo: repo, Name: id, Stats: model.Stats{Add: add}, LastChange: time.Unix(int64(add), 0)}
}

func TestUpsertAndList(t *testing.T) {
	r := New()
	r.Upsert(wt("a", "p1", 1))
	r.Upsert(wt("b", "p1", 5))
	list := r.List()
	if len(list) != 2 {
		t.Fatalf("want 2, got %d", len(list))
	}
	// List is sorted by most-recent change first (add=5 before add=1 given our timestamps).
	if list[0].ID != "b" {
		t.Errorf("expected most-recently-changed first, got %q", list[0].ID)
	}
}

func TestUpsertUpdatesInPlace(t *testing.T) {
	r := New()
	r.Upsert(wt("a", "p1", 1))
	r.Upsert(wt("a", "p1", 9)) // same id, new stats
	if got := r.Get("a"); got == nil || got.Stats.Add != 9 {
		t.Fatalf("upsert did not update in place: %+v", got)
	}
	if len(r.List()) != 1 {
		t.Errorf("duplicate id created a second entry")
	}
}

func TestRemove(t *testing.T) {
	r := New()
	r.Upsert(wt("a", "p1", 1))
	r.Remove("a")
	if r.Get("a") != nil {
		t.Error("worktree not removed")
	}
}

func TestBusReceivesUpsertAndRemoveEvents(t *testing.T) {
	r := New()
	sub, cancel := r.Subscribe(8)
	defer cancel()

	r.Upsert(wt("a", "p1", 1))
	r.Remove("a")

	e1 := recv(t, sub)
	if e1.Type != model.EventWorktreeUpserted || e1.Worktree == nil || e1.Worktree.ID != "a" {
		t.Fatalf("first event wrong: %+v", e1)
	}
	e2 := recv(t, sub)
	if e2.Type != model.EventWorktreeRemoved || e2.ID != "a" {
		t.Fatalf("second event wrong: %+v", e2)
	}
}

func TestUnchangedUpsertDoesNotEmit(t *testing.T) {
	r := New()
	sub, cancel := r.Subscribe(8)
	defer cancel()

	w := wt("a", "p1", 1)
	w.DiffHash = "h1"
	r.Upsert(w)
	recv(t, sub) // consume first upsert

	// Identical upsert (same DiffHash + stats + state) should be a no-op on the bus.
	r.Upsert(w)
	select {
	case e := <-sub:
		t.Fatalf("unchanged upsert should not emit, got %+v", e)
	case <-time.After(50 * time.Millisecond):
		// good: nothing emitted
	}
}

// TestSameCountGuardrailSwapStillEmits is the registry-level pin for the
// meaningfullyDiffers fix: a same-COUNT change to Guardrails (one rule swaps
// for another, or a hit's Line moves) must still be treated as meaningful —
// a length-only comparison would silently swallow it.
func TestSameCountGuardrailSwapStillEmits(t *testing.T) {
	r := New()
	sub, cancel := r.Subscribe(8)
	defer cancel()

	w := wt("a", "p1", 1)
	w.Guardrails = []model.GuardrailHit{{Rule: "rule-a", Severity: "warn", File: "x.go"}}
	r.Upsert(w)
	recv(t, sub) // consume the first upsert

	w2 := w
	w2.Guardrails = []model.GuardrailHit{{Rule: "rule-b", Severity: "danger", File: "x.go"}} // same count (1), different rule
	r.Upsert(w2)

	e := recv(t, sub)
	if e.Worktree == nil || len(e.Worktree.Guardrails) != 1 || e.Worktree.Guardrails[0].Rule != "rule-b" {
		t.Fatalf("expected the swapped guardrail hit to emit, got %+v", e)
	}
}

// TestSameGuardrailHitDoesNotEmit is the control case: an identical
// Guardrails slice (same rule, same fields) alongside no other change must
// still coalesce to nothing on the bus.
func TestSameGuardrailHitDoesNotEmit(t *testing.T) {
	r := New()
	sub, cancel := r.Subscribe(8)
	defer cancel()

	w := wt("a", "p1", 1)
	w.Guardrails = []model.GuardrailHit{{Rule: "rule-a", Severity: "warn", File: "x.go", Line: 3}}
	r.Upsert(w)
	recv(t, sub)

	r.Upsert(w) // byte-identical Guardrails
	select {
	case e := <-sub:
		t.Fatalf("unchanged Guardrails should not emit, got %+v", e)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestMultipleSubscribersBothReceive(t *testing.T) {
	r := New()
	s1, c1 := r.Subscribe(8)
	s2, c2 := r.Subscribe(8)
	defer c1()
	defer c2()
	r.Upsert(wt("a", "p1", 1))
	if recv(t, s1).Worktree.ID != "a" || recv(t, s2).Worktree.ID != "a" {
		t.Error("both subscribers should receive the event")
	}
}

func recv(t *testing.T, ch <-chan model.Event) model.Event {
	t.Helper()
	select {
	case e := <-ch:
		return e
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
		return model.Event{}
	}
}
