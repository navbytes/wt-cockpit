// Package notify is wtd's desktop-notification subscriber: a plain registry-
// bus subscriber (Notifier.Run) that fires an OS notification on danger-
// severity guardrail.tripped events, via a fixed-argv exec — osascript on
// darwin, notify-send on linux, never a shell (P5-design.md §1.5). This is
// an EXEC surface: content (rule messages, which are attacker/team-
// controlled via a repo's own .wtcockpit.toml pack, not just tracked file
// bytes) must never be able to become code or shell syntax.
//
// This file holds the two pieces most load-bearing for that guarantee —
// buildArgvDarwin/buildArgvLinux and Sanitize — kept pure and dependency-free
// so they're directly testable without an OS notifier binary at all.
package notify

import (
	"strings"
	"unicode/utf8"
)

// titleCap and bodyCap are P5-design.md §1.5's frozen sanitization caps.
const (
	titleCap = 80
	bodyCap  = 200
)

// buildArgvDarwin returns the fixed osascript argv for one notification. The
// AppleScript program (the three -e lines) is a CONSTANT — title and body
// travel only as trailing argv elements, read at runtime via `item N of
// argv` inside the "on run argv" handler, and are never concatenated into
// the script text. No content, however hostile ("; rm -rf ~", "$(whoami)",
// backticks, ...), can therefore execute as AppleScript, and since
// exec.Command never invokes a shell, none of it can be reinterpreted as
// shell syntax either. Frozen shape (P5-design.md §1.5); tests golden it.
func buildArgvDarwin(title, body string) []string {
	return []string{
		"osascript",
		"-e", "on run argv",
		"-e", "display notification (item 2 of argv) with title (item 1 of argv)",
		"-e", "end run",
		title, body,
	}
}

// buildArgvLinux returns the fixed notify-send argv. urgency is "normal" or
// "critical" (danger hits get critical). The "--" end-of-flags marker is
// belt-and-braces on top of this package's own titles always starting with
// the constant "wt-cockpit" prefix (never bare user/rule content): even a
// title or body that happens to start with "-" can never be parsed as a
// notify-send flag. Frozen shape (P5-design.md §1.5); tests golden it.
func buildArgvLinux(title, body, urgency string) []string {
	return []string{
		"notify-send",
		"--app-name=wt-cockpit",
		"--urgency=" + urgency,
		"--",
		title,
		body,
	}
}

// Sanitize makes s safe to render as a desktop-notification field and to
// pass as a single inert argv element:
//
//   - runes below 0x20 and 0x7F (DEL) — which is what kills ESC/ANSI escape
//     sequences, BEL, and raw newlines — become a space;
//   - invalid UTF-8 decodes to the standard replacement rune (Go's `range`
//     over a string already does this; no separate pass is needed);
//   - consecutive whitespace collapses to a single space and the ends are
//     trimmed;
//   - the result is truncated to at most max runes, rune-safe (never
//     splitting a multi-byte rune), with a trailing "…" marking an actual
//     truncation.
//
// Exported so wt watch's OSC 9 path (P5-design.md §1.6) reuses this exact
// rule instead of a second, drifting implementation. Combined with the
// non-echo invariant (guardrail hit Messages never carry matched content),
// no secret and no terminal-escape byte can reach a notifier or a terminal.
func Sanitize(s string, max int) string {
	var b strings.Builder
	b.Grow(len(s))
	lastWasSpace := false
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			r = ' '
		}
		if r == ' ' || r == '\t' {
			if lastWasSpace {
				continue
			}
			lastWasSpace = true
			b.WriteByte(' ')
			continue
		}
		lastWasSpace = false
		b.WriteRune(r)
	}
	return truncateRunes(strings.TrimSpace(b.String()), max)
}

// truncateRunes caps s at max runes, rune-safe, appending "…" (and dropping
// one more rune to make room for it) only when s was actually cut.
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	if max == 1 {
		return "…"
	}
	runes := []rune(s)
	return string(runes[:max-1]) + "…"
}
