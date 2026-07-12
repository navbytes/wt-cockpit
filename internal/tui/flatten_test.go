package tui

import (
	"fmt"
	"testing"

	"github.com/navbytes/wt-cockpit/internal/model"
)

// dumpRows renders a flattenDiff result into one deterministic line per row,
// so a test failure prints something reviewable instead of a raw struct dump.
func dumpRows(lines []renderLine) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = fmt.Sprintf("%d|fi=%d|lk=%s|old=%d|new=%d|ci=%d|%q",
			l.kind, l.fileIdx, l.lineKind, l.oldNum, l.newNum, l.codeIdx, l.content)
	}
	return out
}

func assertRows(t *testing.T, got []renderLine, want []renderLine) {
	t.Helper()
	gotDump, wantDump := dumpRows(got), dumpRows(want)
	if len(gotDump) != len(wantDump) {
		t.Fatalf("row count = %d, want %d\ngot:  %v\nwant: %v", len(gotDump), len(wantDump), gotDump, wantDump)
	}
	for i := range wantDump {
		if gotDump[i] != wantDump[i] {
			t.Errorf("row %d = %s, want %s", i, gotDump[i], wantDump[i])
		}
	}
}

func line(kind model.LineKind, old, new int, content string) model.Line {
	return model.Line{Kind: kind, OldNum: old, NewNum: new, Content: content}
}

// TestFlattenDiffSimpleModifyProducesHeaderHunkAndCodeRowsInOrder pins the
// basic shape: one file header, one hunk header, then code rows carrying
// both gutters and a continuously-incrementing codeIdx.
func TestFlattenDiffSimpleModifyProducesHeaderHunkAndCodeRowsInOrder(t *testing.T) {
	d := model.Diff{Files: []model.DiffFile{
		{
			Path: "internal/auth/token.go", Status: model.FileModified,
			Stats: model.Stats{Add: 1, Del: 1}, Hash: "h1",
			Hunks: []model.Hunk{{
				Header: "@@ -18,3 +18,4 @@ func NewToken(",
				Lines: []model.Line{
					line(model.LineContext, 18, 18, "func NewToken() {"),
					line(model.LineDel, 19, 0, "  old line"),
					line(model.LineAdd, 0, 19, "  new line"),
					line(model.LineContext, 20, 20, "}"),
				},
			}},
		},
	}}

	rows, offsets := flattenDiff(d, nil)
	want := []renderLine{
		{kind: rowFileHeader, fileIdx: 0},
		{kind: rowHunkHeader, fileIdx: 0, content: "@@ -18,3 +18,4 @@ func NewToken("},
		{kind: rowCode, fileIdx: 0, lineKind: model.LineContext, oldNum: 18, newNum: 18, content: "func NewToken() {", codeIdx: 0},
		{kind: rowCode, fileIdx: 0, lineKind: model.LineDel, oldNum: 19, content: "  old line", codeIdx: 1},
		{kind: rowCode, fileIdx: 0, lineKind: model.LineAdd, newNum: 19, content: "  new line", codeIdx: 2},
		{kind: rowCode, fileIdx: 0, lineKind: model.LineContext, oldNum: 20, newNum: 20, content: "}", codeIdx: 3},
	}
	assertRows(t, rows, want)
	if len(offsets) != 1 || offsets[0] != 0 {
		t.Errorf("fileOffsets = %v, want [0]", offsets)
	}
}

// TestFlattenDiffMultiHunkCodeIdxIsContinuousAcrossHunks pins that codeIdx
// (which indexes into a file's single concatenated chroma result) keeps
// counting across a hunk boundary rather than resetting per hunk.
func TestFlattenDiffMultiHunkCodeIdxIsContinuousAcrossHunks(t *testing.T) {
	d := model.Diff{Files: []model.DiffFile{
		{
			Path: "a.go", Status: model.FileModified, Hash: "h1",
			Hunks: []model.Hunk{
				{Header: "@@ -1,2 +1,2 @@", Lines: []model.Line{
					line(model.LineContext, 1, 1, "package a"),
					line(model.LineContext, 2, 2, ""),
				}},
				{Header: "@@ -10,1 +10,2 @@", Lines: []model.Line{
					line(model.LineAdd, 0, 11, "// added"),
				}},
			},
		},
	}}
	rows, _ := flattenDiff(d, nil)
	var codeIdxs []int
	for _, r := range rows {
		if r.kind == rowCode {
			codeIdxs = append(codeIdxs, r.codeIdx)
		}
	}
	want := []int{0, 1, 2}
	if len(codeIdxs) != len(want) {
		t.Fatalf("codeIdxs = %v, want %v", codeIdxs, want)
	}
	for i := range want {
		if codeIdxs[i] != want[i] {
			t.Errorf("codeIdxs[%d] = %d, want %d (full: %v)", i, codeIdxs[i], want[i], codeIdxs)
		}
	}
}

// TestFlattenDiffRenameShowsOldArrowNewAndNoCollapseByStatusAlone pins the
// rename fixture: displayPath renders "old → new", and a rename with a small
// diff isn't collapsed just because it's a rename.
func TestFlattenDiffRenameShowsOldArrowNewAndNoCollapseByStatusAlone(t *testing.T) {
	f := model.DiffFile{
		Path: "new/name.go", OldPath: "old/name.go", Status: model.FileRenamed,
		Stats: model.Stats{Add: 1, Del: 1}, Hash: "h1",
		Hunks: []model.Hunk{{Header: "@@ -1,1 +1,1 @@", Lines: []model.Line{
			line(model.LineDel, 1, 0, "old"),
			line(model.LineAdd, 0, 1, "new"),
		}}},
	}
	if got := displayPath(f); got != "old/name.go → new/name.go" {
		t.Errorf("displayPath = %q, want %q", got, "old/name.go → new/name.go")
	}

	rows, _ := flattenDiff(model.Diff{Files: []model.DiffFile{f}}, nil)
	// header + hunk header + 2 code rows: not collapsed to a note.
	if len(rows) != 4 {
		t.Fatalf("rows = %v, want 4 rows (not collapsed)", dumpRows(rows))
	}
	if rows[1].kind != rowHunkHeader {
		t.Errorf("row 1 kind = %v, want rowHunkHeader (rename alone must not collapse)", rows[1].kind)
	}
}

// TestFlattenDiffPureRenameWithNoHunksIsJustAHeaderRow covers a 100%-
// similarity rename: no content changed, so there are zero hunks — the file
// card is just its header, nothing else.
func TestFlattenDiffPureRenameWithNoHunksIsJustAHeaderRow(t *testing.T) {
	d := model.Diff{Files: []model.DiffFile{
		{Path: "b.go", OldPath: "a.go", Status: model.FileRenamed, Hash: "h1"},
	}}
	rows, offsets := flattenDiff(d, nil)
	assertRows(t, rows, []renderLine{{kind: rowFileHeader, fileIdx: 0}})
	if len(offsets) != 1 || offsets[0] != 0 {
		t.Errorf("fileOffsets = %v, want [0]", offsets)
	}
}

// TestFlattenDiffBinaryFileShowsHeaderThenBinaryNoteOnly pins binary
// handling: never emits hunks/code rows even if (unusually) present.
func TestFlattenDiffBinaryFileShowsHeaderThenBinaryNoteOnly(t *testing.T) {
	d := model.Diff{Files: []model.DiffFile{
		{Path: "assets/logo.png", Status: model.FileModified, Binary: true, Hash: "h1"},
	}}
	rows, _ := flattenDiff(d, nil)
	assertRows(t, rows, []renderLine{
		{kind: rowFileHeader, fileIdx: 0},
		{kind: rowNote, fileIdx: 0, content: "binary file"},
	})
}

// TestFlattenDiffCollapsesLargeFileBySizeThreshold pins the Add+Del > 400
// collapse rule, and that `o`-expanding it (via the expanded map) restores
// full hunk rendering.
func TestFlattenDiffCollapsesLargeFileBySizeThreshold(t *testing.T) {
	hunkLines := make([]model.Line, 3)
	for i := range hunkLines {
		hunkLines[i] = line(model.LineAdd, 0, i+1, "line")
	}
	f := model.DiffFile{
		Path: "big.go", Status: model.FileModified, Hash: "hbig",
		Stats: model.Stats{Add: 300, Del: 200}, // 500 > 400
		Hunks: []model.Hunk{{Header: "@@ -0,0 +1,3 @@", Lines: hunkLines}},
	}
	d := model.Diff{Files: []model.DiffFile{f}}

	rows, _ := flattenDiff(d, nil)
	assertRows(t, rows, []renderLine{
		{kind: rowFileHeader, fileIdx: 0},
		{kind: rowNote, fileIdx: 0, content: "(collapsed: 3 lines — o to expand)"},
	})

	expanded := map[string]bool{"hbig": true}
	rows, _ = flattenDiff(d, expanded)
	if len(rows) != 1+1+len(hunkLines) {
		t.Fatalf("expanded rows = %v, want header+hunkheader+%d code rows", dumpRows(rows), len(hunkLines))
	}
	if rows[1].kind != rowHunkHeader {
		t.Errorf("row 1 kind = %v, want rowHunkHeader once expanded", rows[1].kind)
	}
}

// TestFlattenDiffCollapsesLockfileByGlobEvenWhenSmall pins the lockfile/
// snapshot glob collapse rule independent of size.
func TestFlattenDiffCollapsesLockfileByGlobEvenWhenSmall(t *testing.T) {
	cases := []string{"go.sum", "package-lock.json", "yarn.lock", "Cargo.lock", "vendor/foo.lock", "testdata/x.snap"}
	for _, path := range cases {
		f := model.DiffFile{
			Path: path, Status: model.FileModified, Hash: "h-" + path,
			Stats: model.Stats{Add: 1, Del: 1},
			Hunks: []model.Hunk{{Header: "@@ -1,1 +1,1 @@", Lines: []model.Line{
				line(model.LineAdd, 0, 1, "x"),
			}}},
		}
		rows, _ := flattenDiff(model.Diff{Files: []model.DiffFile{f}}, nil)
		if len(rows) != 2 || rows[1].kind != rowNote {
			t.Errorf("path %q: rows = %v, want a collapsed 2-row card", path, dumpRows(rows))
		}
	}
}

// TestFlattenDiffUnicodePathAndContentSurviveByteForByte pins that
// multi-byte UTF-8 in both the path and the code content flows through
// unchanged (path splitting is a byte-index '/' split, safe for any valid
// UTF-8 since '/' can't appear inside a multi-byte rune).
func TestFlattenDiffUnicodePathAndContentSurviveByteForByte(t *testing.T) {
	f := model.DiffFile{
		Path: "café/日本語.go", Status: model.FileModified, Hash: "hu",
		Stats: model.Stats{Add: 1},
		Hunks: []model.Hunk{{Header: "@@ -1,0 +1,1 @@", Lines: []model.Line{
			line(model.LineAdd, 0, 1, `fmt.Println("héllo 世界")`),
		}}},
	}
	rows, _ := flattenDiff(model.Diff{Files: []model.DiffFile{f}}, nil)
	assertRows(t, rows, []renderLine{
		{kind: rowFileHeader, fileIdx: 0},
		{kind: rowHunkHeader, fileIdx: 0, content: "@@ -1,0 +1,1 @@"},
		{kind: rowCode, fileIdx: 0, lineKind: model.LineAdd, newNum: 1, content: `fmt.Println("héllo 世界")`, codeIdx: 0},
	})

	dir, base := splitDirBase(displayPath(f))
	if dir != "café/" || base != "日本語.go" {
		t.Errorf("splitDirBase(%q) = (%q, %q), want (\"café/\", \"日本語.go\")", f.Path, dir, base)
	}
}

// TestFlattenDiffMultipleFilesEachGetTheirOwnOffset pins fileOffsets: one
// entry per file, pointing at that file's header row index.
func TestFlattenDiffMultipleFilesEachGetTheirOwnOffset(t *testing.T) {
	d := model.Diff{Files: []model.DiffFile{
		{Path: "a.go", Hash: "ha", Hunks: []model.Hunk{{Header: "@@h1@@", Lines: []model.Line{line(model.LineAdd, 0, 1, "x")}}}},
		{Path: "b.go", Binary: true, Hash: "hb"},
		{Path: "c.go", Hash: "hc", Hunks: []model.Hunk{{Header: "@@h2@@", Lines: []model.Line{line(model.LineAdd, 0, 1, "y")}}}},
	}}
	rows, offsets := flattenDiff(d, nil)
	want := []int{0, 3, 5} // a: header,hunk,code (0,1,2); b: header,note (3,4); c: header,hunk,code (5,6,7)
	if len(offsets) != len(want) {
		t.Fatalf("fileOffsets = %v, want %v (rows: %v)", offsets, want, dumpRows(rows))
	}
	for i := range want {
		if offsets[i] != want[i] {
			t.Errorf("fileOffsets[%d] = %d, want %d", i, offsets[i], want[i])
		}
	}
}
