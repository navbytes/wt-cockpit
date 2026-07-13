package tui

import (
	"fmt"
	"path"
	"strings"

	"github.com/navbytes/wt-cockpit/internal/diffparse"
	"github.com/navbytes/wt-cockpit/internal/model"
)

// rowKind classifies one entry in a flattened diff's render-line list
// (P3-design.md §2.5). Distinct from model.LineKind, which classifies the
// add/ctx/del meaning of a rowCode line specifically.
type rowKind int

const (
	rowFileHeader rowKind = iota
	rowHunkHeader
	rowCode
	rowNote // collapsed-file / binary-file summary line
)

// renderLine is one line flattenDiff produces: a file card header, a hunk
// separator, a code row (with both line-number gutters), or a note (binary/
// collapsed placeholder). Fields beyond Kind/FileIdx are only meaningful for
// the row kinds that use them — see the field comments below.
type renderLine struct {
	kind    rowKind
	fileIdx int

	lineKind model.LineKind // rowCode only: ctx/add/del
	oldNum   int            // rowCode only: 0 means "not applicable" (blank gutter)
	newNum   int            // rowCode only: 0 means "not applicable" (blank gutter)
	content  string         // rowHunkHeader: the "@@ ... @@" text; rowNote: the note text; rowCode: raw source text
	codeIdx  int            // rowCode only: 0-based position within the file's concatenated code-line sequence (indexes highlightedMsg.Lines)
}

// collapseAddDelThreshold and collapseGlobs are the client-side display
// heuristic from P3-design.md §2.5 (not git logic — purely how much of a
// large/generated file the diff pane shows by default).
const collapseAddDelThreshold = 400

var collapseGlobs = []string{"go.sum", "package-lock.json", "*.lock", "yarn.lock", "*.snap", "Cargo.lock"}

// isCollapsedByDefault reports whether f collapses to a one-line summary
// unless the user has expanded it with `o`.
func isCollapsedByDefault(f model.DiffFile) bool {
	if f.Stats.Add+f.Stats.Del > collapseAddDelThreshold {
		return true
	}
	base := path.Base(f.Path)
	for _, g := range collapseGlobs {
		if ok, _ := path.Match(g, base); ok {
			return true
		}
	}
	return false
}

// showsCode reports whether f's hunks are actually rendered as rows (as
// opposed to a single note row) given the current expand overrides, keyed by
// file hash so an override survives an unchanged file across diff refetches.
func showsCode(f model.DiffFile, expanded map[string]bool) bool {
	if f.Binary {
		return false
	}
	return !isCollapsedByDefault(f) || expanded[f.Hash]
}

// totalHunkLines sums every Line across every hunk in f — used for both the
// collapsed-note's line count and (in highlight.go) the chroma size gate.
func totalHunkLines(f model.DiffFile) int {
	n := 0
	for _, h := range f.Hunks {
		n += len(h.Lines)
	}
	return n
}

// flattenDiff turns d into a flat list of renderable rows plus one offset
// per file (the row index of that file's header, for `[`/`]` file-jump and
// the `o` fold toggle). Pure function: no I/O, no styling, no lipgloss —
// diffview.go styles only the rows a frame actually paints. expanded is
// keyed by DiffFile.Hash (the `o` override for collapsed-by-default files).
//
// Hunk header/content text is run through diffparse.SanitizeControl here —
// the one shared choke point every row kind's content flows through before a
// render call ever sees it (DEFECT D2). Production diffs already arrive
// pre-sanitized (wtd's diffparse.Parse call runs the same function before
// the hash is even computed), so this is belt-and-suspenders for anything
// that builds a model.Diff by hand; the import is of diffparse's pure,
// I/O-free text transform only, not git logic, so it doesn't reach past the
// "thin client" boundary this package's own doc comment describes.

func flattenDiff(d model.Diff, expanded map[string]bool) (lines []renderLine, fileOffsets []int) {
	for fi, f := range d.Files {
		fileOffsets = append(fileOffsets, len(lines))
		lines = append(lines, renderLine{kind: rowFileHeader, fileIdx: fi})

		switch {
		case f.Binary:
			lines = append(lines, renderLine{kind: rowNote, fileIdx: fi, content: "binary file"})
		case !showsCode(f, expanded):
			n := totalHunkLines(f)
			lines = append(lines, renderLine{
				kind: rowNote, fileIdx: fi,
				content: fmt.Sprintf("(collapsed: %d lines — o to expand)", n),
			})
		default:
			codeIdx := 0
			for _, h := range f.Hunks {
				lines = append(lines, renderLine{kind: rowHunkHeader, fileIdx: fi, content: diffparse.SanitizeControl(h.Header)})
				for _, ln := range h.Lines {
					lines = append(lines, renderLine{
						kind: rowCode, fileIdx: fi,
						lineKind: ln.Kind, oldNum: ln.OldNum, newNum: ln.NewNum,
						content: diffparse.SanitizeControl(ln.Content), codeIdx: codeIdx,
					})
					codeIdx++
				}
			}
		}
	}
	return lines, fileOffsets
}

// displayPath is the file card's title text: "old → new" for a rename,
// otherwise just the (git-forward-slash) path. Renames aside, DiffFile.Path
// already carries the right side (new path, or old path for a delete).
func displayPath(f model.DiffFile) string {
	if f.Status == model.FileRenamed && f.OldPath != "" && f.OldPath != f.Path {
		return f.OldPath + " → " + f.Path
	}
	return f.Path
}

// splitDirBase splits a git diff path (always "/"-separated, regardless of
// host OS) into a dim directory prefix and the base name, matching the
// mock's "path with dim dir prefix" file card treatment. Byte-splitting on
// '/' is safe for any valid UTF-8 path: '/' never appears as, or inside, a
// multi-byte rune's continuation bytes.
func splitDirBase(p string) (dir, base string) {
	i := strings.LastIndexByte(p, '/')
	if i < 0 {
		return "", p
	}
	return p[:i+1], p[i+1:]
}
