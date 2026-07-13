package guardrail

import (
	"strings"
	"testing"
	"time"
)

// TestGlobMatchSemantics pins GlobMatch's documented behaviour (literal
// bytes, "*" bounded to one path segment, "**" crossing slashes including a
// bare trailing/leading "**", "?" as a single non-slash byte, and anchoring
// to the whole path) — the correctness backstop for BLOCKER-1's rewrite from
// a recursive backtracking matcher to a bounded DP scan: matching behaviour
// must not change, only how it's computed.
func TestGlobMatchSemantics(t *testing.T) {
	cases := []struct {
		pattern, path string
		want          bool
	}{
		{"go.mod", "go.mod", true},
		{"go.mod", "sub/go.mod", false}, // literal, not anchored to basename
		{"migrations/**", "migrations/014_drop.sql", true},
		{"migrations/**", "migrations/nested/dir/file.sql", true},
		{"migrations/**", "src/app.ts", false},
		{"**/migrations/**", "a/b/migrations/x.sql", true},
		{"**/*.lock", "yarn.lock", true}, // leading "**/" also matches zero directories
		{"**/*.lock", "sub/dir/yarn.lock", true},
		{"a/**/b", "a/b", true},   // "**" between slashes matches zero segments too
		{"a/**/b", "a/x/b", true}, // ...or one
		{"a/**/b", "a/x/y/b", true},
		{"a/**/b", "a/", false},
		{".github/workflows/*", ".github/workflows/ci.yml", true},
		{".github/workflows/*", ".github/workflows/nested/ci.yml", false}, // "*" doesn't cross "/"
		{"*.go", "app.go", true},
		{"*.go", "dir/app.go", false},
		{"file?.txt", "file1.txt", true},
		{"file?.txt", "file12.txt", false},
		{"file?.txt", "file/.txt", false}, // "?" must not match "/"
		{"**", "anything/at/all", true},
		{"**", "", true},
		{"", "", true},
		{"", "x", false},
		{"abc", "abx", false},
		{"a*b*c", "aXXbYYc", true},
		{"a*b*c", "aXXbYY", false},
	}
	for _, c := range cases {
		if got := GlobMatch(c.pattern, c.path); got != c.want {
			t.Errorf("GlobMatch(%q, %q) = %v, want %v", c.pattern, c.path, got, c.want)
		}
	}
}

// timedGlobMatch runs GlobMatch on its own goroutine and reports whether it
// returned within budget — used so a regression back to the old exponential
// matcher fails this test via a clean timeout rather than wedging `go test`
// itself indefinitely (the leaked goroutine is harmless test-process noise).
func timedGlobMatch(pattern, path string, budget time.Duration) (result, inBudget bool) {
	done := make(chan bool, 1)
	go func() { done <- GlobMatch(pattern, path) }()
	select {
	case r := <-done:
		return r, true
	case <-time.After(budget):
		return false, false
	}
}

// TestGlobMatchPathologicalPatternDoesNotHang is BLOCKER-1's regression pin:
// a pack glob is semi-trusted (checked-in .wtcockpit.toml) input, evaluated
// on every refresh under refreshMu — a pattern like repeated "**a" (or even
// repeated "*a") against a long NON-matching path made the old recursive
// "try every split point" globHelper run superpolynomially, wedging the
// refresh goroutine (and so every refresh + Approve behind refreshMu) on a
// single Compile'd rule. Against the old matcher this exact case measurably
// exceeds the budget below (verified while diagnosing the bug); the bounded
// DP matcher returns in well under a millisecond.
func TestGlobMatchPathologicalPatternDoesNotHang(t *testing.T) {
	const budget = 2 * time.Second
	cases := []struct {
		name, pattern, path string
	}{
		{"double-star chain", strings.Repeat("**a", 30) + "b", strings.Repeat("a", 40)},
		{"single-star chain", strings.Repeat("*a", 30) + "b", strings.Repeat("a", 40)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			result, ok := timedGlobMatch(c.pattern, c.path, budget)
			if !ok {
				t.Fatalf("GlobMatch did not return within %v on a pathological pattern (exponential blowup regression)", budget)
			}
			if result {
				t.Errorf("pattern %q should not match %q", c.pattern, c.path)
			}
		})
	}
}
