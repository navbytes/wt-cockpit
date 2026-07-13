// sxs.go is the reading room's side-by-side transform (P4-design.md §1.4): a
// pure function pairing one hunk's lines into aligned left/right rows for the
// mock's "before / after" grid. It deliberately corrects a bug in the mock's
// own JS, which pairs add/del lines by their position across the *whole*
// hunk — so a context line that follows an earlier unbalanced add/del block
// comes out misaligned, because the mock's left/right arrays have already
// drifted apart by then. This transform resets pairing at every block
// boundary (a context line, or the end of the hunk) instead, which is what
// keeps every later context line aligned regardless of any earlier
// imbalance (the same per-block pairing delta/GitHub use).
package web

import (
	"html/template"

	"github.com/navbytes/wt-cockpit/internal/model"
)

// sxsEmpty marks a padding filler cell on the shorter side of an unbalanced
// add/del block — rendered as the mock's striped ".srow.empty" filler. It is
// a value of model.LineKind that model's own const block never defines
// (model stays free of a web-only display detail), so SxsCell.Kind can hold
// either a real line kind or this sentinel.
const sxsEmpty model.LineKind = "empty"

// SxsCell is one half of an SxsRow: either a real diff line (ctx/add/del,
// HTML already rendered) or an empty filler (sxsEmpty, zero Num, empty HTML).
type SxsCell struct {
	Kind model.LineKind
	Num  int
	HTML template.HTML
}

// SxsRow is one aligned pair of cells in the side-by-side grid: Left is the
// "before" column, Right is the "after" column.
type SxsRow struct {
	Left, Right SxsCell
}

// SplitHunk pairs h's lines into side-by-side rows. lineHTML must carry one
// entry per entry of h.Lines, in the same order — the file-level highlight
// pass's output (highlight.go), sliced to this hunk by the caller. SplitHunk
// never constructs template.HTML itself (P4-design.md's two-producer rule:
// both producers live in highlight.go) — it only places already-rendered
// cells left or right, so it stays a pure, deterministic function of its
// inputs, safe to golden-test without touching chroma at all.
//
// Algorithm: a context line first flushes any pending add/del block (see
// below), then becomes its own aligned Left/Right pair (both sides share the
// same line and HTML, since a context line is identical old and new). A
// maximal run of consecutive add/del lines buffers into a pending block —
// every del in encounter order on the left, every add in encounter order on
// the right — flushed by pairing index-wise (del[i] <-> add[i]) once a
// context line or the end of the hunk is reached, padding whichever side is
// shorter with an empty cell. Resetting at every block boundary (rather than
// carrying one pair of arrays across the whole hunk) is exactly what keeps a
// later context line aligned even after an earlier unbalanced block.
func SplitHunk(h model.Hunk, lineHTML []template.HTML) []SxsRow {
	var rows []SxsRow
	var dels, adds []SxsCell

	flush := func() {
		n := len(dels)
		if len(adds) > n {
			n = len(adds)
		}
		for i := 0; i < n; i++ {
			row := SxsRow{Left: SxsCell{Kind: sxsEmpty}, Right: SxsCell{Kind: sxsEmpty}}
			if i < len(dels) {
				row.Left = dels[i]
			}
			if i < len(adds) {
				row.Right = adds[i]
			}
			rows = append(rows, row)
		}
		dels, adds = nil, nil
	}

	for i, ln := range h.Lines {
		var html template.HTML
		if i < len(lineHTML) {
			html = lineHTML[i]
		}
		switch ln.Kind {
		case model.LineDel:
			dels = append(dels, SxsCell{Kind: model.LineDel, Num: ln.OldNum, HTML: html})
		case model.LineAdd:
			adds = append(adds, SxsCell{Kind: model.LineAdd, Num: ln.NewNum, HTML: html})
		default: // model.LineContext
			flush()
			rows = append(rows, SxsRow{
				Left:  SxsCell{Kind: model.LineContext, Num: ln.OldNum, HTML: html},
				Right: SxsCell{Kind: model.LineContext, Num: ln.NewNum, HTML: html},
			})
		}
	}
	flush()
	return rows
}
