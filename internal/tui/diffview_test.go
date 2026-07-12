package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/navbytes/wt-cockpit/internal/model"
)

// manyLineDiff builds a diff with one file of n added lines, for exercising
// window math without hand-writing giant fixtures.
func manyLineDiff(n int) model.Diff {
	lines := make([]model.Line, n)
	for i := range lines {
		lines[i] = line(model.LineAdd, 0, i+1, "line")
	}
	return model.Diff{WorktreeID: "w1", Files: []model.DiffFile{
		{Path: "big.go", Status: model.FileModified, Hash: "h1", Hunks: []model.Hunk{{Header: "@@h@@", Lines: lines}}},
	}}
}

func newTestPane(d model.Diff) diffview {
	var v diffview
	v.setDiff(d)
	return v
}

// TestDiffviewOffsetNeverNegative pins "offset always clamped".
func TestDiffviewOffsetNeverNegative(t *testing.T) {
	v := newTestPane(manyLineDiff(100))
	v.setHeight(10)
	v.LineUp() // already at 0
	if v.offset != 0 {
		t.Errorf("offset = %d, want 0 (never negative)", v.offset)
	}
	v.scroll(-1000)
	if v.offset != 0 {
		t.Errorf("offset = %d, want 0 after a huge negative scroll", v.offset)
	}
}

// TestDiffviewOffsetNeverExceedsMaxScroll pins "view height honored": you
// can never scroll so far that the viewport shows past the last line.
func TestDiffviewOffsetNeverExceedsMaxScroll(t *testing.T) {
	v := newTestPane(manyLineDiff(100))
	v.setHeight(10)
	v.scroll(100000)
	maxScroll := len(v.lines) - v.height
	if v.offset != maxScroll {
		t.Errorf("offset = %d, want clamped to %d (len(lines)=%d height=%d)", v.offset, maxScroll, len(v.lines), v.height)
	}
}

// TestDiffviewBottomJumpsToExactEnd pins the `G` jump-to-end edge case.
func TestDiffviewBottomJumpsToExactEnd(t *testing.T) {
	v := newTestPane(manyLineDiff(100))
	v.setHeight(10)
	v.Bottom()
	want := len(v.lines) - v.height
	if v.offset != want {
		t.Errorf("offset after Bottom() = %d, want %d", v.offset, want)
	}
	// Top() must return to exactly 0.
	v.Top()
	if v.offset != 0 {
		t.Errorf("offset after Top() = %d, want 0", v.offset)
	}
}

// TestDiffviewShorterThanWindowStaysAtOffsetZero is the "diff shorter than
// window" edge case: a tiny diff in a tall pane never scrolls at all.
func TestDiffviewShorterThanWindowStaysAtOffsetZero(t *testing.T) {
	v := newTestPane(manyLineDiff(2)) // header + hunkheader + 2 code = 4 rows
	v.setHeight(40)
	if v.offset != 0 {
		t.Fatalf("precondition: offset = %d, want 0", v.offset)
	}
	v.PageDown()
	if v.offset != 0 {
		t.Errorf("offset after PageDown() on a short diff = %d, want 0 (nothing to scroll to)", v.offset)
	}
	v.Bottom()
	if v.offset != 0 {
		t.Errorf("offset after Bottom() on a short diff = %d, want 0", v.offset)
	}
}

// TestDiffviewTinyHeightNeverPanics is the "tiny terminal" edge case: a
// height of 0 or 1 must still produce a sane, non-panicking render.
func TestDiffviewTinyHeightNeverPanics(t *testing.T) {
	v := newTestPane(manyLineDiff(20))
	for _, h := range []int{0, 1, -5} {
		v.setHeight(h)
		if v.height < 1 {
			t.Errorf("setHeight(%d): internal height = %d, want clamped to >= 1", h, v.height)
		}
		out := v.render(80, h, nil, nil)
		if out == "" {
			t.Errorf("render at height %d produced empty output", h)
		}
	}
}

// TestDiffviewPageAndHalfPageSizesTrackHeight pins the page/half-page scroll
// deltas against the current viewport height.
func TestDiffviewPageAndHalfPageSizesTrackHeight(t *testing.T) {
	v := newTestPane(manyLineDiff(1000))
	v.setHeight(20)
	v.PageDown()
	if v.offset != 20 {
		t.Errorf("offset after PageDown at height 20 = %d, want 20", v.offset)
	}
	v.Top()
	v.HalfPageDown()
	if v.offset != 10 {
		t.Errorf("offset after HalfPageDown at height 20 = %d, want 10", v.offset)
	}
	v.PageUp()
	if v.offset != 0 {
		t.Errorf("offset after PageUp back past 0 = %d, want clamped to 0", v.offset)
	}
}

// TestDiffviewPrevNextFileJumpsToHeaderOffsets pins `[`/`]` navigation.
func TestDiffviewPrevNextFileJumpsToHeaderOffsets(t *testing.T) {
	d := model.Diff{Files: []model.DiffFile{
		{Path: "a.go", Hash: "ha", Hunks: []model.Hunk{{Header: "@@1@@", Lines: []model.Line{line(model.LineAdd, 0, 1, "x")}}}},
		{Path: "b.go", Hash: "hb", Hunks: []model.Hunk{{Header: "@@2@@", Lines: []model.Line{line(model.LineAdd, 0, 1, "y")}}}},
		{Path: "c.go", Hash: "hc", Hunks: []model.Hunk{{Header: "@@3@@", Lines: []model.Line{line(model.LineAdd, 0, 1, "z")}}}},
	}}
	v := newTestPane(d)
	v.setHeight(2)

	if got := v.currentFileIndex(); got != 0 {
		t.Fatalf("currentFileIndex at start = %d, want 0", got)
	}
	v.NextFile()
	if v.offset != v.fileOffsets[1] {
		t.Errorf("offset after NextFile = %d, want file 1's header offset %d", v.offset, v.fileOffsets[1])
	}
	v.NextFile()
	if v.offset != v.fileOffsets[2] {
		t.Errorf("offset after second NextFile = %d, want file 2's header offset %d", v.offset, v.fileOffsets[2])
	}
	v.NextFile() // already at the last file: no-op
	if v.offset != v.fileOffsets[2] {
		t.Errorf("offset after NextFile past the last file = %d, want unchanged %d", v.offset, v.fileOffsets[2])
	}
	v.PrevFile()
	if v.offset != v.fileOffsets[1] {
		t.Errorf("offset after PrevFile = %d, want file 1's header offset %d", v.offset, v.fileOffsets[1])
	}
}

// TestDiffviewPrevFileFromMidFileGoesToItsOwnHeaderFirst pins that PrevFile,
// when scrolled *into* a file (not sitting on its header), jumps back to
// that file's own header before walking to the previous file.
func TestDiffviewPrevFileFromMidFileGoesToItsOwnHeaderFirst(t *testing.T) {
	v := newTestPane(manyLineDiff(20)) // single file, header+hunkheader+20 code rows
	v.setHeight(3)
	v.offset = 5 // scrolled into the middle of the (only) file
	v.PrevFile()
	if v.offset != v.fileOffsets[0] {
		t.Errorf("offset = %d, want file 0's header offset %d", v.offset, v.fileOffsets[0])
	}
}

// TestDiffviewToggleFoldExpandsAndRecollapses pins `o` flipping a
// collapsed-by-default file both ways and keeping it in view across reflow.
func TestDiffviewToggleFoldExpandsAndRecollapses(t *testing.T) {
	hunkLines := make([]model.Line, 5)
	for i := range hunkLines {
		hunkLines[i] = line(model.LineAdd, 0, i+1, "line")
	}
	d := model.Diff{Files: []model.DiffFile{
		{Path: "go.sum", Status: model.FileModified, Hash: "hlock", Hunks: []model.Hunk{{Header: "@@h@@", Lines: hunkLines}}},
	}}
	v := newTestPane(d)
	v.setHeight(10)

	if len(v.lines) != 2 {
		t.Fatalf("initial rows = %v, want a collapsed 2-row card", dumpRows(v.lines))
	}
	v.ToggleFold()
	if len(v.lines) != 2+len(hunkLines) {
		t.Fatalf("expanded rows = %v, want header+hunkheader+%d code rows", dumpRows(v.lines), len(hunkLines))
	}
	if v.offset != v.fileOffsets[0] {
		t.Errorf("offset after expand = %d, want the (only) file's header offset %d", v.offset, v.fileOffsets[0])
	}
	v.ToggleFold()
	if len(v.lines) != 2 {
		t.Errorf("re-collapsed rows = %v, want back to a 2-row card", dumpRows(v.lines))
	}
}

// TestDiffviewToggleFoldNoOpOnNonCollapsedFile pins the "(collapsed-by-
// default files, §2.5)" scope of `o`: a small, ordinary file has nothing to
// fold.
func TestDiffviewToggleFoldNoOpOnNonCollapsedFile(t *testing.T) {
	v := newTestPane(manyLineDiff(3))
	before := len(v.lines)
	v.ToggleFold()
	if len(v.lines) != before {
		t.Errorf("rows changed after ToggleFold on a non-collapsed file: %d -> %d", before, len(v.lines))
	}
}

// ---- WP3: per-file reviewed state (setReviewed/currentFile, ✓ in the header) ----

func TestRenderFileHeaderShowsCheckmarkWhenReviewed(t *testing.T) {
	v := newTestPane(manyLineDiff(3))
	v.setReviewed("big.go", true)
	out := stripANSI(v.renderFileHeader(0, 80, nil))
	if !strings.Contains(out, "✓") {
		t.Errorf("header = %q, want a ✓ for a reviewed file", out)
	}
}

func TestRenderFileHeaderNoCheckmarkWhenNotReviewed(t *testing.T) {
	v := newTestPane(manyLineDiff(3))
	out := stripANSI(v.renderFileHeader(0, 80, nil))
	if strings.Contains(out, "✓") {
		t.Errorf("header = %q, want no ✓ for an unreviewed file", out)
	}
}

func TestCurrentFileReturnsFileUnderCursor(t *testing.T) {
	d := model.Diff{WorktreeID: "w1", Files: []model.DiffFile{
		{Path: "a.go", Hash: "ha"}, {Path: "b.go", Hash: "hb"},
	}}
	v := newTestPane(d)
	v.setHeight(1) // each file is a bare 1-row header here (no hunks); height must be < total rows or clampOffset undoes the jump
	v.NextFile()   // jump to b.go's header
	f, ok := v.currentFile()
	if !ok || f.Path != "b.go" {
		t.Errorf("currentFile() = %+v, %v, want b.go", f, ok)
	}
}

func TestCurrentFileEmptyDiffReturnsFalse(t *testing.T) {
	var v diffview
	v.setDiff(model.Diff{WorktreeID: "w1"})
	if _, ok := v.currentFile(); ok {
		t.Error("currentFile() ok = true for an empty diff, want false")
	}
}

func TestSetReviewedInitializesNilMapAndIsReadableViaDiffReviewed(t *testing.T) {
	var v diffview
	v.setDiff(model.Diff{WorktreeID: "w1", Files: []model.DiffFile{{Path: "a.go"}}})
	if v.diff.Reviewed != nil {
		t.Fatal("precondition: a freshly loaded diff should have a nil Reviewed map")
	}
	v.setReviewed("a.go", true)
	if !v.diff.Reviewed["a.go"] {
		t.Error("setReviewed(a.go, true) did not stick")
	}
	v.setReviewed("a.go", false)
	if v.diff.Reviewed["a.go"] {
		t.Error("setReviewed(a.go, false) did not revert")
	}
}

// TestDiffviewRenderProducesExactlyHeightLines pins that a render() block is
// always exactly `height` lines tall, whether the diff over- or under-fills
// the viewport — required for lipgloss.JoinHorizontal alongside the sidebar
// to stay aligned.
func TestDiffviewRenderProducesExactlyHeightLines(t *testing.T) {
	for _, n := range []int{0, 2, 100} {
		v := newTestPane(manyLineDiff(n))
		out := v.render(80, 15, nil, nil)
		got := strings.Count(out, "\n") + 1
		if got != 15 {
			t.Errorf("n=%d: render produced %d lines, want exactly 15", n, got)
		}
	}
}

// TestDiffviewNoLinesRendersNoChangesPlaceholder covers an empty diff (a
// selected worktree with zero changed files).
func TestDiffviewNoLinesRendersNoChangesPlaceholder(t *testing.T) {
	var v diffview
	v.setDiff(model.Diff{WorktreeID: "w1"})
	out := v.render(80, 5, nil, nil)
	if !strings.Contains(out, "no changes") {
		t.Errorf("render of an empty diff = %q, want it to mention \"no changes\"", out)
	}
}

// bigRealisticDiff builds a ~5000-line diff across several files with
// varied, chroma-tokenizable Go content (not just a repeated placeholder),
// for the render-step benchmark below.
func bigRealisticDiff(totalLines int) model.Diff {
	const filesCount = 20
	perFile := totalLines / filesCount
	var files []model.DiffFile
	for f := 0; f < filesCount; f++ {
		lines := make([]model.Line, perFile)
		for i := range lines {
			kind := model.LineContext
			switch i % 5 {
			case 0:
				kind = model.LineAdd
			case 1:
				kind = model.LineDel
			}
			content := fmt.Sprintf("\tresult := compute(%d, \"item-%d\") // step %d", i, i, i)
			lines[i] = model.Line{Kind: kind, OldNum: i + 1, NewNum: i + 1, Content: content}
		}
		files = append(files, model.DiffFile{
			Path: fmt.Sprintf("internal/pkg%d/file%d.go", f, f), Status: model.FileModified,
			Hash:  fmt.Sprintf("hash-%d", f),
			Stats: model.Stats{Add: perFile / 5, Del: perFile / 5},
			Hunks: []model.Hunk{{Header: "@@ big hunk @@", Lines: lines}},
		})
	}
	return model.Diff{WorktreeID: "bench", Files: files}
}

// BenchmarkDiffviewRenderFrame is P3-design.md §6's scroll-latency gate: a
// 5k-line diff, ~60 visible rows, highlight cache pre-warmed (the steady-
// state scrolling cost — a cache miss only ever renders the cheaper plain
// fallback). Budget: < 16ms/op; run with `go test -bench
// BenchmarkDiffviewRenderFrame -benchtime=200x ./internal/tui`.
func BenchmarkDiffviewRenderFrame(b *testing.B) {
	d := bigRealisticDiff(5000)
	v := newTestPane(d)
	v.setHeight(60)

	hl := newHighlightCache()
	hl.maxBytes = 64 * 1024 * 1024 // enough to hold every file in this benchmark without evicting
	for _, f := range d.Files {
		content := concatFileContent(f)
		lines := strings.Split(content, "\n")
		hl.put(f.Hash, lines)
	}
	danger := map[string]bool{}

	maxOffset := len(v.lines) - v.height
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		v.offset = i % (maxOffset + 1)
		_ = v.render(120, 60, hl, danger)
	}
}
