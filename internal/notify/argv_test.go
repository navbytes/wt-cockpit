package notify

// Pure argv-construction and Sanitize tests: no OS gating, no notifier
// binary needed. This is the security centerpiece of WP2 (P5-design.md
// §1.5) — every hostile-content case here proves content lands as exactly
// one inert trailing argv element, never split, never concatenated into a
// script or shell string.

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// hostileCase names one injection shape the brief calls out, paired with a
// short, readable subtest name — using the raw (possibly 10KB, possibly
// invalid-UTF-8) content itself as a t.Run name would make -v output
// unreadable.
type hostileCase struct {
	name string
	s    string
}

// hostileStrings covers every injection shape the brief calls out: a
// classic shell-metachar chain, command substitution (two syntaxes),
// newlines, a flag-looking leading dash, control/escape bytes, and a large
// body.
func hostileStrings() []hostileCase {
	return []hostileCase{
		{"shell metachar chain", `"; rm -rf ~`},
		{"command substitution dollar-paren", "$(whoami)"},
		{"command substitution backticks", "`whoami`"},
		{"newlines CR and LF", "line1\nline2\r\nline3"},
		{"leading dash looks like a flag", "-danger --urgency=critical"},
		{"OSC/BEL escape sequence", "\x1b]0;pwn\x07"},
		{"10KB body", strings.Repeat("A", 10*1024)},
		{"invalid UTF-8 bytes", "invalid utf8: \xff\xfe"},
		{"empty string", ""},
	}
}

// TestBuildArgvDarwinContentIsAlwaysExactlyTwoTrailingArgs proves the
// osascript argv is fixed regardless of content: the script (-e lines) never
// changes, and title/body always land as argv[4] and argv[5] verbatim — even
// completely unsanitized hostile input, since the argv shape itself (not
// sanitization) is what makes this safe. There is no shell in
// exec.CommandContext(argv[0], argv[1:]...), so none of this can ever be
// reinterpreted as shell syntax either.
func TestBuildArgvDarwinContentIsAlwaysExactlyTwoTrailingArgs(t *testing.T) {
	for _, c := range hostileStrings() {
		t.Run(c.name, func(t *testing.T) {
			s := c.s
			got := buildArgvDarwin(s, s)
			want := []string{
				"osascript",
				"-e", "on run argv",
				"-e", "display notification (item 2 of argv) with title (item 1 of argv)",
				"-e", "end run",
				s, s,
			}
			assertArgvEqual(t, got, want)
			// The AppleScript program itself must never contain the content —
			// proof that it wasn't concatenated into the script text.
			for _, part := range got[:len(got)-2] {
				if s != "" && strings.Contains(part, s) {
					t.Errorf("script argv element %q must not contain the raw content %q", part, s)
				}
			}
		})
	}
}

// TestBuildArgvLinuxContentIsAlwaysExactlyTwoTrailingArgsAfterDashDash pins
// the same guarantee for notify-send, plus the "--" end-of-flags guard: even
// a title starting with "-" (which would otherwise look like a flag to
// notify-send) lands as an inert trailing argv element after "--".
func TestBuildArgvLinuxContentIsAlwaysExactlyTwoTrailingArgsAfterDashDash(t *testing.T) {
	for _, c := range hostileStrings() {
		t.Run(c.name, func(t *testing.T) {
			s := c.s
			got := buildArgvLinux(s, s, "critical")
			want := []string{
				"notify-send",
				"--app-name=wt-cockpit",
				"--urgency=critical",
				"--",
				s, s,
			}
			assertArgvEqual(t, got, want)
			if got[3] != "--" {
				t.Fatalf("argv[3] = %q, want the end-of-flags guard %q", got[3], "--")
			}
		})
	}
}

// TestBuildArgvLinuxUrgencyIsNotSanitizedButAlwaysCallerControlled pins that
// urgency (never user/rule content — always the literal "normal" or
// "critical" fire() passes) lands in its own flag slot untouched.
func TestBuildArgvLinuxUrgencyControlsTheFlag(t *testing.T) {
	got := buildArgvLinux("t", "b", "normal")
	if got[2] != "--urgency=normal" {
		t.Errorf("argv[2] = %q, want --urgency=normal", got[2])
	}
	got = buildArgvLinux("t", "b", "critical")
	if got[2] != "--urgency=critical" {
		t.Errorf("argv[2] = %q, want --urgency=critical", got[2])
	}
}

func assertArgvEqual(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("argv = %q (len %d), want %q (len %d)", got, len(got), want, len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("argv[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// ---- Sanitize ----

func TestSanitizeStripsControlAndEscapeBytes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"newlines become spaces then collapse", "line1\nline2\r\nline3", "line1 line2 line3"},
		{"ESC/OSC/BEL sequence neutralized", "\x1b]0;pwn\x07", "]0;pwn"},
		{"DEL byte neutralized", "a\x7fb", "a b"},
		{"tabs collapse like spaces", "a\t\tb", "a b"},
		{"plain text passes through untouched", "touches database migrations", "touches database migrations"},
		{"empty string stays empty", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Sanitize(c.in, 200); got != c.want {
				t.Errorf("Sanitize(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestSanitizeNeverEmitsAControlByte is the property version of the table
// above: for every hostile input, the result must contain no rune below
// 0x20 and no 0x7F, full stop.
func TestSanitizeNeverEmitsAControlByte(t *testing.T) {
	for _, c := range hostileStrings() {
		got := Sanitize(c.s, 300)
		for _, r := range got {
			if r < 0x20 || r == 0x7f {
				t.Errorf("Sanitize(%q) [%s] = %q contains control byte %U", c.s, c.name, got, r)
			}
		}
	}
}

func TestSanitizeReplacesInvalidUTF8(t *testing.T) {
	got := Sanitize("invalid utf8: \xff\xfe end", 200)
	if !utf8.ValidString(got) {
		t.Errorf("Sanitize output %q is not valid UTF-8", got)
	}
	if !strings.Contains(got, "invalid utf8:") || !strings.Contains(got, "end") {
		t.Errorf("Sanitize(%q) = %q, want the surrounding text preserved", "invalid utf8: \xff\xfe end", got)
	}
}

func TestSanitizeTruncatesRuneSafeWithEllipsis(t *testing.T) {
	long := strings.Repeat("A", 300)
	got := Sanitize(long, 80)
	if n := len([]rune(got)); n != 80 {
		t.Fatalf("Sanitize truncated length = %d runes, want 80", n)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("Sanitize(%q) = %q, want a trailing ellipsis marking truncation", long, got)
	}
	if !utf8.ValidString(got) {
		t.Errorf("truncated output %q is not valid UTF-8 (rune split)", got)
	}
}

// TestSanitizeTruncationIsRuneSafeOnMultiByteContent proves truncation never
// splits a multi-byte rune even when the cut falls mid-character.
func TestSanitizeTruncationIsRuneSafeOnMultiByteContent(t *testing.T) {
	long := strings.Repeat("é", 100) // each é is 2 bytes in UTF-8
	got := Sanitize(long, 10)
	if !utf8.ValidString(got) {
		t.Fatalf("Sanitize(%q, 10) = %q is not valid UTF-8", long, got)
	}
	if n := len([]rune(got)); n != 10 {
		t.Errorf("Sanitize truncated length = %d runes, want 10", n)
	}
}

func TestSanitizeUnderCapReturnsUnchangedContent(t *testing.T) {
	if got := Sanitize("short", 80); got != "short" {
		t.Errorf("Sanitize(%q, 80) = %q, want unchanged", "short", got)
	}
}

func TestSanitizeDefaultCapsMatchDesignedConstants(t *testing.T) {
	if titleCap != 80 {
		t.Errorf("titleCap = %d, want 80 (P5-design.md §1.5)", titleCap)
	}
	if bodyCap != 200 {
		t.Errorf("bodyCap = %d, want 200 (P5-design.md §1.5)", bodyCap)
	}
}
