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

		case strings.HasPrefix(ln, "new file"):
			cur.Status = model.FileAdded
			cur.OldPath = ""

		case strings.HasPrefix(ln, "deleted file"):
			cur.Status = model.FileDeleted

		case strings.HasPrefix(ln, "rename from "):
			cur.Status = model.FileRenamed
			// No a/b prefix on this line (unlike ---/+++/diff --git), just the
			// bare path — only undo quoting/escaping, don't strip a fake prefix.
			cur.OldPath = unquotePath(strings.TrimPrefix(ln, "rename from "))

		case strings.HasPrefix(ln, "rename to "):
			cur.Status = model.FileRenamed
			cur.Path = unquotePath(strings.TrimPrefix(ln, "rename to "))

		case strings.HasPrefix(ln, "index "):
			cur.OldBlob, cur.NewBlob = parseIndexLine(ln)

		case strings.HasPrefix(ln, "Binary files"):
			cur.Binary = true

		case strings.HasPrefix(ln, "--- "):
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

		case strings.HasPrefix(ln, "+++ "):
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
				Kind: model.LineAdd, NewNum: newNum, Content: ln[1:],
			})
			newNum++
			cur.Stats.Add++

		case hunk != nil && strings.HasPrefix(ln, "-"):
			hunk.Lines = append(hunk.Lines, model.Line{
				Kind: model.LineDel, OldNum: oldNum, Content: ln[1:],
			})
			oldNum++
			cur.Stats.Del++

		case hunk != nil && strings.HasPrefix(ln, " "):
			hunk.Lines = append(hunk.Lines, model.Line{
				Kind: model.LineContext, OldNum: oldNum, NewNum: newNum, Content: ln[1:],
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
			return unquoted
		}
		// Malformed/unsupported escape: fall through with the tab trimmed but
		// the quotes left as-is rather than losing the value entirely.
	}
	return p
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
	h := model.Hunk{Header: ln}
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
