package tui

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/navbytes/wt-cockpit/internal/model"
)

// ---- highlightCache: hit / miss / eviction ----

func TestHighlightCacheMissThenHitAfterPut(t *testing.T) {
	c := newHighlightCache()
	if _, ok := c.get("h1"); ok {
		t.Fatal("get on an empty cache should miss")
	}
	c.put("h1", []string{"a", "b"})
	got, ok := c.get("h1")
	if !ok {
		t.Fatal("get after put should hit")
	}
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("get returned %v, want [a b]", got)
	}
}

// TestHighlightCacheEvictsLeastRecentlyUsedPastByteCap pins the "byte-
// accounted, 16MB-capped LRU" contract at a tiny synthetic cap so the test
// runs instantly.
func TestHighlightCacheEvictsLeastRecentlyUsedPastByteCap(t *testing.T) {
	c := newHighlightCache()
	c.maxBytes = 10 // tiny, so a couple of small entries force eviction

	c.put("old", []string{"aaaa"}) // 4 bytes, least recently used: never touched again until the final assertions
	c.put("mid", []string{"bbbb"}) // 4 bytes, total 8
	c.put("new", []string{"cc"})   // 2 bytes, total 10: exactly at cap, no eviction yet
	if c.bytes != 10 {
		t.Fatalf("precondition: cache bytes = %d, want 10 (exactly at cap, nothing evicted yet)", c.bytes)
	}

	c.put("push", []string{"dddd"}) // 4 bytes: total would be 14, over cap by 4 -> evicts the LRU entry
	if _, ok := c.get("old"); ok {
		t.Error("old should have been evicted as the least-recently-used entry")
	}
	if _, ok := c.get("mid"); !ok {
		t.Error("mid was inserted after old and should have survived")
	}
	if _, ok := c.get("new"); !ok {
		t.Error("new should have survived")
	}
	if _, ok := c.get("push"); !ok {
		t.Error("push was just inserted and should be present")
	}
	if c.bytes > c.maxBytes {
		t.Errorf("cache bytes = %d, want <= cap %d", c.bytes, c.maxBytes)
	}
}

func TestHighlightCachePutOverwritesExistingHashAndRecomputesSize(t *testing.T) {
	c := newHighlightCache()
	c.put("h1", []string{"short"})
	c.put("h1", []string{"a", "much", "longer", "replacement"})
	got, ok := c.get("h1")
	if !ok || len(got) != 4 {
		t.Fatalf("get after overwrite = %v, ok=%v, want the 4-line replacement", got, ok)
	}
	wantBytes := len("a") + len("much") + len("longer") + len("replacement")
	if c.bytes != wantBytes {
		t.Errorf("cache bytes = %d, want %d (overwrite must not double-count the old entry)", c.bytes, wantBytes)
	}
}

// TestNewHighlightCacheDefaultCapMatchesDesignBudget pins the *production*
// default against P3-design.md §6's budget-table row ("Highlight memory
// ≤ 16 MB cache") — the eviction tests above deliberately use a tiny
// synthetic cap for speed, so nothing else in this file asserts the real
// constant a shipped binary actually runs with hasn't drifted.
func TestNewHighlightCacheDefaultCapMatchesDesignBudget(t *testing.T) {
	c := newHighlightCache()
	const want = 16 * 1024 * 1024
	if c.maxBytes != want {
		t.Errorf("newHighlightCache().maxBytes = %d, want %d (16MB, P3-design.md §6)", c.maxBytes, want)
	}
}

// TestHighlightCacheConcurrentAccessIsRaceFree exercises the cache the way
// real usage does (many files' highlightCmds landing around the same time)
// under `go test -race`.
func TestHighlightCacheConcurrentAccessIsRaceFree(t *testing.T) {
	c := newHighlightCache()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		i := i
		wg.Add(2)
		go func() {
			defer wg.Done()
			hash := fmt.Sprintf("file-%d", i%5)
			c.put(hash, []string{"line one", "line two"})
		}()
		go func() {
			defer wg.Done()
			hash := fmt.Sprintf("file-%d", i%5)
			c.get(hash)
		}()
	}
	wg.Wait()
}

// ---- chroma eligibility / threshold ----

func TestChromaTooLargeByLineCount(t *testing.T) {
	lines := make([]model.Line, chromaMaxLines+1)
	for i := range lines {
		lines[i] = line(model.LineAdd, 0, i+1, "x")
	}
	f := model.DiffFile{Hunks: []model.Hunk{{Lines: lines}}}
	if !chromaTooLarge(f) {
		t.Error("a file over chromaMaxLines should be too large for chroma")
	}
}

func TestChromaTooLargeByByteSize(t *testing.T) {
	big := strings.Repeat("x", chromaMaxBytes+1)
	f := model.DiffFile{Hunks: []model.Hunk{{Lines: []model.Line{line(model.LineAdd, 0, 1, big)}}}}
	if !chromaTooLarge(f) {
		t.Error("a file over chromaMaxBytes should be too large for chroma")
	}
}

func TestChromaNotTooLargeForAnOrdinarySmallFile(t *testing.T) {
	f := model.DiffFile{Hunks: []model.Hunk{{Lines: []model.Line{line(model.LineAdd, 0, 1, "package a")}}}}
	if chromaTooLarge(f) {
		t.Error("a small file should not be flagged too large for chroma")
	}
}

// TestChromaFormatterDegradesWithColorProfile pins §2.6/§2.5's "chroma
// formatter chosen by the same detected profile" — including that Ascii
// (NO_COLOR / the profile tests force) has no formatter to degrade to.
func TestChromaFormatterDegradesWithColorProfile(t *testing.T) {
	cases := []struct {
		profile termenv.Profile
		wantOK  bool
	}{
		{termenv.TrueColor, true},
		{termenv.ANSI256, true},
		{termenv.ANSI, true},
		{termenv.Ascii, false},
	}
	for _, c := range cases {
		_, ok := chromaFormatter(c.profile)
		if ok != c.wantOK {
			t.Errorf("chromaFormatter(%s) ok = %v, want %v", c.profile.Name(), ok, c.wantOK)
		}
	}
}

// ---- concatFileContent ----

func TestConcatFileContentJoinsHunkLinesWithNewlines(t *testing.T) {
	f := model.DiffFile{Hunks: []model.Hunk{
		{Lines: []model.Line{line(model.LineContext, 1, 1, "a"), line(model.LineAdd, 0, 2, "b")}},
		{Lines: []model.Line{line(model.LineDel, 3, 0, "c")}},
	}}
	got := concatFileContent(f)
	want := "a\nb\nc\n"
	if got != want {
		t.Errorf("concatFileContent = %q, want %q", got, want)
	}
}

// ---- tintRow: the reset-reinjection trick ----

// TestTintRowSurvivesEmbeddedResets pins the delta trick's whole point: a
// chroma-style line with several internal ESC[0m resets (one per token) must
// still show the row's background across the *entire* line, not just up to
// the first reset.
func TestTintRowSurvivesEmbeddedResets(t *testing.T) {
	const prefix = "\x1b[48;2;1;2;3m"
	chromaLine := "\x1b[38;2;255;0;0mfoo" + chromaReset + "bar" + "\x1b[38;2;0;255;0mbaz" + chromaReset
	got := tintRow(prefix, chromaLine)

	if !strings.HasPrefix(got, prefix) {
		t.Fatalf("tinted row = %q, want it to start with the background prefix", got)
	}
	if !strings.HasSuffix(got, chromaReset) {
		t.Errorf("tinted row = %q, want it to end with a final reset (no bleed into the next row)", got)
	}
	// Every reset in the original line must be immediately followed by the
	// background prefix again, except (structurally) the row's own final
	// reset, which closes everything instead.
	wantReopens := strings.Count(chromaLine, chromaReset) // one reopen per original reset
	gotReopens := strings.Count(got, chromaReset+prefix)
	if gotReopens != wantReopens {
		t.Errorf("reopen count = %d, want %d (once per embedded reset in %q)", gotReopens, wantReopens, chromaLine)
	}
	// The visible text itself must be untouched.
	plain := stripANSI(got)
	if plain != "foobarbaz" {
		t.Errorf("visible text = %q, want %q (tinting must not alter content)", plain, "foobarbaz")
	}
}

func TestTintRowNoOpWhenPrefixEmpty(t *testing.T) {
	line := "plain text, no color"
	if got := tintRow("", line); got != line {
		t.Errorf("tintRow with empty prefix = %q, want the row unchanged", got)
	}
}

func TestBgPrefixEmptyUnderAsciiProfile(t *testing.T) {
	saved := lipgloss.ColorProfile()
	defer lipgloss.SetColorProfile(saved)
	lipgloss.SetColorProfile(termenv.Ascii)
	if got := bgPrefix(styles.AddBg); got != "" {
		t.Errorf("bgPrefix under Ascii = %q, want empty (nothing to reinject)", got)
	}
}

func TestBgPrefixNonEmptyUnderTrueColor(t *testing.T) {
	saved := lipgloss.ColorProfile()
	defer lipgloss.SetColorProfile(saved)
	lipgloss.SetColorProfile(termenv.TrueColor)
	if got := bgPrefix(styles.AddBg); got == "" {
		t.Error("bgPrefix under TrueColor should produce a non-empty escape prefix")
	}
}

// ---- ensureHighlightCmds / visibleFileIndices ----

func TestVisibleFileIndicesDedupesInFirstSeenOrder(t *testing.T) {
	lines := []renderLine{
		{fileIdx: 0}, {fileIdx: 0}, {fileIdx: 1}, {fileIdx: 1}, {fileIdx: 2},
	}
	got := visibleFileIndices(lines, 1, 3) // rows 1,2,3 -> files 0,1,1
	want := []int{0, 1}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("visibleFileIndices = %v, want %v", got, want)
	}
}

func TestEnsureHighlightCmdsSkipsBinaryTooLargeAndUnrecognizedFiles(t *testing.T) {
	d := model.Diff{Files: []model.DiffFile{
		{Path: "logo.png", Binary: true, Hash: "hbin"},
		{Path: "unknown.zzzznotalanguage", Hash: "hunk"},
	}}
	v := newTestPane(d)
	v.setHeight(10)
	hl := newHighlightCache()
	pending := map[string]bool{}

	cmd := ensureHighlightCmds(&v, hl, pending)
	if cmd != nil {
		t.Errorf("expected no highlight commands for binary/unrecognized files, got a non-nil Cmd")
	}
	if len(pending) != 0 {
		t.Errorf("pending = %v, want empty (nothing eligible was dispatched)", pending)
	}
}

func TestEnsureHighlightCmdsDispatchesOnceThenSkipsWhilePending(t *testing.T) {
	d := model.Diff{Files: []model.DiffFile{
		{Path: "main.go", Hash: "hgo", Hunks: []model.Hunk{{Lines: []model.Line{line(model.LineAdd, 0, 1, "package main")}}}},
	}}
	v := newTestPane(d)
	v.setHeight(10)
	hl := newHighlightCache()
	pending := map[string]bool{}

	cmd := ensureHighlightCmds(&v, hl, pending)
	if cmd == nil {
		t.Fatal("expected a highlight command for an eligible, recognized file")
	}
	if !pending["hgo"] {
		t.Error("hgo should be marked pending after dispatch")
	}

	// A second check before the result lands must not dispatch again.
	if again := ensureHighlightCmds(&v, hl, pending); again != nil {
		t.Error("expected no additional command while the file is still pending")
	}
}

func TestEnsureHighlightCmdsSkipsFilesAlreadyCached(t *testing.T) {
	d := model.Diff{Files: []model.DiffFile{
		{Path: "main.go", Hash: "hgo", Hunks: []model.Hunk{{Lines: []model.Line{line(model.LineAdd, 0, 1, "package main")}}}},
	}}
	v := newTestPane(d)
	v.setHeight(10)
	hl := newHighlightCache()
	hl.put("hgo", []string{"package main"})
	pending := map[string]bool{}

	if cmd := ensureHighlightCmds(&v, hl, pending); cmd != nil {
		t.Error("expected no command for a file whose highlight is already cached")
	}
}

// TestHighlightCmdProducesLinesMatchingRealChromaOutput is an integration
// check of the real async path end to end: dispatch, run the returned
// tea.Cmd (as bubbletea would, off the calling goroutine), and apply the
// resulting message through the same code Update() uses.
func TestHighlightCmdProducesLinesMatchingRealChromaOutput(t *testing.T) {
	saved := lipgloss.ColorProfile()
	defer lipgloss.SetColorProfile(saved)
	lipgloss.SetColorProfile(termenv.TrueColor)

	r := newRadarView()
	d := model.Diff{WorktreeID: "w1", Files: []model.DiffFile{
		{Path: "main.go", Hash: "hgo", Hunks: []model.Hunk{{Lines: []model.Line{
			line(model.LineContext, 1, 1, "package main"),
			line(model.LineAdd, 0, 2, "// hi"),
		}}}},
	}}
	r.pane.setDiff(d)
	r.pane.setHeight(10)

	cmd := r.ensureHighlightsCmd()
	if cmd == nil {
		t.Fatal("expected a highlight command")
	}
	// Exactly one file is eligible here, and tea.Batch (see bubbletea's
	// compactCmds) returns a lone command directly rather than wrapping it
	// in a BatchMsg — so running cmd() yields the highlightedMsg itself.
	hmRaw := cmd()
	hm, ok := hmRaw.(highlightedMsg)
	if !ok {
		t.Fatalf("expected a highlightedMsg, got %T", hmRaw)
	}
	if hm.FileHash != "hgo" {
		t.Errorf("FileHash = %q, want hgo", hm.FileHash)
	}
	if len(hm.Lines) != 2 {
		t.Fatalf("Lines = %v, want 2 entries (one per source line)", hm.Lines)
	}
	if !strings.Contains(hm.Lines[0], "package") || !strings.Contains(stripANSI(hm.Lines[0]), "package main") {
		t.Errorf("Lines[0] = %q, want it to contain the highlighted \"package main\"", hm.Lines[0])
	}

	r.applyHighlighted(hm)
	if pending := r.pending["hgo"]; pending {
		t.Error("pending should be cleared once the highlight result is applied")
	}
	if _, ok := r.hl.get("hgo"); !ok {
		t.Error("cache should now hold hgo's highlighted lines")
	}
}
