// comments.go builds the reading room's comment view-models: the per-file
// "strip" of threads (P4-design.md §1.5's fallback placement for stale/
// orphaned comments, generalised here to every comment — see room.tmpl's own
// doc comment for why the side-by-side grid can't safely host a variable-
// height thread inline) and the page-bottom section for comments whose file
// left the diff entirely. Shared by both the full room page (pages.go) and
// the live-refresh fragment (fragments.go) so the two can never render
// different markup for the same data.
package web

import (
	"fmt"
	"sort"
	"strconv"

	"github.com/navbytes/wt-cockpit/internal/model"
)

// commentGroupView is every comment anchored to the same (file, line, side)
// within one file, in insertion order. Anchor is rendered as the group's
// data-anchor attribute verbatim in the "fileIdx:side:line" shape the design
// froze for the comments fragment (P4-design.md §2/§3) — kept on the markup
// for clarity even though app.js (below) reconciles at the coarser per-file
// container granularity, which is simpler and never leaves a stale group
// behind (see fragments.go's handleFragmentComments doc comment).
type commentGroupView struct {
	Anchor   string
	Label    string // "file-level" or "line 19 (new)"
	Comments []model.CommentView
}

// fileCommentsView is the lightweight (no highlighting, no hunks) per-file
// shape the comments fragment renders — fileCardView also satisfies the
// "fileCommentsStrip" template's field requirements (Idx, Path,
// CommentGroups), so the exact same template block renders both the full room
// page and the fragment without ever risking the two drifting apart. Path is
// what app.js's live-refresh reconciles the per-file strip on (P4-fixes.md
// #10): Idx alone drifts if a file's position in the diff shifts between the
// page's initial render and a later fragment fetch, but a file's path is
// stable for as long as it stays in the diff at all.
type fileCommentsView struct {
	Idx           int
	Path          string
	CommentGroups []commentGroupView
}

// commentsFragmentView is GET /fragment/comments's template data: every
// file's comment groups plus the page-bottom orphaned section, computed
// fresh from the engine on every request (P4-design.md §1.5: Stale/Orphaned
// are never persisted, always derived at read time).
type commentsFragmentView struct {
	Files    []fileCommentsView
	Orphaned []model.CommentView
}

// buildCommentsFragmentView assembles GET /fragment/comments's data from a
// diff snapshot and its comment views — shared by fragments.go's handler.
func buildCommentsFragmentView(d model.Diff, views []model.CommentView) commentsFragmentView {
	files := make([]fileCommentsView, len(d.Files))
	for i, f := range d.Files {
		files[i] = fileCommentsView{Idx: i, Path: f.Path, CommentGroups: fileCommentGroups(views, f.Path, i)}
	}
	return commentsFragmentView{Files: files, Orphaned: orphanedComments(views)}
}

// fileCommentGroups groups views belonging to filePath into
// commentGroupViews, one per distinct (line, side), sorted by line then side
// ("old" before "new") for a deterministic, golden-testable order. fileIdx is
// baked into the anchor string so the room page and the fragment agree on it
// byte for byte without either needing to re-derive it from a path lookup.
func fileCommentGroups(views []model.CommentView, filePath string, fileIdx int) []commentGroupView {
	type key struct {
		line int
		side string
	}
	groups := map[key][]model.CommentView{}
	var keys []key
	for _, v := range views {
		if v.File != filePath {
			continue
		}
		k := key{v.Line, v.Side}
		if _, ok := groups[k]; !ok {
			keys = append(keys, k)
		}
		groups[k] = append(groups[k], v)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].line != keys[j].line {
			return keys[i].line < keys[j].line
		}
		return sideRank(keys[i].side) < sideRank(keys[j].side)
	})

	out := make([]commentGroupView, 0, len(keys))
	for _, k := range keys {
		out = append(out, commentGroupView{
			Anchor:   commentAnchor(fileIdx, k.side, k.line),
			Label:    commentLabel(k.line, k.side),
			Comments: groups[k],
		})
	}
	return out
}

// sideRank orders "old" before "new" (a rename's pre-image reads first) —
// plain string comparison would put "new" first (n < o), which is backwards.
func sideRank(side string) int {
	if side == "old" {
		return 0
	}
	return 1
}

func commentAnchor(fileIdx int, side string, line int) string {
	return strconv.Itoa(fileIdx) + ":" + side + ":" + strconv.Itoa(line)
}

func commentLabel(line int, side string) string {
	if line == 0 {
		return "file-level"
	}
	return fmt.Sprintf("line %d (%s)", line, side)
}

// orphanedComments returns every Orphaned view (its file left the diff
// entirely — there is no file card left to attach a strip to), sorted by
// file then line then side for deterministic rendering. Rendered flat (one
// card per comment, each labelled with its own file/line) rather than
// grouped: unlike fileCommentGroups, there is no fileIdx to group under here,
// and the whole section refreshes as one unit on every comment.changed
// (fragments.go), so per-group anchoring buys nothing.
func orphanedComments(views []model.CommentView) []model.CommentView {
	var out []model.CommentView
	for _, v := range views {
		if v.Orphaned {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		if out[i].Line != out[j].Line {
			return out[i].Line < out[j].Line
		}
		return out[i].Side < out[j].Side
	})
	return out
}
