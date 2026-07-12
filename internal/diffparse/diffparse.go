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
			cur.OldPath = strings.TrimPrefix(ln, "rename from ")

		case strings.HasPrefix(ln, "rename to "):
			cur.Status = model.FileRenamed
			cur.Path = strings.TrimPrefix(ln, "rename to ")

		case strings.HasPrefix(ln, "Binary files"):
			cur.Binary = true

		case strings.HasPrefix(ln, "--- "):
			// old path; "/dev/null" means an add. Real path handled via +++.
			continue

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

// stripPrefix removes a leading a/ or b/ (or quoted variants) from a diff path.
func stripPrefix(p string) string {
	p = strings.Trim(p, "\"")
	if strings.HasPrefix(p, "a/") || strings.HasPrefix(p, "b/") {
		return p[2:]
	}
	return p
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
