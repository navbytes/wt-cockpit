// Package diffparse turns `git diff` unified output into structured model.DiffFile
// values. It is a pure function with no I/O so it is cheap to test exhaustively and
// safe to run off the UI goroutine.
package diffparse

import (
	"strconv"
	"strings"

	"github.com/navbytes/wt-cockpit/internal/model"
)

// Parse converts unified diff text into structured files. Unknown/garbage lines
// are skipped rather than erroring — git output evolves and a viewer should be
// resilient, not brittle.
func Parse(diff string) []model.DiffFile {
	var files []model.DiffFile
	lines := strings.Split(diff, "\n")

	var cur *model.DiffFile
	var hunk *model.Hunk
	oldNum, newNum := 0, 0

	flushFile := func() {
		if cur != nil {
			if hunk != nil {
				cur.Hunks = append(cur.Hunks, *hunk)
				hunk = nil
			}
			files = append(files, *cur)
			cur = nil
		}
	}

	for _, ln := range lines {
		switch {
		case strings.HasPrefix(ln, "diff --git "):
			flushFile()
			a, b := parseDiffGitPaths(ln)
			cur = &model.DiffFile{Path: b, OldPath: a, Status: model.FileModified}

		case cur == nil:
			// Preamble before the first file header; ignore.
			continue

		// Every case below down to "@@" only ever appears in a per-file
		// *header* block: git always emits new/deleted/rename/index/Binary/
		// ---/+++ lines before a file's first hunk, never after. hunk == nil
		// means exactly that — "no @@ has been seen yet for cur" — since it's
		// reset to nil only by flushFile, at the next "diff --git " (or EOF).
		// Without this guard, a deleted (or added) file's own hunk-body
		// content can byte-collide with these header prefixes: git marks a
		// removed content line with a literal leading '-', so a deleted
		// line whose text itself starts with "-- " (two dashes, a space)
		// comes out as "--- <text>" — indistinguishable by prefix alone from
		// a real "--- a/<path>" header — and symmetrically "++ " content on
		// an added line collides with "+++ ". "new file"/"deleted
		// file"/"rename from/to "/"index "/"Binary files" can never actually
		// collide this way in real git output (every hunk-body line carries
		// a mandatory leading '+'/'-'/' ' marker, and none of those texts
		// start with one), but they're gated the same way for a uniform,
		// cheap state machine rather than special-casing just the two that
		// do collide.
		case hunk == nil && strings.HasPrefix(ln, "new file"):
			cur.Status = model.FileAdded
			cur.OldPath = ""

		case hunk == nil && strings.HasPrefix(ln, "deleted file"):
			cur.Status = model.FileDeleted

		case hunk == nil && strings.HasPrefix(ln, "rename from "):
			cur.Status = model.FileRenamed
			// No a/b prefix on this line (unlike ---/+++/diff --git), just the
			// bare path — only undo quoting/escaping, don't strip a fake prefix.
			cur.OldPath = unquotePath(strings.TrimPrefix(ln, "rename from "))

		case hunk == nil && strings.HasPrefix(ln, "rename to "):
			cur.Status = model.FileRenamed
			cur.Path = unquotePath(strings.TrimPrefix(ln, "rename to "))

		case hunk == nil && strings.HasPrefix(ln, "index "):
			cur.OldBlob, cur.NewBlob = parseIndexLine(ln)

		case hunk == nil && strings.HasPrefix(ln, "Binary files"):
			cur.Binary = true

		case hunk == nil && strings.HasPrefix(ln, "--- "):
			// Old path; "/dev/null" means an add (OldPath is already "" from the
			// "new file" case above). A real path here is authoritative for a
			// plain modify or a delete — where Path itself holds the old path,
			// per DiffFile's doc comment, since there's no "+++" real path to set
			// it — and redundant-but-harmless for a rename (already set by
			// "rename from ", which this just reproduces identically).
			p := strings.TrimPrefix(ln, "--- ")
			if p != "/dev/null" {
				cur.OldPath = stripPrefix(p)
				if cur.Status == model.FileDeleted {
					cur.Path = cur.OldPath
				}
			}

		case hunk == nil && strings.HasPrefix(ln, "+++ "):
			p := strings.TrimPrefix(ln, "+++ ")
			if p != "/dev/null" {
				cur.Path = stripPrefix(p)
			}

		case strings.HasPrefix(ln, "@@"):
			if hunk != nil {
				cur.Hunks = append(cur.Hunks, *hunk)
			}
			h := parseHunkHeader(ln)
			hunk = &h
			oldNum = h.OldStart
			newNum = h.NewStart

		case hunk != nil && strings.HasPrefix(ln, "+"):
			hunk.Lines = append(hunk.Lines, model.Line{
				Kind: model.LineAdd, NewNum: newNum, Content: SanitizeControl(ln[1:]),
			})
			newNum++
			cur.Stats.Add++

		case hunk != nil && strings.HasPrefix(ln, "-"):
			hunk.Lines = append(hunk.Lines, model.Line{
				Kind: model.LineDel, OldNum: oldNum, Content: SanitizeControl(ln[1:]),
			})
			oldNum++
			cur.Stats.Del++

		case hunk != nil && strings.HasPrefix(ln, " "):
			hunk.Lines = append(hunk.Lines, model.Line{
				Kind: model.LineContext, OldNum: oldNum, NewNum: newNum, Content: SanitizeControl(ln[1:]),
			})
			oldNum++
			newNum++

		case ln == `\ No newline at end of file`:
			continue
		}
	}
	flushFile()

	for i := range files {
		files[i].Stats.Files = 1
	}
	return files
}

// parseDiffGitPaths extracts a/ and b/ paths from a "diff --git a/x b/y" line.
// The split on the first space is naive and can mis-tokenize when a path
// itself contains a space — harmless in practice, because both return values
// here are only ever provisional: OldPath is immediately overwritten by
// "rename from "/"--- ", and Path by "rename to "/"+++ ", for every status
// this parser produces except a mode-only change or a 100%-similarity rename
// with an unchanged mode, neither of which has any hunks (nothing reviewable)
// riding on the exact pre-image path anyway.
func parseDiffGitPaths(ln string) (old, new string) {
	rest := strings.TrimPrefix(ln, "diff --git ")
	// Paths are space-separated but may themselves contain spaces; the common
	// case (no spaces) is handled directly, which covers the overwhelming majority.
	fields := strings.SplitN(rest, " ", 2)
	if len(fields) == 2 {
		return stripPrefix(fields[0]), stripPrefix(fields[1])
	}
	return "", stripPrefix(rest)
}

// unquotePath undoes the two independent transformations git applies to a
// path on a diff header line: core.quotePath=true (the default) wraps a path
// containing a non-ASCII byte (or another special character) in double
// quotes with C-style octal escapes (e.g. café.txt -> "caf\303\251.txt"), and
// — completely independently — git always appends a bare trailing tab after
// a path (quoted or not) that merely *contains a space*, to mark
// unambiguously where the filename ends. Either, both, or neither may apply.
func unquotePath(p string) string {
	p = strings.TrimSuffix(p, "\t")
	if len(p) >= 2 && p[0] == '"' && p[len(p)-1] == '"' {
		if unquoted, err := strconv.Unquote(p); err == nil {
			// DEFECT D2: an octal escape here can decode straight back to a
			// raw control byte (e.g. \033 -> a literal ESC) — sanitize the
			// decoded result, not just the fallback below.
			return SanitizeControl(unquoted)
		}
		// Malformed/unsupported escape: fall through with the tab trimmed but
		// the quotes left as-is rather than losing the value entirely.
	}
	return SanitizeControl(p)
}

// stripPrefix removes a leading a/ or b/ from a diff path — git always adds
// one on "---"/"+++"/"diff --git" lines even though the real path has no such
// prefix — after first undoing git's path quoting/escaping.
func stripPrefix(p string) string {
	p = unquotePath(p)
	if strings.HasPrefix(p, "a/") || strings.HasPrefix(p, "b/") {
		return p[2:]
	}
	return p
}

// parseIndexLine extracts the old/new blob object ids from a per-file
// "index <old>..<new>[ <mode>]" header line. Git omits the trailing mode when
// the file's mode also changed (carried instead by separate "old mode"/"new
// mode" lines) and omits the line entirely for a pure, content-identical
// (100%-similarity) rename — an absent index line just leaves both "".
func parseIndexLine(ln string) (oldBlob, newBlob string) {
	rest := strings.TrimPrefix(ln, "index ")
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return "", ""
	}
	blobs := strings.SplitN(fields[0], "..", 2)
	if len(blobs) != 2 {
		return "", ""
	}
	return blobs[0], blobs[1]
}

// parseHunkHeader parses "@@ -oldStart,oldLen +newStart,newLen @@ optional".
func parseHunkHeader(ln string) model.Hunk {
	// The trailing "optional" section (git's nearby-function context, e.g.
	// "@@ -18,3 +18,4 @@ func NewToken(") is copied verbatim from the source
	// file, same as any hunk line's content — DEFECT D2 applies here too.
	h := model.Hunk{Header: SanitizeControl(ln)}
	// Find the two @@ markers.
	body := ln
	if i := strings.Index(ln[2:], "@@"); i >= 0 {
		body = ln[2 : i+2]
	}
	for _, tok := range strings.Fields(body) {
		if strings.HasPrefix(tok, "-") {
			h.OldStart = firstInt(tok[1:])
		} else if strings.HasPrefix(tok, "+") {
			h.NewStart = firstInt(tok[1:])
		}
	}
	return h
}

func firstInt(s string) int {
	if i := strings.IndexByte(s, ','); i >= 0 {
		s = s[:i]
	}
	n, _ := strconv.Atoi(s)
	return n
}

// SanitizeControl neutralizes bytes that could otherwise drive a terminal's
// escape-sequence interpreter once diff output reaches a real screen (DEFECT
// D2 — a hostile agent worktree's tracked content/paths are untrusted input,
// and every consumer downstream of Parse, from `wt diff` to the TUI's
// lipgloss/chroma render path, treats them as plain text to display, not
// bytes to execute). Replaces C0 controls (0x00-0x1F, tab excepted — it's
// just whitespace) and DEL (0x7F) with the standard visible caret notation
// (ESC -> "^[", BEL -> "^G", DEL -> "^?"), and C1 controls (0x80-0x9F, once
// UTF-8-decoded — git's own path-quoting escapes can resurrect one of these
// from an octal escape, see unquotePath) with the same notation as their
// classic 7-bit equivalent. Exported so any other renderer of a parsed diff
// (internal/tui's flattenDiff, in particular, which must stay safe even for
// a model.Diff assembled by hand rather than through Parse) can reuse the
// exact same rule instead of a second, drifting implementation.
func SanitizeControl(s string) string {
	if strings.IndexFunc(s, isControlRune) < 0 {
		return s // fast path: control bytes are rare in real code
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == 0x7f:
			b.WriteString("^?")
		case r < 0x20:
			b.WriteByte('^')
			b.WriteByte(byte(r) + '@') // 0x00-0x1F -> ^@ .. ^_ (standard caret notation)
		case r >= 0x80 && r <= 0x9f:
			b.WriteByte('^')
			b.WriteByte(byte(r-0x80) + '@') // C1 -> same glyph as its C0/7-bit equivalent
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func isControlRune(r rune) bool {
	return (r < 0x20 && r != '\t') || r == 0x7f || (r >= 0x80 && r <= 0x9f)
}
