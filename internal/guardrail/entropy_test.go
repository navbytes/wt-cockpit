package guardrail

import (
	"math"
	"testing"

	"github.com/navbytes/wt-cockpit/internal/model"
)

// approxEqual compares floats with a small tolerance — entropy math involves
// log2, so exact equality isn't the right bar even for hand-computed vectors.
func approxEqual(a, b, tol float64) bool {
	return math.Abs(a-b) <= tol
}

// TestShannonEntropyHandComputedVectors pins shannonEntropy against
// hand-computed values for uniform distributions (H = log2(k) exactly, for k
// distinct equally-frequent symbols) plus a skewed, low-entropy case computed
// by hand from letter frequencies.
func TestShannonEntropyHandComputedVectors(t *testing.T) {
	cases := []struct {
		name string
		s    string
		want float64
		tol  float64
	}{
		{"single repeated byte: zero entropy", "AAAAAAAA", 0, 1e-9},
		{"two symbols, 50/50 (aabb): H=1.0", "aabb", 1.0, 1e-9},
		{"eight distinct symbols, uniform: H=log2(8)=3.0", "ABCDEFGH", 3.0, 1e-9},
		// A 16-symbol hex alphabet, every digit exactly once: H=log2(16)=4.0 —
		// the mathematical CEILING for any hex-only string (at most 16
		// distinct symbols), which is exactly why a 4.8 threshold structurally
		// ignores git SHAs/checksums no matter how "random" they look.
		{"hex alphabet, all 16 digits once each: H=log2(16)=4.0 (hex ceiling)", "0123456789abcdef", 4.0, 1e-9},
		// 32 distinct symbols from the token alphabet, uniform: H=log2(32)=5.0
		// — a clean base64-shaped high-entropy vector that clears the 4.8
		// secrets-entropy threshold.
		{"32 distinct base64-alphabet symbols, uniform: H=log2(32)=5.0", "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdef", 5.0, 1e-9},
		// "helloworld": h1 e1 l3 o2 w1 r1 d1 (n=10) — hand-computed low-entropy
		// natural-language-shaped case, well clear of the 4.8 threshold.
		{"english word 'helloworld': hand-computed ~2.646 bits/char", "helloworld", 2.6464393446710154, 1e-6},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := shannonEntropy(c.s)
			if !approxEqual(got, c.want, c.tol) {
				t.Errorf("shannonEntropy(%q) = %v, want %v (+/- %v)", c.s, got, c.want, c.tol)
			}
		})
	}
}

// TestShannonEntropyHexNeverReachesThreshold is the structural property the
// secrets-entropy default rule's 4.8 threshold relies on: ANY string drawn
// only from the 16-symbol hex alphabet has entropy <= log2(16) = 4.0,
// regardless of content — so a git SHA or checksum can never trip it, however
// "random" the specific hex digits are.
func TestShannonEntropyHexNeverReachesThreshold(t *testing.T) {
	hexish := []string{
		"0123456789abcdef0123456789abcdef", // one of every digit, twice
		"deadbeefdeadbeefdeadbeefdeadbeef",
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", // degenerate, even lower
		"5f4dcc3b5aa765d61d8327deb882cf99", // md5-shaped
	}
	for _, s := range hexish {
		if h := shannonEntropy(s); h > 4.0+1e-9 {
			t.Errorf("shannonEntropy(%q) = %v, want <= 4.0 (hex ceiling)", s, h)
		}
	}
}

// TestShannonEntropyUUIDShapedNeverReachesThreshold extends the hex-ceiling
// property to a dash-containing shape a real secret scanner is far more
// likely to actually be handed: a UUID. '-' is itself a token-alphabet byte
// (isTokenByte), so a UUID scans as ONE 36-byte token, not five separate
// ones — its alphabet is hex plus '-' (17 symbols), so its entropy ceiling is
// log2(17)~=4.087, still comfortably under the shipped 4.8 threshold.
func TestShannonEntropyUUIDShapedNeverReachesThreshold(t *testing.T) {
	uuids := []string{
		"550e8400-e29b-41d4-a716-446655440000",
		"f47ac10b-58cc-4372-a567-0e02b2c3d479",
		"00000000-0000-0000-0000-000000000000",
	}
	for _, u := range uuids {
		if h := shannonEntropy(u); h > 4.8 {
			t.Errorf("shannonEntropy(%q) = %v, want <= 4.8 (UUID/hex+dash ceiling log2(17)~=4.087)", u, h)
		}
	}
}

// TestUUIDShapedTokenDoesNotTripSecretsEntropyDefault is the end-to-end
// twin, run over the actual shipped DefaultRules(): a UUID in added content
// must never trip secrets-entropy — the false-positive boundary the PROBE
// calls out by name.
func TestUUIDShapedTokenDoesNotTripSecretsEntropyDefault(t *testing.T) {
	e := mustCompile(t, DefaultRules())
	diff := model.Diff{Files: []model.DiffFile{addedFile("app.go", `id := "550e8400-e29b-41d4-a716-446655440000"`)}}
	for _, h := range e.Eval(diff) {
		if h.Rule == "secrets-entropy" {
			t.Errorf("a UUID-shaped token should not trip secrets-entropy (hex+dash ceiling under 4.8), got %+v", h)
		}
	}
}

// TestEntropyThresholdBoundaryInclusiveGTE pins hasHighEntropyToken's
// comparison operator: a token whose entropy exactly equals the threshold
// must trip (the code uses ">=", not ">"), and a threshold a hair above the
// token's own entropy must not.
func TestEntropyThresholdBoundaryInclusiveGTE(t *testing.T) {
	token := "ABCDEFGHIJKLMNOPQRSTUVWXYZ0" // 27 distinct symbols, uniform: H = log2(27)
	h := shannonEntropy(token)
	if !hasHighEntropyToken(token, 8, h) {
		t.Errorf("a token whose entropy exactly equals the threshold must trip (>=, not >): H=%v", h)
	}
	if hasHighEntropyToken(token, 8, h+0.001) {
		t.Error("a threshold a hair above the token's own entropy must not trip")
	}
}

// TestEntropyJustBelowAndJustAboveSharedDefaultThreshold is the false-pos/
// false-neg boundary the PROBE calls out explicitly: two hand-computed
// tokens straddling the shipped 4.8 threshold as tightly as an exact
// integer-distinct-symbol construction allows (log2(27)~=4.755 just under,
// log2(28)~=4.807 just over), run through the full engine (not just
// shannonEntropy directly).
func TestEntropyJustBelowAndJustAboveSharedDefaultThreshold(t *testing.T) {
	below := "ABCDEFGHIJKLMNOPQRSTUVWXYZ0"  // 27 distinct symbols: H=log2(27)~=4.755
	above := "ABCDEFGHIJKLMNOPQRSTUVWXYZ01" // 28 distinct symbols: H=log2(28)~=4.807
	if h := shannonEntropy(below); h >= 4.8 {
		t.Fatalf("precondition: %q should have entropy < 4.8, got %v", below, h)
	}
	if h := shannonEntropy(above); h < 4.8 {
		t.Fatalf("precondition: %q should have entropy >= 4.8, got %v", above, h)
	}

	e := mustCompile(t, []Rule{{Name: "secret", MinTokenEntropy: 4.8, MinTokenLen: 8}})
	if hits := e.Eval(model.Diff{Files: []model.DiffFile{addedFile("a.go", `x := "`+below+`"`)}}); len(hits) != 0 {
		t.Errorf("a token just under the 4.8 threshold must not trip, got %+v", hits)
	}
	if hits := e.Eval(model.Diff{Files: []model.DiffFile{addedFile("b.go", `x := "`+above+`"`)}}); len(hits) != 1 {
		t.Errorf("a token just over the 4.8 threshold should trip, got %+v", hits)
	}
}

// TestTokensFindsMaximalRunsAtOrAboveMinLen pins the token scanner: runs of
// [A-Za-z0-9+/=_-] shorter than minLen are dropped, adjacent runs separated by
// a non-token byte (space, punctuation) are distinct tokens, and a run is
// reported in full (maximal), not truncated at minLen.
func TestTokensFindsMaximalRunsAtOrAboveMinLen(t *testing.T) {
	// "=", "+", "/", "_", "-" are themselves token bytes (base64 padding/URL-
	// safe alphabet), so separators here are space/colon/quote — bytes
	// actually outside [A-Za-z0-9+/=_-].
	s := `key: "` + fakeAWSKeyID + `" short:ab padded:zzzzzz`
	got := tokens(s, 8)
	found := map[string]bool{}
	for _, tok := range got {
		found[tok] = true
	}
	if !found[fakeAWSKeyID] {
		t.Errorf("tokens(%q, 8) = %v, want it to include the 20-byte run", s, got)
	}
	for _, short := range []string{"key", "short", "ab", "padded", "zzzzzz"} {
		if found[short] {
			t.Errorf("tokens(%q, 8) = %v, want %q (len %d < 8) excluded", s, got, short, len(short))
		}
	}
	if len(got) != 1 {
		t.Errorf("tokens(%q, 8) = %v, want exactly 1 qualifying run", s, got)
	}
}

// TestTokensMinLenBoundary: a run exactly minLen long is included; one byte
// short is not.
func TestTokensMinLenBoundary(t *testing.T) {
	if got := tokens("1234567", 8); len(got) != 0 {
		t.Errorf("tokens(7-byte run, minLen 8) = %v, want none", got)
	}
	if got := tokens("12345678", 8); len(got) != 1 || got[0] != "12345678" {
		t.Errorf("tokens(8-byte run, minLen 8) = %v, want [12345678]", got)
	}
}

// TestHasHighEntropyTokenRequiresBothLenAndEntropy: a token that clears the
// entropy bar but is too short must not count, and vice versa.
func TestHasHighEntropyTokenRequiresBothLenAndEntropy(t *testing.T) {
	highEntropyShort := "Ab3" // too short regardless of entropy
	if hasHighEntropyToken("x="+highEntropyShort, 8, 1.0) {
		t.Error("a token shorter than minLen must never count, however high its entropy")
	}

	longLowEntropy := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" // 32 chars, entropy 0
	if hasHighEntropyToken("x="+longLowEntropy, 8, 0.5) {
		t.Error("a long but low-entropy token must not count")
	}

	longHighEntropy := "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdef" // 32 distinct chars, H=5.0
	if !hasHighEntropyToken("prefix "+longHighEntropy+" suffix", 32, 4.8) {
		t.Error("a token clearing both the length floor and the entropy threshold should count")
	}
}
