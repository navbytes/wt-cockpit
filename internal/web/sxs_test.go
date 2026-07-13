package web

import (
	"fmt"
	"html/template"
	"testing"

	"github.com/navbytes/wt-cockpit/internal/model"
)

// sline builds a model.Line the way diffparse would, tagging each line's
// Content with its own index so assertions can trace exactly which input
// line ended up in which output cell.
func sline(kind model.LineKind, old, new int) model.Line {
	return model.Line{Kind: kind, OldNum: old, NewNum: new, Content: fmt.Sprintf("old=%d,new=%d", old, new)}
}

// htmlFor builds one distinguishable template.HTML value per line index, so
// tests can confirm SplitHunk passes HTML through unmodified rather than
// constructing or altering it itself (sxs.go is not one of the two allowed
// template.HTML producers).
func htmlFor(lines []model.Line) []template.HTML {
	out := make([]template.HTML, len(lines))
	for i := range lines {
		out[i] = template.HTML(fmt.Sprintf("<mark-%d>", i))
	}
	return out
}

func dumpRows(rows []SxsRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = fmt.Sprintf("%d: L(%s,%d,%q) R(%s,%d,%q)", i, r.Left.Kind, r.Left.Num, r.Left.HTML, r.Right.Kind, r.Right.Num, r.Right.HTML)
	}
	return out
}

func assertRow(t *testing.T, got SxsRow, want SxsRow, idx int) {
	t.Helper()
	if got != want {
		t.Errorf("row %d = %+v, want %+v", idx, got, want)
	}
}

// ---- pure shapes ----

func TestSplitHunkPureContextPairsEveryLineBothSides(t *testing.T) {
	lines := []model.Line{sline(model.LineContext, 1, 1), sline(model.LineContext, 2, 2)}
	h := model.Hunk{Lines: lines}
	rows := SplitHunk(h, htmlFor(lines))

	if len(rows) != 2 {
		t.Fatalf("rows = %v, want 2", dumpRows(rows))
	}
	assertRow(t, rows[0], SxsRow{Left: SxsCell{Kind: model.LineContext, Num: 1, HTML: "<mark-0>"}, Right: SxsCell{Kind: model.LineContext, Num: 1, HTML: "<mark-0>"}}, 0)
	assertRow(t, rows[1], SxsRow{Left: SxsCell{Kind: model.LineContext, Num: 2, HTML: "<mark-1>"}, Right: SxsCell{Kind: model.LineContext, Num: 2, HTML: "<mark-1>"}}, 1)
}

func TestSplitHunkPureAddsFillLeftWithEmpty(t *testing.T) {
	lines := []model.Line{sline(model.LineAdd, 0, 1), sline(model.LineAdd, 0, 2), sline(model.LineAdd, 0, 3)}
	h := model.Hunk{Lines: lines}
	rows := SplitHunk(h, htmlFor(lines))

	if len(rows) != 3 {
		t.Fatalf("rows = %v, want 3 (one per pure add, no dels to pair against)", dumpRows(rows))
	}
	for i, r := range rows {
		if r.Left.Kind != sxsEmpty || r.Left.Num != 0 || r.Left.HTML != "" {
			t.Errorf("row %d Left = %+v, want an empty filler", i, r.Left)
		}
		want := SxsCell{Kind: model.LineAdd, Num: i + 1, HTML: template.HTML(fmt.Sprintf("<mark-%d>", i))}
		if r.Right != want {
			t.Errorf("row %d Right = %+v, want %+v", i, r.Right, want)
		}
	}
}

func TestSplitHunkPureDelsFillRightWithEmpty(t *testing.T) {
	lines := []model.Line{sline(model.LineDel, 1, 0), sline(model.LineDel, 2, 0)}
	h := model.Hunk{Lines: lines}
	rows := SplitHunk(h, htmlFor(lines))

	if len(rows) != 2 {
		t.Fatalf("rows = %v, want 2", dumpRows(rows))
	}
	for i, r := range rows {
		if r.Right.Kind != sxsEmpty || r.Right.Num != 0 || r.Right.HTML != "" {
			t.Errorf("row %d Right = %+v, want an empty filler", i, r.Right)
		}
		want := SxsCell{Kind: model.LineDel, Num: i + 1, HTML: template.HTML(fmt.Sprintf("<mark-%d>", i))}
		if r.Left != want {
			t.Errorf("row %d Left = %+v, want %+v", i, r.Left, want)
		}
	}
}

func TestSplitHunkBalancedBlockPairsIndexWiseNoFiller(t *testing.T) {
	lines := []model.Line{sline(model.LineDel, 1, 0), sline(model.LineDel, 2, 0), sline(model.LineAdd, 0, 1), sline(model.LineAdd, 0, 2)}
	h := model.Hunk{Lines: lines}
	rows := SplitHunk(h, htmlFor(lines))

	if len(rows) != 2 {
		t.Fatalf("rows = %v, want 2 (equal-sized block, no filler needed)", dumpRows(rows))
	}
	for _, r := range rows {
		if r.Left.Kind == sxsEmpty || r.Right.Kind == sxsEmpty {
			t.Errorf("row %+v: a balanced block should never need a filler cell", r)
		}
	}
}

// ---- the corrected pairing: unbalanced blocks ----

func TestSplitHunkUnbalancedBlockMoreAddsThanDels(t *testing.T) {
	// 2 dels, 4 adds: del[i] <-> add[i] for i<2, then 2 add-only rows.
	lines := []model.Line{
		sline(model.LineDel, 10, 0), sline(model.LineDel, 11, 0),
		sline(model.LineAdd, 0, 10), sline(model.LineAdd, 0, 11), sline(model.LineAdd, 0, 12), sline(model.LineAdd, 0, 13),
	}
	h := model.Hunk{Lines: lines}
	rows := SplitHunk(h, htmlFor(lines))

	if len(rows) != 4 {
		t.Fatalf("rows = %v, want 4 (max(2 dels, 4 adds))", dumpRows(rows))
	}
	for i := 0; i < 2; i++ {
		if rows[i].Left.Kind != model.LineDel || rows[i].Right.Kind != model.LineAdd {
			t.Errorf("row %d = %+v, want a real del/add pair", i, rows[i])
		}
	}
	for i := 2; i < 4; i++ {
		if rows[i].Left.Kind != sxsEmpty {
			t.Errorf("row %d Left = %+v, want empty filler (only %d dels)", i, rows[i].Left, 2)
		}
		if rows[i].Right.Kind != model.LineAdd || rows[i].Right.Num != i+10 {
			t.Errorf("row %d Right = %+v, want the leftover add (NewNum %d)", i, rows[i].Right, i+10)
		}
	}
}

func TestSplitHunkUnbalancedBlockMoreDelsThanAdds(t *testing.T) {
	lines := []model.Line{
		sline(model.LineDel, 1, 0), sline(model.LineDel, 2, 0), sline(model.LineDel, 3, 0), sline(model.LineDel, 4, 0),
		sline(model.LineAdd, 0, 1),
	}
	h := model.Hunk{Lines: lines}
	rows := SplitHunk(h, htmlFor(lines))

	if len(rows) != 4 {
		t.Fatalf("rows = %v, want 4 (max(4 dels, 1 add))", dumpRows(rows))
	}
	if rows[0].Right.Kind != model.LineAdd {
		t.Errorf("row 0 Right = %+v, want the lone add paired with the first del", rows[0].Right)
	}
	for i := 1; i < 4; i++ {
		if rows[i].Right.Kind != sxsEmpty {
			t.Errorf("row %d Right = %+v, want empty filler (only 1 add)", i, rows[i].Right)
		}
		if rows[i].Left.Kind != model.LineDel {
			t.Errorf("row %d Left = %+v, want the leftover del", i, rows[i].Left)
		}
	}
}

// TestSplitHunkMultipleBlocksResetAlignmentPerBlock is the regression the
// design calls out explicitly: the mock's own JS pairs add/del lines by
// position across the *whole* hunk, so a context line following an earlier
// unbalanced block comes out misaligned (its row index has already drifted).
// This pins that SplitHunk resets pairing at every block boundary instead,
// so every context line always emits as a clean, correctly aligned pair
// regardless of what happened earlier in the hunk.
func TestSplitHunkMultipleBlocksResetAlignmentPerBlock(t *testing.T) {
	lines := []model.Line{
		sline(model.LineContext, 1, 1), // ctx A
		sline(model.LineDel, 2, 0),     // block 1: 1 del, 3 adds (unbalanced)
		sline(model.LineAdd, 0, 2),
		sline(model.LineAdd, 0, 3),
		sline(model.LineAdd, 0, 4),
		sline(model.LineContext, 3, 5), // ctx B -- must still land as a clean pair
		sline(model.LineDel, 4, 0),     // block 2: 3 dels, 1 add (unbalanced the other way)
		sline(model.LineDel, 5, 0),
		sline(model.LineDel, 6, 0),
		sline(model.LineAdd, 0, 6),
		sline(model.LineContext, 7, 7), // ctx C
	}
	h := model.Hunk{Lines: lines}
	rows := SplitHunk(h, htmlFor(lines))

	// ctx A (1) + block1 (max(1,3)=3) + ctx B (1) + block2 (max(3,1)=3) + ctx C (1) = 9
	const want = 9
	if len(rows) != want {
		t.Fatalf("rows = %v (%d), want %d", dumpRows(rows), len(rows), want)
	}

	ctxA, ctxB, ctxC := rows[0], rows[4], rows[8]
	for name, r := range map[string]SxsRow{"A": ctxA, "B": ctxB, "C": ctxC} {
		if r.Left.Kind != model.LineContext || r.Right.Kind != model.LineContext {
			t.Errorf("ctx %s = %+v, want a real, aligned context pair (the fix: unaffected by neighboring block imbalance)", name, r)
		}
	}
	if ctxB.Left.Num != 3 || ctxB.Right.Num != 5 {
		t.Errorf("ctx B = %+v, want Left.Num=3 Right.Num=5 straight from its own line", ctxB)
	}
	if ctxC.Left.Num != 7 || ctxC.Right.Num != 7 {
		t.Errorf("ctx C = %+v, want Left.Num=7 Right.Num=7", ctxC)
	}

	// Block 1 (rows 1-3): del pairs with the first add, the other two adds get filler.
	if rows[1].Left.Kind != model.LineDel || rows[1].Right.Kind != model.LineAdd {
		t.Errorf("block1 row0 = %+v, want del/add pair", rows[1])
	}
	if rows[2].Left.Kind != sxsEmpty || rows[3].Left.Kind != sxsEmpty {
		t.Errorf("block1 rows 1-2 Left = %+v / %+v, want empty filler", rows[2].Left, rows[3].Left)
	}

	// Block 2 (rows 5-7): add pairs with the first del, the other two dels get filler.
	if rows[5].Left.Kind != model.LineDel || rows[5].Right.Kind != model.LineAdd {
		t.Errorf("block2 row0 = %+v, want del/add pair", rows[5])
	}
	if rows[6].Right.Kind != sxsEmpty || rows[7].Right.Kind != sxsEmpty {
		t.Errorf("block2 rows 1-2 Right = %+v / %+v, want empty filler", rows[6].Right, rows[7].Right)
	}
}

// ---- edge cases ----

func TestSplitHunkEmptyHunkProducesNoRows(t *testing.T) {
	if rows := SplitHunk(model.Hunk{}, nil); len(rows) != 0 {
		t.Errorf("rows = %v, want none for a hunk with no lines", dumpRows(rows))
	}
}

func TestSplitHunkTrailingBlockWithNoFollowingContextStillFlushes(t *testing.T) {
	// A hunk can legitimately end mid-block (e.g. the last lines of a file).
	lines := []model.Line{sline(model.LineDel, 1, 0), sline(model.LineAdd, 0, 1), sline(model.LineAdd, 0, 2)}
	h := model.Hunk{Lines: lines}
	rows := SplitHunk(h, htmlFor(lines))
	if len(rows) != 2 {
		t.Fatalf("rows = %v, want 2 (the final block must flush even with nothing after it)", dumpRows(rows))
	}
}

// TestSplitHunkShorterLineHTMLIsSafe pins defensive behavior for a caller
// bug (an lineHTML slice shorter than h.Lines): no panic, missing entries
// just render with empty HTML rather than indexing out of range.
func TestSplitHunkShorterLineHTMLIsSafe(t *testing.T) {
	lines := []model.Line{sline(model.LineContext, 1, 1), sline(model.LineAdd, 0, 2)}
	h := model.Hunk{Lines: lines}
	rows := SplitHunk(h, []template.HTML{"<only-one>"})
	if len(rows) != 2 {
		t.Fatalf("rows = %v, want 2", dumpRows(rows))
	}
	if rows[1].Right.HTML != "" {
		t.Errorf("row 1 Right.HTML = %q, want empty (lineHTML ran out)", rows[1].Right.HTML)
	}
}

// ---- property test ----

// TestSplitHunkPropertyEveryLineAccountedForExactlyOnce walks a hunk mixing
// context, unbalanced add/del blocks, and repeats, then checks the
// structural invariant every SplitHunk caller (and reviewer) can rely on:
// every context line becomes exactly one aligned pair, and every add/del
// line's own (kind, line-number) identity shows up in exactly one non-empty
// cell across the whole output — never duplicated, never dropped, and row
// count per block is exactly max(dels, adds) in that block.
func TestSplitHunkPropertyEveryLineAccountedForExactlyOnce(t *testing.T) {
	lines := []model.Line{
		sline(model.LineContext, 1, 1),
		sline(model.LineDel, 2, 0), sline(model.LineDel, 3, 0),
		sline(model.LineAdd, 0, 2), sline(model.LineAdd, 0, 3), sline(model.LineAdd, 0, 4), sline(model.LineAdd, 0, 5),
		sline(model.LineContext, 4, 6),
		sline(model.LineAdd, 0, 7),
		sline(model.LineContext, 5, 8),
		sline(model.LineDel, 6, 0), sline(model.LineDel, 7, 0), sline(model.LineDel, 8, 0),
		sline(model.LineContext, 9, 9),
	}
	h := model.Hunk{Lines: lines}
	rows := SplitHunk(h, htmlFor(lines))

	wantCtx := 0
	wantDelNums, wantAddNums := map[int]bool{}, map[int]bool{}
	for _, ln := range lines {
		switch ln.Kind {
		case model.LineContext:
			wantCtx++
		case model.LineDel:
			wantDelNums[ln.OldNum] = true
		case model.LineAdd:
			wantAddNums[ln.NewNum] = true
		}
	}

	gotCtx := 0
	gotDelNums, gotAddNums := map[int]int{}, map[int]int{} // value counts occurrences, to catch duplicates
	for _, r := range rows {
		if r.Left.Kind == model.LineContext && r.Right.Kind == model.LineContext {
			gotCtx++
		}
		if r.Left.Kind == model.LineDel {
			gotDelNums[r.Left.Num]++
		}
		if r.Right.Kind == model.LineAdd {
			gotAddNums[r.Right.Num]++
		}
	}

	if gotCtx != wantCtx {
		t.Errorf("context rows = %d, want %d", gotCtx, wantCtx)
	}
	for n := range wantDelNums {
		if gotDelNums[n] != 1 {
			t.Errorf("del line old=%d appeared %d times in output, want exactly 1", n, gotDelNums[n])
		}
	}
	for n := range wantAddNums {
		if gotAddNums[n] != 1 {
			t.Errorf("add line new=%d appeared %d times in output, want exactly 1", n, gotAddNums[n])
		}
	}
	if len(gotDelNums) != len(wantDelNums) || len(gotAddNums) != len(wantAddNums) {
		t.Errorf("got %d distinct dels / %d distinct adds, want %d / %d (no extras)", len(gotDelNums), len(gotAddNums), len(wantDelNums), len(wantAddNums))
	}
}

// TestSplitHunkNeverConstructsHTMLItself pins the two-producer rule from the
// caller's side: feed lineHTML full of recognizable markers (a del, a ctx,
// then an add — kept apart by the ctx line so each lands in its own row
// rather than pairing into a shared one) and confirm every marker shows up
// byte-for-byte in its cell, proving SplitHunk only places values, never
// (re)builds or escapes them.
func TestSplitHunkNeverConstructsHTMLItself(t *testing.T) {
	lines := []model.Line{sline(model.LineDel, 1, 0), sline(model.LineContext, 2, 2), sline(model.LineAdd, 0, 3)}
	marks := []template.HTML{"<del-mark>", "<ctx-mark>", "<add-mark>"}
	rows := SplitHunk(model.Hunk{Lines: lines}, marks)

	if len(rows) != 3 {
		t.Fatalf("rows = %v, want 3 (del and add are separated by ctx, so each is its own block)", dumpRows(rows))
	}
	if rows[0].Left.HTML != "<del-mark>" {
		t.Errorf("del row Left.HTML = %q, want the input marker untouched", rows[0].Left.HTML)
	}
	if rows[1].Left.HTML != "<ctx-mark>" || rows[1].Right.HTML != "<ctx-mark>" {
		t.Errorf("ctx row = %+v, want both sides carrying the same input marker untouched", rows[1])
	}
	if rows[2].Right.HTML != "<add-mark>" {
		t.Errorf("add row Right.HTML = %q, want the input marker untouched", rows[2].Right.HTML)
	}
}
