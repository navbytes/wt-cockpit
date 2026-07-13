package tui

import (
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// TestKeybarsFitEightyColumnBudget pins the ux-expert P2-2 budget: every
// keybar variant must fit within 78 display columns — radarKeybar was
// previously 86 cols wide (double-spaced group separators), which wraps
// onto a second line on an 80-col terminal, the narrowest width the smoke
// matrix exercises. Covers every variant, not just the one that was over
// budget, so a future addition to any of them regresses loudly.
func TestKeybarsFitEightyColumnBudget(t *testing.T) {
	const budget = 78
	variants := map[string]string{
		"radarKeybar":  radarKeybar,
		"reviewKeybar": reviewKeybar,
		"diffKeybar":   diffKeybar,
	}
	for name, s := range variants {
		if w := lipgloss.Width(s); w > budget {
			t.Errorf("%s width = %d, want <= %d: %q", name, w, budget, s)
		}
	}
}
