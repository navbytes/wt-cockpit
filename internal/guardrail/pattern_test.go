package guardrail

import (
	"regexp"
	"strings"
	"testing"
)

// matchesAny is the same "OR of every branch" check compiledRule.addedPatternMatches
// does, used here to compare against a single combined regexp's own MatchString.
func matchesAny(res []*regexp.Regexp, s string) bool {
	for _, re := range res {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

// TestSplitAlternationMatchesCombinedRegexp is the correctness backstop for
// the eval-cost optimisation in compileAddedPattern: splitting a top-level
// alternation into independently-compiled branches must never change which
// strings match, only how fast the check runs. This is a basic regular-
// language identity (L(A|B|C) = L(A)∪L(B)∪L(C)), pinned here against the
// exact shipped secrets-pattern alternation plus a battery of inputs,
// including ones designed to trip each individual branch.
func TestSplitAlternationMatchesCombinedRegexp(t *testing.T) {
	pattern := strings.Join([]string{
		`AKIA[0-9A-Z]{16}`,
		`-----BEGIN [A-Z ]*PRIVATE KEY-----`,
		`ghp_[A-Za-z0-9]{36}`,
		`github_pat_[A-Za-z0-9_]{22,}`,
		`sk-[A-Za-z0-9]{20,}`,
		`xox[bpars]-[A-Za-z0-9-]{10,}`,
		`AIza[0-9A-Za-z_-]{35}`,
	}, "|")
	combined := regexp.MustCompile(pattern)
	split := splitAlternation(pattern)
	// Note: regexp/syntax's own parser factors out a shared literal prefix
	// ("g") between ghp_... and github_pat_..., so this 7-pattern alternation
	// parses to 6 top-level branches, not 7 — an internal parser optimisation,
	// not a bug in the split. The property that matters (asserted below) is
	// behavioural equivalence with the combined regexp, not the exact count.
	if len(split) <= 1 {
		t.Fatalf("splitAlternation should split this top-level alternation into multiple branches, got %d", len(split))
	}

	inputs := []string{
		"",
		"nothing suspicious here",
		`key := "` + fakeAWSKeyID + `"`, // AWS access key shape
		"-----BEGIN RSA PRIVATE KEY-----",
		"-----BEGIN PRIVATE KEY-----",
		"ghp_" + strings.Repeat("a", 36),
		"github_pat_" + strings.Repeat("a", 22),
		"sk-" + strings.Repeat("a", 20),
		"xoxb-" + strings.Repeat("1", 10),
		"xoxp-" + strings.Repeat("1", 10),
		"AIza" + strings.Repeat("a", 35),
		"AKIA_too_short", // looks close but doesn't match
		"ghp_tooshort",   // below the 36-char requirement
		"just a normal line of Go code",
		"func main() { fmt.Println(\"hi\") }",
	}
	for _, in := range inputs {
		want := combined.MatchString(in)
		got := matchesAny(split, in)
		if want != got {
			t.Errorf("input %q: combined.MatchString=%v, split OR=%v (want equal)", in, want, got)
		}
	}
}

// TestSplitAlternationReturnsNilForNonAlternation: a pattern whose top-level
// structure ISN'T an alternation (no "|" at all, or "|" nested inside a
// group) must not be split — splitAlternation returns nil so the caller
// keeps using the single combined regexp unchanged.
func TestSplitAlternationReturnsNilForNonAlternation(t *testing.T) {
	cases := []string{
		`AKIA[0-9A-Z]{16}`,  // no alternation at all
		`(?:foo|bar)baz`,    // "|" is nested inside a group, not top-level
		`prefix(a|b)suffix`, // same, with a capturing group
		`^anchored$`,        // no alternation
	}
	for _, p := range cases {
		if got := splitAlternation(p); got != nil {
			t.Errorf("splitAlternation(%q) = %d branches, want nil (not a top-level alternation)", p, len(got))
		}
	}
}

// TestSplitAlternationTopLevelNestedCaseStillMatchesCorrectly is the
// behavioural pin for the "nested alternation, not split" case above: even
// though splitAlternation declines to split it, compileAddedPattern must
// still produce a matcher with identical behaviour to the plain combined
// regexp (it just falls back to the one-element slice).
func TestSplitAlternationTopLevelNestedCaseStillMatchesCorrectly(t *testing.T) {
	pattern := `prefix(?:foo|bar)suffix`
	res, err := compileAddedPattern(pattern)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 {
		t.Fatalf("a non-top-level alternation should compile to exactly 1 matcher, got %d", len(res))
	}
	for _, in := range []string{"prefixfoosuffix", "prefixbarsuffix", "prefixbazsuffix", ""} {
		want := regexp.MustCompile(pattern).MatchString(in)
		if got := matchesAny(res, in); got != want {
			t.Errorf("input %q: want %v, got %v", in, want, got)
		}
	}
}

// TestCompileAddedPatternRejectsInvalidRegex mirrors
// TestCompileRejectsNonCompilingAddedPattern at the compileAddedPattern unit
// level: an unparseable pattern must error, not panic.
func TestCompileAddedPatternRejectsInvalidRegex(t *testing.T) {
	if _, err := compileAddedPattern("(unterminated["); err == nil {
		t.Fatal("expected an error for an invalid pattern")
	}
}
