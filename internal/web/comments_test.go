package web

import (
	"testing"
	"time"

	"github.com/navbytes/wt-cockpit/internal/model"
)

func cv(file string, line int, side, id string) model.CommentView {
	return model.CommentView{Comment: model.Comment{
		ID: id, File: file, Line: line, Side: side, Body: "b", Author: "a", State: "open", At: time.Now(),
	}}
}

// ---- fileCommentGroups ----

func TestFileCommentGroupsFiltersByFileAndGroupsByLineAndSide(t *testing.T) {
	views := []model.CommentView{
		cv("a.go", 10, "new", "c1"),
		cv("a.go", 10, "new", "c2"), // same (line,side): must join c1's group
		cv("a.go", 5, "old", "c3"),
		cv("b.go", 10, "new", "c4"), // different file: must not appear
	}
	groups := fileCommentGroups(views, "a.go", 2)
	if len(groups) != 2 {
		t.Fatalf("groups = %+v, want 2 distinct (line,side) buckets for a.go", groups)
	}
	// Sorted by line then side: line 5 before line 10.
	if groups[0].Anchor != "2:old:5" || len(groups[0].Comments) != 1 {
		t.Errorf("groups[0] = %+v, want anchor 2:old:5 with 1 comment", groups[0])
	}
	if groups[1].Anchor != "2:new:10" || len(groups[1].Comments) != 2 {
		t.Errorf("groups[1] = %+v, want anchor 2:new:10 with 2 comments (c1+c2)", groups[1])
	}
}

func TestFileCommentGroupsEmptyForFileWithNoComments(t *testing.T) {
	views := []model.CommentView{cv("a.go", 1, "new", "c1")}
	if groups := fileCommentGroups(views, "b.go", 0); len(groups) != 0 {
		t.Errorf("groups = %+v, want none for a file with zero comments", groups)
	}
}

func TestFileCommentGroupsOldSortsBeforeNewOnSameLine(t *testing.T) {
	views := []model.CommentView{
		cv("a.go", 3, "new", "c1"),
		cv("a.go", 3, "old", "c2"),
	}
	groups := fileCommentGroups(views, "a.go", 0)
	if len(groups) != 2 || groups[0].Anchor != "0:old:3" || groups[1].Anchor != "0:new:3" {
		t.Errorf("groups = %+v, want old before new on the same line", groups)
	}
}

func TestCommentLabelFileLevelVsLineAnchored(t *testing.T) {
	if got := commentLabel(0, "new"); got != "file-level" {
		t.Errorf("commentLabel(0, ...) = %q, want %q", got, "file-level")
	}
	if got := commentLabel(19, "new"); got != "line 19 (new)" {
		t.Errorf("commentLabel(19, new) = %q, want %q", got, "line 19 (new)")
	}
	if got := commentLabel(4, "old"); got != "line 4 (old)" {
		t.Errorf("commentLabel(4, old) = %q, want %q", got, "line 4 (old)")
	}
}

// ---- orphanedComments ----

func TestOrphanedCommentsFiltersAndSorts(t *testing.T) {
	open := cv("a.go", 1, "new", "c-open")
	o1 := cv("z.go", 1, "new", "c-o1")
	o1.Orphaned = true
	o2 := cv("a.go", 9, "new", "c-o2")
	o2.Orphaned = true
	o3 := cv("a.go", 2, "new", "c-o3")
	o3.Orphaned = true

	got := orphanedComments([]model.CommentView{open, o1, o2, o3})
	if len(got) != 3 {
		t.Fatalf("got %d orphaned comments, want 3 (non-orphaned excluded)", len(got))
	}
	// Sorted by file then line: a.go/2, a.go/9, z.go/1.
	if got[0].ID != "c-o3" || got[1].ID != "c-o2" || got[2].ID != "c-o1" {
		t.Errorf("order = [%s %s %s], want [c-o3 c-o2 c-o1]", got[0].ID, got[1].ID, got[2].ID)
	}
}

func TestOrphanedCommentsNoneReturnsEmpty(t *testing.T) {
	if got := orphanedComments([]model.CommentView{cv("a.go", 1, "new", "c1")}); len(got) != 0 {
		t.Errorf("got %+v, want none", got)
	}
}

// ---- buildCommentsFragmentView ----

func TestBuildCommentsFragmentViewShapesFilesAndOrphaned(t *testing.T) {
	d := model.Diff{Files: []model.DiffFile{{Path: "a.go"}, {Path: "b.go"}}}
	inDiff := cv("a.go", 1, "new", "c1")
	orphan := cv("gone.go", 1, "new", "c2")
	orphan.Orphaned = true

	got := buildCommentsFragmentView(d, []model.CommentView{inDiff, orphan})
	if len(got.Files) != 2 {
		t.Fatalf("Files = %+v, want one entry per diff file (2)", got.Files)
	}
	if got.Files[0].Idx != 0 || got.Files[0].Path != "a.go" || len(got.Files[0].CommentGroups) != 1 {
		t.Errorf("Files[0] = %+v, want idx 0, path a.go, with a.go's one group", got.Files[0])
	}
	if got.Files[1].Path != "b.go" {
		t.Errorf("Files[1].Path = %q, want b.go — app.js's live-refresh reconciles the strip by this field, not by index", got.Files[1].Path)
	}
	if len(got.Files[1].CommentGroups) != 0 {
		t.Errorf("Files[1] (b.go) = %+v, want no groups", got.Files[1])
	}
	if len(got.Orphaned) != 1 || got.Orphaned[0].ID != "c2" {
		t.Errorf("Orphaned = %+v, want just c2", got.Orphaned)
	}
}
