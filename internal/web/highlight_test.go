package web

import (
	"html/template"
	"strings"
	"sync"
	"testing"

	"github.com/alecthomas/chroma/v2/lexers"

	"github.com/navbytes/wt-cockpit/internal/model"
)

func hline(kind model.LineKind, content string) model.Line {
	return model.Line{Kind: kind, Content: content}
}

// ---- collapse / size thresholds (mirrors internal/tui/flatten_test.go and
// highlight_test.go's identical constants, restated for this package) ----

func TestIsCollapsedByDefaultBySizeThreshold(t *testing.T) {
	f := model.DiffFile{Path: "big.go", Stats: model.Stats{Add: 300, Del: 200}} // 500 > 400
	if !isCollapsedByDefault(f) {
		t.Error("a file with Add+Del > 400 should collapse by default")
	}
}

func TestIsCollapsedByDefaultByLockfileGlob(t *testing.T) {
	for _, p := range []string{"go.sum", "package-lock.json", "yarn.lock", "Cargo.lock", "vendor/foo.lock", "testdata/x.snap"} {
		f := model.DiffFile{Path: p, Stats: model.Stats{Add: 1, Del: 1}}
		if !isCollapsedByDefault(f) {
			t.Errorf("path %q should collapse by the lockfile/snapshot glob regardless of size", p)
		}
	}
}

func TestIsCollapsedByDefaultFalseForOrdinarySmallFile(t *testing.T) {
	f := model.DiffFile{Path: "small.go", Stats: model.Stats{Add: 3, Del: 1}}
	if isCollapsedByDefault(f) {
		t.Error("a small, non-lockfile file should not collapse by default")
	}
}

func TestHunkTooLargeByLineCount(t *testing.T) {
	lines := make([]model.Line, highlightMaxLines+1)
	for i := range lines {
		lines[i] = hline(model.LineAdd, "x")
	}
	f := model.DiffFile{Hunks: []model.Hunk{{Lines: lines}}}
	if !hunkTooLarge(f) {
		t.Error("a file over highlightMaxLines should be too large to highlight")
	}
}

func TestHunkTooLargeByByteSize(t *testing.T) {
	big := strings.Repeat("x", highlightMaxBytes+1)
	f := model.DiffFile{Hunks: []model.Hunk{{Lines: []model.Line{hline(model.LineAdd, big)}}}}
	if !hunkTooLarge(f) {
		t.Error("a file over highlightMaxBytes should be too large to highlight")
	}
}

func TestHunkNotTooLargeForOrdinaryFile(t *testing.T) {
	f := model.DiffFile{Hunks: []model.Hunk{{Lines: []model.Line{hline(model.LineAdd, "package a")}}}}
	if hunkTooLarge(f) {
		t.Error("a small file should not be flagged too large to highlight")
	}
}

// ---- concatFileContent ----

func TestConcatFileContentJoinsHunkLinesWithNewlines(t *testing.T) {
	f := model.DiffFile{Hunks: []model.Hunk{
		{Lines: []model.Line{hline(model.LineContext, "a"), hline(model.LineAdd, "b")}},
		{Lines: []model.Line{hline(model.LineDel, "c")}},
	}}
	got := concatFileContent(f)
	if want := "a\nb\nc\n"; got != want {
		t.Errorf("concatFileContent = %q, want %q", got, want)
	}
}

// ---- the two template.HTML producers ----

// TestChromaHTMLLinesEscapesTokensAndPreservesLineCount is the core
// chroma-classes-producer contract: one template.HTML per source line
// (chroma.SplitTokensIntoLines drives that 1:1 mapping), class attributes
// only (no inline style — WithClasses(true), P4-design.md §1.3's CSP
// story), and every token's text run through the escaper chroma's own HTML
// formatter uses (html.EscapeString, verified against v2.27.0).
func TestChromaHTMLLinesEscapesTokensAndPreservesLineCount(t *testing.T) {
	lexer := lexers.Match("main.go")
	if lexer == nil {
		t.Fatal("precondition: expected a lexer match for main.go")
	}
	src := "package main\n\nfunc f() { s := \"<script>\" }\n"
	lines, ok := chromaHTMLLines(lexer, src)
	if !ok {
		t.Fatal("expected chromaHTMLLines to succeed for ordinary Go source")
	}
	if len(lines) != 3 {
		t.Fatalf("lines = %d, want 3 (one per source line, blank line included)", len(lines))
	}
	for i, l := range lines {
		if strings.Contains(string(l), "style=") {
			t.Errorf("line %d = %q, want class attributes only (WithClasses(true)), no inline style", i, l)
		}
	}
	if !strings.Contains(string(lines[2]), "&lt;script&gt;") {
		t.Errorf("line 2 = %q, want the string literal's %q escaped to %q", lines[2], "<script>", "&lt;script&gt;")
	}
	if strings.Contains(string(lines[2]), "<script>") {
		t.Errorf("line 2 = %q, contains an unescaped <script> — chroma must escape every token", lines[2])
	}
	for i, l := range lines {
		if strings.Contains(string(l), "\n") {
			t.Errorf("line %d = %q, must not carry a trailing newline into the row (trimTrailingNewline)", i, l)
		}
	}
}

// TestChromaHTMLLinesUnknownLexerStillTerminates guards against a lexer
// panicking/erroring on content it can't classify: chroma's own lexers fall
// back to plain/error tokens rather than failing tokenization outright, so
// this should still succeed with a line-aligned result.
func TestChromaHTMLLinesMismatchedLexerStillLineAligned(t *testing.T) {
	lexer := lexers.Match("main.py") // content below is actually shell, a plausible misdetection
	if lexer == nil {
		t.Fatal("precondition: expected a lexer match for main.py")
	}
	src := "#!/bin/sh\nset -e\necho hi\n"
	lines, ok := chromaHTMLLines(lexer, src)
	if !ok {
		t.Fatal("expected chromaHTMLLines to still succeed for a mismatched lexer/content pair")
	}
	if len(lines) != 3 {
		t.Errorf("lines = %d, want 3 (a misdetected lexer must not drop/merge source lines)", len(lines))
	}
}

func TestPlainHTMLLinesEscapesAndCountsOneLinePerHunkLine(t *testing.T) {
	f := model.DiffFile{Hunks: []model.Hunk{
		{Lines: []model.Line{hline(model.LineAdd, `<img src=x onerror=alert(1)>`)}},
		{Lines: []model.Line{hline(model.LineDel, "plain text")}},
	}}
	lines := plainHTMLLines(f)
	if len(lines) != 2 {
		t.Fatalf("lines = %d, want 2", len(lines))
	}
	if strings.Contains(string(lines[0]), "<img") {
		t.Errorf("line 0 = %q, want the raw <img tag escaped", lines[0])
	}
	if !strings.Contains(string(lines[0]), "&lt;img") {
		t.Errorf("line 0 = %q, want html.EscapeString's &lt;img", lines[0])
	}
	if string(lines[1]) != "plain text" {
		t.Errorf("line 1 = %q, want unmodified plain text (nothing to escape)", lines[1])
	}
}

// ---- linesForFile: the size/lexer decision + cache integration ----

func TestLinesForFileHighlightsAnOrdinaryGoFile(t *testing.T) {
	a := &app{hl: newHighlightCache()}
	f := model.DiffFile{Path: "main.go", Hash: "h1", Hunks: []model.Hunk{{Lines: []model.Line{hline(model.LineAdd, "package main")}}}}
	lines, off := a.linesForFile(f)
	if off {
		t.Error("off = true, want false for a small ordinary file")
	}
	if len(lines) != 1 || !strings.Contains(string(lines[0]), "class=") {
		t.Errorf("lines = %v, want one chroma-classed line", lines)
	}
}

func TestLinesForFileFallsBackToPlainWhenTooLarge(t *testing.T) {
	a := &app{hl: newHighlightCache()}
	lines := make([]model.Line, highlightMaxLines+1)
	for i := range lines {
		lines[i] = hline(model.LineAdd, "package main")
	}
	f := model.DiffFile{Path: "big.go", Hash: "hbig", Hunks: []model.Hunk{{Lines: lines}}}
	got, off := a.linesForFile(f)
	if !off {
		t.Error("off = false, want true (over highlightMaxLines)")
	}
	if len(got) != len(lines) {
		t.Fatalf("lines = %d, want %d (still one entry per source line, just unhighlighted)", len(got), len(lines))
	}
	if strings.Contains(string(got[0]), "class=") {
		t.Errorf("line 0 = %q, want the plain escaped fallback (no chroma classes) once too large", got[0])
	}
}

func TestLinesForFileFallsBackToPlainWhenNoLexerMatches(t *testing.T) {
	a := &app{hl: newHighlightCache()}
	f := model.DiffFile{Path: "README.zzznotalanguage", Hash: "hreadme", Hunks: []model.Hunk{{Lines: []model.Line{hline(model.LineAdd, "hello")}}}}
	lines, off := a.linesForFile(f)
	// No lexer match is not a "large file" — the dim note is size-specific
	// (matches the TUI's identical distinction), so off must stay false.
	if off {
		t.Error("off = true, want false: a missing lexer match is not the same as 'too large'")
	}
	if len(lines) != 1 {
		t.Fatalf("lines = %d, want 1", len(lines))
	}
}

func TestLinesForFileUsesCacheOnSecondCall(t *testing.T) {
	a := &app{hl: newHighlightCache()}
	f := model.DiffFile{Path: "main.go", Hash: "hcached", Hunks: []model.Hunk{{Lines: []model.Line{hline(model.LineAdd, "package main")}}}}

	first, _ := a.linesForFile(f)
	if _, ok := a.hl.get("hcached"); !ok {
		t.Fatal("expected linesForFile to populate the cache under the file's hash")
	}
	second, _ := a.linesForFile(f)
	if len(first) != len(second) || (len(first) > 0 && first[0] != second[0]) {
		t.Errorf("second call = %v, want the identical cached result %v", second, first)
	}
}

// ---- highlightCache: hit / miss / eviction (mirrors internal/tui's identical cache) ----

func TestHighlightCacheMissThenHitAfterPut(t *testing.T) {
	c := newHighlightCache()
	if _, ok := c.get("h1"); ok {
		t.Fatal("get on an empty cache should miss")
	}
	c.put("h1", []template.HTML{"a", "b"}, false)
	got, ok := c.get("h1")
	if !ok {
		t.Fatal("get after put should hit")
	}
	if len(got.lines) != 2 || got.lines[0] != "a" || got.lines[1] != "b" {
		t.Errorf("get returned %v, want [a b]", got.lines)
	}
	if got.off {
		t.Error("off should round-trip as false")
	}
}

func TestHighlightCacheEvictsLeastRecentlyUsedPastByteCap(t *testing.T) {
	c := newHighlightCache()
	c.maxBytes = 10

	c.put("old", []template.HTML{"aaaa"}, false) // 4 bytes
	c.put("mid", []template.HTML{"bbbb"}, false) // 4 bytes, total 8
	c.put("new", []template.HTML{"cc"}, false)   // 2 bytes, total 10: exactly at cap
	if c.bytes != 10 {
		t.Fatalf("precondition: cache bytes = %d, want 10", c.bytes)
	}

	c.put("push", []template.HTML{"dddd"}, false) // 4 bytes: 14 over cap by 4 -> evicts LRU
	if _, ok := c.get("old"); ok {
		t.Error("old should have been evicted as the least-recently-used entry")
	}
	if _, ok := c.get("mid"); !ok {
		t.Error("mid should have survived")
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
	c.put("h1", []template.HTML{"short"}, false)
	c.put("h1", []template.HTML{"a", "much", "longer", "replacement"}, true)
	got, ok := c.get("h1")
	if !ok || len(got.lines) != 4 || !got.off {
		t.Fatalf("get after overwrite = %+v, ok=%v, want the 4-line replacement with off=true", got, ok)
	}
	wantBytes := len("a") + len("much") + len("longer") + len("replacement")
	if c.bytes != wantBytes {
		t.Errorf("cache bytes = %d, want %d (overwrite must not double-count the old entry)", c.bytes, wantBytes)
	}
}

func TestNewHighlightCacheDefaultCapMatchesDesignBudget(t *testing.T) {
	c := newHighlightCache()
	const want = 16 * 1024 * 1024
	if c.maxBytes != want {
		t.Errorf("newHighlightCache().maxBytes = %d, want %d (16 MiB, P4-design.md §1.4/§6)", c.maxBytes, want)
	}
}

func TestHighlightCacheConcurrentAccessIsRaceFree(t *testing.T) {
	c := newHighlightCache()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		i := i
		wg.Add(2)
		go func() {
			defer wg.Done()
			c.put(hashFor(i), []template.HTML{"line one", "line two"}, false)
		}()
		go func() {
			defer wg.Done()
			c.get(hashFor(i))
		}()
	}
	wg.Wait()
}

func hashFor(i int) string {
	return []string{"file-0", "file-1", "file-2", "file-3", "file-4"}[i%5]
}

// ---- chroma.css: generated once, class-based, no inline styles ----

func TestChromaCSSHasNoInlineStyleAndDefinesTokenClasses(t *testing.T) {
	css := string(chromaCSS())
	if css == "" {
		t.Fatal("chromaCSS() returned empty output")
	}
	if !strings.Contains(css, ".chroma") {
		t.Errorf("expected the generated stylesheet to define .chroma-scoped rules, got:\n%s", css)
	}
	if strings.Contains(css, "<") || strings.Contains(css, "script") {
		t.Errorf("chroma.css must be plain CSS, found suspicious content:\n%s", css)
	}
}

func TestChromaCSSIsMemoizedAcrossCalls(t *testing.T) {
	a, b := chromaCSS(), chromaCSS()
	if string(a) != string(b) {
		t.Error("chromaCSS() should return the identical (memoized) stylesheet on every call")
	}
}
