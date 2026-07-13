package guardrail

import "math"

// tokens returns every maximal run of [A-Za-z0-9+/=_-] in s with length >=
// minLen — the "candidate secret" shape the entropy condition scores
// (P5-design.md §1.1). A plain byte scan rather than a compiled regexp: the
// character class is fixed and minLen varies per rule, so this avoids
// building a new pattern per rule/eval.
func tokens(s string, minLen int) []string {
	var out []string
	start := -1
	for i := 0; i <= len(s); i++ {
		var isTok bool
		if i < len(s) {
			isTok = isTokenByte(s[i])
		}
		switch {
		case isTok && start < 0:
			start = i
		case !isTok && start >= 0:
			if i-start >= minLen {
				out = append(out, s[start:i])
			}
			start = -1
		}
	}
	return out
}

func isTokenByte(b byte) bool {
	switch {
	case b >= 'A' && b <= 'Z', b >= 'a' && b <= 'z', b >= '0' && b <= '9':
		return true
	case b == '+' || b == '/' || b == '=' || b == '_' || b == '-':
		return true
	default:
		return false
	}
}

// shannonEntropy computes H = -Σ p·log2(p) over s's byte distribution
// (bits/char, per P5-design.md §1.1). Hex digests (0-9a-f) max out at 4.0
// bits/char — never enough on their own to clear a 4.8 threshold — which is
// exactly what makes the shipped secrets-entropy rule structurally ignore
// git SHAs and checksums.
func shannonEntropy(s string) float64 {
	if len(s) == 0 {
		return 0
	}
	var freq [256]int
	for i := 0; i < len(s); i++ {
		freq[s[i]]++
	}
	n := float64(len(s))
	var h float64
	for _, c := range freq {
		if c == 0 {
			continue
		}
		p := float64(c) / n
		h -= p * math.Log2(p)
	}
	return h
}

// hasHighEntropyToken reports whether s contains any candidate token (length
// >= minLen) whose Shannon entropy meets minEntropy.
func hasHighEntropyToken(s string, minLen int, minEntropy float64) bool {
	for _, tok := range tokens(s, minLen) {
		if shannonEntropy(tok) >= minEntropy {
			return true
		}
	}
	return false
}
