package tui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/navbytes/wt-cockpit/internal/model"
)

// chromaReset is the literal ANSI reset chroma's TTY formatters (and
// lipgloss's own Style.Render) emit — confirmed against both at the exact
// pinned versions. tintRow relies on this being the one sequence that ever
// closes a styled run.
const chromaReset = "\x1b[0m"

// gutterCols is the two 4-col line-number gutters plus their separating
// spaces (P3-design.md §2.5: "two line-number gutters (old/new, 4 cols
// each)"), fixed width regardless of pane width.
const gutterCols = 4 + 1 + 4 + 1

// diffview is the custom virtualized window over one loaded diff's flattened
// rows (P3-design.md §2.5): NOT bubbles/viewport — it keeps offset/height
// over []renderLine and styles only the visible slice per frame, so a
// 5k-line diff never gets fully re-styled just to scroll one line.
type diffview struct {
	diff        model.Diff
	lines       []renderLine
	fileOffsets []int
	tooLarge    []bool // per file index: exceeds the chroma size threshold
	expanded    map[string]bool

	offset int // topmost visible render-line index
	height int // last known viewport height, kept current by render/setHeight
}

// setDiff loads a freshly-fetched diff. Expand overrides persist across
// loads (keyed by file hash, so an unchanged file keeps its `o` state across
// a refetch of the same worktree) — scroll position now matches that (DEFECT
// D4 fix): a refetch of the SAME worktree (e.g. a diff.ready-triggered
// refresh while mid-review) preserves offset, re-clamped by reflow against
// the new line count; only switching to a genuinely different worktree
// resets to the top. An empty incoming WorktreeID never counts as "same" —
// it can't be positively confirmed, so this falls back to the always-safe
// reset.
func (v *diffview) setDiff(d model.Diff) {
	sameWorktree := d.WorktreeID != "" && d.WorktreeID == v.diff.WorktreeID
	v.diff = d
	if v.expanded == nil {
		v.expanded = map[string]bool{}
	}
	v.reflow() // re-clamps offset against the new line count either way
	if !sameWorktree {
		v.offset = 0
	}
}

// reflow recomputes lines/fileOffsets/tooLarge from the current diff and
// expand overrides — called on load and whenever `o` changes a file's
// collapsed state (the row count for that file changes).
func (v *diffview) reflow() {
	v.lines, v.fileOffsets = flattenDiff(v.diff, v.expanded)
	v.tooLarge = make([]bool, len(v.diff.Files))
	for i, f := range v.diff.Files {
		v.tooLarge[i] = chromaTooLarge(f)
	}
	v.clampOffset()
}

// setHeight records the current viewport height and re-clamps the scroll
// offset against it — called by render() every frame, and separately by
// radarView.view() even while showing a loading/error placeholder, so the
// height is already accurate the instant real data lands (see highlight.go's
// visibility-driven highlight trigger).
func (v *diffview) setHeight(h int) {
	if h < 1 {
		h = 1
	}
	v.height = h
	v.clampOffset()
}

// clampOffset keeps offset within [0, maxScroll], where maxScroll is 0 when
// the whole diff already fits in the viewport (P3-design.md's window-math
// property: "offset always clamped, view height honored").
func (v *diffview) clampOffset() {
	max := len(v.lines) - v.height
	if max < 0 {
		max = 0
	}
	if v.offset > max {
		v.offset = max
	}
	if v.offset < 0 {
		v.offset = 0
	}
}

func (v *diffview) scroll(delta int) { v.offset += delta; v.clampOffset() }

func (v *diffview) pageSize() int {
	if v.height < 1 {
		return 1
	}
	return v.height
}

func (v *diffview) halfPageSize() int {
	h := v.pageSize() / 2
	if h < 1 {
		h = 1
	}
	return h
}

// ---- scroll keys (P3-design.md §1.3's diff-pane scrolling set) ----

func (v *diffview) LineDown()     { v.scroll(1) }
func (v *diffview) LineUp()       { v.scroll(-1) }
func (v *diffview) PageDown()     { v.scroll(v.pageSize()) }
func (v *diffview) PageUp()       { v.scroll(-v.pageSize()) }
func (v *diffview) HalfPageDown() { v.scroll(v.halfPageSize()) }
func (v *diffview) HalfPageUp()   { v.scroll(-v.halfPageSize()) }
func (v *diffview) Top()          { v.offset = 0 }
func (v *diffview) Bottom()       { v.offset = len(v.lines); v.clampOffset() }

// currentFileIndex is "the file card under the cursor": the last file whose
// header has scrolled to or past the top of the viewport. There is no
// separate row-cursor in this read-only pane (§2.5) — scroll position alone
// determines it, which is what `[`/`]`/`o` act on.
func (v *diffview) currentFileIndex() int {
	idx := 0
	for i, off := range v.fileOffsets {
		if off <= v.offset {
			idx = i
		} else {
			break
		}
	}
	return idx
}

// PrevFile jumps to the current file's own header if the viewport has
// scrolled past it, else to the previous file's header (so repeated presses
// walk backward one file at a time, matching typical pager conventions).
func (v *diffview) PrevFile() {
	cur := v.currentFileIndex()
	switch {
	case cur < len(v.fileOffsets) && v.offset > v.fileOffsets[cur]:
		v.offset = v.fileOffsets[cur]
	case cur > 0:
		v.offset = v.fileOffsets[cur-1]
	}
	v.clampOffset()
}

func (v *diffview) NextFile() {
	cur := v.currentFileIndex()
	if cur+1 < len(v.fileOffsets) {
		v.offset = v.fileOffsets[cur+1]
	}
	v.clampOffset()
}

// ToggleFold flips the `o` expand override for the file under the cursor.
// Only collapsed-by-default files are affected (P3-design.md §1.3: "o
// expand/collapse ... collapsed-by-default files, §2.5") — a binary file has
// nothing to expand, and an already-fully-shown file has nothing to fold.
func (v *diffview) ToggleFold() {
	fi := v.currentFileIndex()
	if fi < 0 || fi >= len(v.diff.Files) {
		return
	}
	f := v.diff.Files[fi]
	if f.Binary || !isCollapsedByDefault(f) {
		return
	}
	v.expanded[f.Hash] = !v.expanded[f.Hash]
	v.reflow()
	if fi < len(v.fileOffsets) {
		v.offset = v.fileOffsets[fi] // keep the toggled file in view rather than an arbitrary numeric offset
	}
	v.clampOffset()
}

// currentFile returns the file under the cursor (same file currentFileIndex
// resolves for `[`/`]`/`o`), used by Review's space-toggle to know which
// file/hash to send. ok=false only when the diff has no files at all.
func (v *diffview) currentFile() (model.DiffFile, bool) {
	fi := v.currentFileIndex()
	if fi < 0 || fi >= len(v.diff.Files) {
		return model.DiffFile{}, false
	}
	return v.diff.Files[fi], true
}

// setReviewed applies a reviewed state for one file, keyed by path (matches
// model.Diff.Reviewed's own keying): Review's space-toggle uses it for the
// optimistic flip before the request round-trips, and reviewOKMsg/a 409
// revert use it for server-truth reconciliation.
func (v *diffview) setReviewed(path string, reviewed bool) {
	if v.diff.Reviewed == nil {
		v.diff.Reviewed = map[string]bool{}
	}
	v.diff.Reviewed[path] = reviewed
}

// ---- rendering ----

// render paints exactly the visible slice [offset, offset+height) into a
// width x height block — the one place per-row styling/highlighting happens,
// so cost per frame is O(visible rows) regardless of diff size (§6's <16ms
// scroll budget). sev maps each file path a guardrail hit names to that
// file's own worst severity ("danger"|"warn") — the file header's tag grades
// red/amber to match (ux-expert P1-1a), rather than a fixed "any hit =
// danger" bool.
func (v *diffview) render(width, height int, hl *highlightCache, sev map[string]string) string {
	v.setHeight(height)
	if len(v.lines) == 0 {
		return lipgloss.NewStyle().Width(width).Height(v.height).Render(styles.Dim.Render("no changes"))
	}

	addPrefix := bgPrefix(styles.AddBg)
	delPrefix := bgPrefix(styles.DelBg)

	end := v.offset + v.height
	if end > len(v.lines) {
		end = len(v.lines)
	}
	var b strings.Builder
	for i := v.offset; i < end; i++ {
		if i > v.offset {
			b.WriteByte('\n')
		}
		b.WriteString(v.renderRow(v.lines[i], width, hl, sev, addPrefix, delPrefix))
	}
	// MaxWidth here (not Width) is a truncating safety net, not the primary
	// sizing mechanism: every row is already exactly `width` cells via
	// clipWidth/renderCodeRow. Width() alone would *wrap* an overlong row
	// instead of clipping it (confirmed against lipgloss v1.1.0), which
	// would silently corrupt the whole side-by-side layout with the
	// sidebar — exactly the bug clipWidth exists to avoid.
	return lipgloss.NewStyle().MaxWidth(width).Height(v.height).Render(b.String())
}

func (v *diffview) renderRow(ln renderLine, width int, hl *highlightCache, sev map[string]string, addPrefix, delPrefix string) string {
	switch ln.kind {
	case rowFileHeader:
		return v.renderFileHeader(ln.fileIdx, width, sev)
	case rowHunkHeader:
		return clipWidth(styles.Accent.Render(ln.content), width)
	case rowNote:
		return clipWidth("  "+styles.Dim.Render(ln.content), width)
	case rowCode:
		return v.renderCodeRow(ln, width, hl, addPrefix, delPrefix)
	default:
		return ""
	}
}

func (v *diffview) renderFileHeader(fi int, width int, sev map[string]string) string {
	f := v.diff.Files[fi]
	dir, base := splitDirBase(displayPath(f))
	hitSev := sev[f.Path]
	if hitSev == "" && f.OldPath != "" {
		hitSev = sev[f.OldPath]
	}

	nameStyle := styles.Txt
	if hitSev != "" {
		nameStyle = styles.Warn
	}
	name := styles.Faint.Render(dir) + nameStyle.Bold(true).Render(base)

	// Both the hit's own severity tag AND the file's status tag render when
	// applicable (ux-expert P1-1a) — the old single-switch version showed at
	// most one, so e.g. a warn-severity binary-added file lost its "added"
	// status entirely under the (then-always-red) "danger" tag.
	var tag string
	switch hitSev {
	case "danger":
		tag = "  " + styles.Del.Render("danger")
	case "warn":
		tag = "  " + styles.Warn.Render("warn")
	}
	if f.Status != model.FileModified {
		tag += "  " + styles.Dim.Render(string(f.Status))
	}
	if v.diff.Reviewed[f.Path] {
		tag += "  " + styles.Add.Render("✓")
	}

	stats := "  " + styles.Add.Render(fmt.Sprintf("+%d", f.Stats.Add)) + " " + styles.Del.Render(fmt.Sprintf("-%d", f.Stats.Del))

	var note string
	if showsCode(f, v.expanded) && v.tooLarge[fi] {
		note = "  " + styles.Dim.Render("highlighting off (large file)")
	}

	return clipWidth(name+tag+stats+note, width)
}

func (v *diffview) renderCodeRow(ln renderLine, width int, hl *highlightCache, addPrefix, delPrefix string) string {
	oldStr, newStr := "", ""
	if ln.oldNum > 0 {
		oldStr = strconv.Itoa(ln.oldNum)
	}
	if ln.newNum > 0 {
		newStr = strconv.Itoa(ln.newNum)
	}
	gutter := styles.Faint.Render(fmt.Sprintf("%4s %4s ", oldStr, newStr))

	codeWidth := width - gutterCols
	if codeWidth < 0 {
		codeWidth = 0
	}
	sizedCode := fitWidth(v.styledCode(ln, hl), codeWidth)

	row := gutter + sizedCode
	switch ln.lineKind {
	case model.LineAdd:
		return tintRow(addPrefix, row)
	case model.LineDel:
		return tintRow(delPrefix, row)
	default:
		return clipWidth(row, width)
	}
}

// styledCode returns the code column's styled text for one row: the
// chroma-highlighted line when the file's async highlight has landed (§2.5's
// per-file cache), otherwise a plain single-color fallback — "until it
// arrives, code rows render plain (fg only)".
func (v *diffview) styledCode(ln renderLine, hl *highlightCache) string {
	if hl != nil {
		f := v.diff.Files[ln.fileIdx]
		if hlLines, ok := hl.get(f.Hash); ok && ln.codeIdx < len(hlLines) {
			return hlLines[ln.codeIdx]
		}
	}
	return plainCodeStyle(ln.lineKind).Render(ln.content)
}

func plainCodeStyle(k model.LineKind) lipgloss.Style {
	switch k {
	case model.LineAdd:
		return styles.Add
	case model.LineDel:
		return styles.Del
	default:
		return styles.Dim
	}
}

// clipWidth is diffview's own ANSI-safe truncation (mirrors sidebar.go's
// clampWidth intent, but rune/ANSI-aware via lipgloss's MaxWidth — a diff
// row can already carry chroma escape codes, which a byte clamp would
// corrupt). Width-only, no color: consistent with app.go/sidebar.go's
// existing use of bare lipgloss.NewStyle() for layout, not palette, outside
// styles.go. Truncate-only (no padding) — fine for header/hunk/note rows,
// which carry no background tint that needs to reach the row's far edge.
func clipWidth(s string, width int) string {
	if width < 0 {
		width = 0
	}
	return lipgloss.NewStyle().MaxWidth(width).Render(s)
}

// fitWidth ANSI-safely fits s to *exactly* width cells: truncated if
// longer, space-padded if shorter, never wrapped. Used for the code column,
// which does need to reach the row's far edge (tintRow's background must
// cover the whole row, short lines included). lipgloss.Style.Width alone
// soft-wraps overflow instead of truncating it (confirmed against lipgloss
// v1.1.0 — the wrap this function exists to avoid), and MaxWidth alone
// truncates but never pads a shorter string — so this runs them as two
// separate Render passes: truncate first (content that may still be too
// long), then pad the now-guaranteed-short-enough plain result.
func fitWidth(s string, width int) string {
	if width < 0 {
		width = 0
	}
	truncated := lipgloss.NewStyle().MaxWidth(width).Render(s)
	return lipgloss.NewStyle().Width(width).Render(truncated)
}

// bgPrefix returns the raw escape-sequence prefix that opens st's
// background color, by rendering it around a private marker byte and
// slicing off everything from the marker onward. Under a profile that
// renders no color at all (NO_COLOR/ascii), st.Render produces no escape
// codes and this returns "" — tintRow then leaves the row untouched, which
// is correct: there's nothing to tint or reinject.
func bgPrefix(st lipgloss.Style) string {
	const marker = "\x00"
	rendered := st.Render(marker)
	if i := strings.Index(rendered, marker); i >= 0 {
		return rendered[:i]
	}
	return ""
}

// tintRow applies prefix as row's background across its *entire* width,
// including past any embedded reset. A chroma-highlighted row resets after
// nearly every token (so a pager can show any single line in isolation —
// confirmed against chroma v2.27.0's formatters/tty_*.go), which would
// otherwise cut our background off after the row's first token. The "delta"
// trick: reopen the background immediately after every reset, then close
// once at the very end so the tint never bleeds into the next row.
func tintRow(prefix, row string) string {
	if prefix == "" {
		return row
	}
	return prefix + strings.ReplaceAll(row, chromaReset, chromaReset+prefix) + chromaReset
}
