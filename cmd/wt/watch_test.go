package main

// wt watch's terminal-bell/OSC9 event hook (P5-design.md §1.6), unit-tested
// against a fake events sequence and a fake clock — no pty needed.

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/navbytes/wt-cockpit/internal/model"
)

func newTestWatchBell(now *time.Time) (*watchBell, *bytes.Buffer) {
	var buf bytes.Buffer
	return &watchBell{out: &buf, now: func() time.Time { return *now }}, &buf
}

func dangerEvent(msg string) model.Event {
	return model.Event{Type: model.EventGuardrail, Hit: &model.GuardrailHit{Rule: "r", File: "f", Severity: "danger", Message: msg}}
}

func TestWatchBellRingsOnDangerGuardrailEvent(t *testing.T) {
	now := time.Now()
	b, buf := newTestWatchBell(&now)

	b.maybeRing(dangerEvent("touches database migrations"))

	got := buf.String()
	if !strings.HasPrefix(got, "\a") {
		t.Errorf("output = %q, want it to start with BEL (\\a)", got)
	}
	if !strings.Contains(got, "\x1b]9;wt-cockpit: touches database migrations\x07") {
		t.Errorf("output = %q, want the OSC 9 notification with the hit's message", got)
	}
}

func TestWatchBellIgnoresWarnSeverity(t *testing.T) {
	now := time.Now()
	b, buf := newTestWatchBell(&now)

	b.maybeRing(model.Event{Type: model.EventGuardrail, Hit: &model.GuardrailHit{Severity: "warn", Message: "m"}})

	if buf.Len() != 0 {
		t.Errorf("output = %q, want no bell for a warn-severity hit", buf.String())
	}
}

func TestWatchBellIgnoresNonGuardrailEvents(t *testing.T) {
	now := time.Now()
	b, buf := newTestWatchBell(&now)

	b.maybeRing(model.Event{Type: model.EventWorktreeUpserted})
	b.maybeRing(model.Event{Type: model.EventDiffReady})
	b.maybeRing(model.Event{Type: model.EventGuardrail, Hit: nil}) // guardrail type but no Hit payload

	if buf.Len() != 0 {
		t.Errorf("output = %q, want no bell for non-guardrail (or hit-less) events", buf.String())
	}
}

// TestWatchBellThrottlesToOnePer5s pins the exact P5-design.md §1.6 throttle:
// a second danger hit within 5s of the last ring is silent; once 5s has
// elapsed, the next one rings again.
func TestWatchBellThrottlesToOnePer5s(t *testing.T) {
	now := time.Now()
	b, buf := newTestWatchBell(&now)

	b.maybeRing(dangerEvent("first"))
	if buf.Len() == 0 {
		t.Fatal("precondition: the first danger hit should ring")
	}
	buf.Reset()

	now = now.Add(4999 * time.Millisecond) // just under the 5s throttle
	b.maybeRing(dangerEvent("second"))
	if buf.Len() != 0 {
		t.Errorf("output = %q, want silence: a re-trip inside the 5s throttle must not ring again", buf.String())
	}

	now = now.Add(2 * time.Millisecond) // now just over 5s since the first ring
	b.maybeRing(dangerEvent("third"))
	if buf.Len() == 0 {
		t.Error("expected a ring once the 5s throttle has elapsed")
	}
	if !strings.Contains(buf.String(), "third") {
		t.Errorf("output = %q, want the third hit's message", buf.String())
	}
}

// TestWatchBellSanitizesHostileMessage: a message carrying control/escape
// bytes (e.g. from an attacker/team-controlled rule pack Message field, per
// P5-design.md §1.3) must never smuggle a second escape sequence into the
// terminal — notify.Sanitize is reused verbatim, so this pins the
// integration, not Sanitize's own behavior (see internal/notify's tests).
func TestWatchBellSanitizesHostileMessage(t *testing.T) {
	now := time.Now()
	b, buf := newTestWatchBell(&now)

	b.maybeRing(dangerEvent("pwned\x1b]0;pwn\x07 line\nbreak"))

	got := buf.String()
	// Exactly one BEL (the bell hook's own) and one OSC 9 introducer — no
	// second escape sequence smuggled in from the hostile message content.
	if n := strings.Count(got, "\x1b]"); n != 1 {
		t.Errorf("output = %q contains %d OSC introducers, want exactly 1 (the hook's own)", got, n)
	}
	if strings.Contains(got, "\n") {
		t.Errorf("output = %q must not contain a raw newline", got)
	}
}

func TestWatchBellBodyCapMatchesDesign(t *testing.T) {
	if watchBellBodyCap != 120 {
		t.Errorf("watchBellBodyCap = %d, want 120 (P5-design.md §1.6)", watchBellBodyCap)
	}
	if watchBellThrottle != 5*time.Second {
		t.Errorf("watchBellThrottle = %v, want 5s (P5-design.md §1.6)", watchBellThrottle)
	}
}

// TestWatchBellTruncatesLongMessageToBodyCap proves the 120-char OSC 9 cap
// (tighter than the desktop notifier's 200) is actually applied.
func TestWatchBellTruncatesLongMessageToBodyCap(t *testing.T) {
	now := time.Now()
	b, buf := newTestWatchBell(&now)

	b.maybeRing(dangerEvent(strings.Repeat("A", 300)))

	got := buf.String()
	start := strings.Index(got, "wt-cockpit: ") + len("wt-cockpit: ")
	// LastIndex, not Index: the leading BEL (\a) that opens the whole
	// sequence is ALSO byte 0x07 — the same byte as \x07 — so searching for
	// the first 0x07 would match that leading bell, not the OSC terminator.
	end := strings.LastIndex(got, "\x07")
	if start < 0 || end < 0 || end < start {
		t.Fatalf("output = %q, could not locate the OSC 9 body", got)
	}
	if n := len([]rune(got[start:end])); n != watchBellBodyCap {
		t.Errorf("OSC 9 body length = %d runes, want %d (the design's cap)", n, watchBellBodyCap)
	}
}

// TestWatchBellDistinctInvocationsIndependentOfSharedState: two separate
// watchBell instances (as would exist across separate wt watch processes)
// never interfere — sanity check that the throttle state is per-instance.
func TestWatchBellDistinctInvocationsIndependentOfSharedState(t *testing.T) {
	now1, now2 := time.Now(), time.Now()
	b1, buf1 := newTestWatchBell(&now1)
	b2, buf2 := newTestWatchBell(&now2)

	b1.maybeRing(dangerEvent("a"))
	if buf1.Len() == 0 {
		t.Error("b1 should ring on its first danger hit")
	}
	if buf2.Len() != 0 {
		t.Error("b2 must be unaffected by b1's ring")
	}
	b2.maybeRing(dangerEvent("b"))
	if buf2.Len() == 0 {
		t.Error("b2 should ring on its own first danger hit, independent of b1's state")
	}
}
