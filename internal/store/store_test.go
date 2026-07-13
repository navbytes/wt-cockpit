package store

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/navbytes/wt-cockpit/internal/model"
)

func TestReviewedRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := OpenJSON(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetReviewed("wt1", "a.go", "hash-a1"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetReviewed("wt1", "b.go", "hash-b1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Unreview("wt1", "a.go"); err != nil {
		t.Fatal(err)
	}

	got, err := s.ReviewedFiles("wt1")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["a.go"]; ok {
		t.Error("a.go should be gone after Unreview")
	}
	if got["b.go"] != "hash-b1" {
		t.Errorf("b.go hash = %q, want %q", got["b.go"], "hash-b1")
	}
}

func TestPersistenceAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, _ := OpenJSON(path)
	s.SetReviewed("wt1", "x.go", "hash-x")
	s.AddComment(model.Comment{
		ID: "c-1", WorktreeID: "wt1", File: "x.go", Line: 10, Side: "new",
		Body: "fix this", Author: "nav", FileHash: "hash-x",
	})

	// Reopen from disk.
	s2, err := OpenJSON(path)
	if err != nil {
		t.Fatal(err)
	}
	rev, _ := s2.ReviewedFiles("wt1")
	if rev["x.go"] != "hash-x" {
		t.Errorf("reviewed state not persisted: %+v", rev)
	}
	cs, _ := s2.Comments("wt1")
	if len(cs) != 1 || cs[0].Body != "fix this" {
		t.Errorf("comment not persisted: %+v", cs)
	}
	if cs[0].ID != "c-1" || cs[0].Side != "new" || cs[0].FileHash != "hash-x" || cs[0].Author != "nav" {
		t.Errorf("new comment fields (id/side/fileHash/author) not persisted: %+v", cs[0])
	}
}

// TestResolveCommentMarksStateResolved and its siblings below pin the P4-
// design.md §1.5 store extension: ResolveComment/DeleteComment address a
// comment by ID (the whole reason ID was added to the shape).
func TestResolveCommentMarksStateResolved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, _ := OpenJSON(path)
	if err := s.AddComment(model.Comment{ID: "c-1", WorktreeID: "wt1", File: "a.go", Body: "fix"}); err != nil {
		t.Fatal(err)
	}

	if err := s.ResolveComment("wt1", "c-1"); err != nil {
		t.Fatal(err)
	}
	cs, _ := s.Comments("wt1")
	if len(cs) != 1 || cs[0].State != "resolved" {
		t.Errorf("comments after resolve = %+v, want state=resolved", cs)
	}
}

func TestResolveCommentUnknownIDReturnsErrCommentNotFound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, _ := OpenJSON(path)
	if err := s.ResolveComment("wt1", "c-nope"); !errors.Is(err, ErrCommentNotFound) {
		t.Errorf("got %v, want ErrCommentNotFound", err)
	}
}

func TestDeleteCommentRemovesIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, _ := OpenJSON(path)
	s.AddComment(model.Comment{ID: "c-1", WorktreeID: "wt1", File: "a.go", Body: "one"})
	s.AddComment(model.Comment{ID: "c-2", WorktreeID: "wt1", File: "a.go", Body: "two"})

	if err := s.DeleteComment("wt1", "c-1"); err != nil {
		t.Fatal(err)
	}
	cs, _ := s.Comments("wt1")
	if len(cs) != 1 || cs[0].ID != "c-2" {
		t.Errorf("comments after delete = %+v, want just c-2 remaining", cs)
	}
}

func TestDeleteCommentUnknownIDReturnsErrCommentNotFound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, _ := OpenJSON(path)
	if err := s.DeleteComment("wt1", "c-nope"); !errors.Is(err, ErrCommentNotFound) {
		t.Errorf("got %v, want ErrCommentNotFound", err)
	}
}

// TestAddCommentEnforcesPerWorktreeCap pins the security-audit LOW-1 fix
// (P4-security.md): AddComment is otherwise unbounded per worktree, and every
// add re-marshals and rewrites the *entire* state file (flush), so an
// unbounded count is both a local-DoS memory sink and O(n^2) write cost.
func TestAddCommentEnforcesPerWorktreeCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, _ := OpenJSON(path)

	for i := 0; i < maxCommentsPerWorktree; i++ {
		c := model.Comment{ID: "c-" + strconv.Itoa(i), WorktreeID: "wt1", File: "a.go", Body: "x"}
		if err := s.AddComment(c); err != nil {
			t.Fatalf("comment %d: unexpected error: %v", i, err)
		}
	}

	if err := s.AddComment(model.Comment{ID: "c-over", WorktreeID: "wt1", File: "a.go", Body: "one too many"}); !errors.Is(err, ErrTooManyComments) {
		t.Errorf("got %v, want ErrTooManyComments once the cap is reached", err)
	}
	cs, err := s.Comments("wt1")
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != maxCommentsPerWorktree {
		t.Errorf("got %d comments, want exactly the cap (%d) — the rejected add must not be persisted", len(cs), maxCommentsPerWorktree)
	}

	// A different worktree must be unaffected by wt1's cap.
	if err := s.AddComment(model.Comment{ID: "c-wt2", WorktreeID: "wt2", File: "z.go", Body: "fine"}); err != nil {
		t.Errorf("a different worktree must not be affected by wt1's cap: %v", err)
	}
}

// TestClearWorktreeAlsoWipesComments pins the design's ClearWorktree
// extension: engine.Approve already calls it on merge, so comments must not
// outlive the worktree they were made on. A second worktree's comments must
// be untouched (same isolation contract ClearWorktree already has for review
// state).
func TestClearWorktreeAlsoWipesComments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, _ := OpenJSON(path)
	s.AddComment(model.Comment{ID: "c-1", WorktreeID: "wt1", File: "a.go", Body: "one"})
	s.AddComment(model.Comment{ID: "c-2", WorktreeID: "wt2", File: "z.go", Body: "two"})

	if err := s.ClearWorktree("wt1"); err != nil {
		t.Fatal(err)
	}
	cs1, _ := s.Comments("wt1")
	if len(cs1) != 0 {
		t.Errorf("wt1 comments should be wiped, got %+v", cs1)
	}
	cs2, _ := s.Comments("wt2")
	if len(cs2) != 1 {
		t.Errorf("wt2 comments must be untouched by clearing wt1, got %+v", cs2)
	}
}

// TestLoadV03StateFileWithoutNewCommentFieldsStillLoads pins the store-shape
// change's own migration story (P4-design.md §1.5): the design's
// justification for changing the shape in place (rather than versioning it)
// is that zero production callers of AddComment/Comments ever shipped, so no
// real state.json in the wild has a "comments" entry missing id/side/
// fileHash. This proves that if one somehow existed anyway (e.g. a v0.3
// pre-release state file), it still loads without error — the new fields
// just default to their Go zero values, same silent-tolerance philosophy as
// TestLoadV01StateDropsOldReviews above.
func TestLoadV03StateFileWithoutNewCommentFieldsStillLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	raw := `{"reviewed_files":{},"comments":{"wt1":[{"worktreeId":"wt1","file":"x.go","line":10,"body":"fix this","state":"open","at":"2026-01-01T00:00:00Z"}]}}`
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := OpenJSON(path)
	if err != nil {
		t.Fatalf("loading a pre-id/side/fileHash comment shape must not error: %v", err)
	}
	cs, err := s.Comments("wt1")
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 || cs[0].Body != "fix this" {
		t.Fatalf("comment not loaded correctly: %+v", cs)
	}
	if cs[0].ID != "" || cs[0].Side != "" || cs[0].FileHash != "" {
		t.Errorf("new fields should default to zero values for an old-shaped entry, got %+v", cs[0])
	}

	// The store must still be fully usable after loading the old shape.
	if err := s.AddComment(model.Comment{ID: "c-new", WorktreeID: "wt1", File: "y.go", Body: "another"}); err != nil {
		t.Fatal(err)
	}
	cs2, _ := s.Comments("wt1")
	if len(cs2) != 2 {
		t.Errorf("store unusable after loading the old shape: %+v", cs2)
	}
}

func TestClearWorktreeResetsReview(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, _ := OpenJSON(path)
	s.SetReviewed("wt1", "a.go", "h1")
	s.SetReviewed("wt2", "z.go", "h2")

	if err := s.ClearWorktree("wt1"); err != nil {
		t.Fatal(err)
	}
	rev, _ := s.ReviewedFiles("wt1")
	if len(rev) != 0 {
		t.Errorf("wt1 review state should be cleared, got %+v", rev)
	}
	// wt2 must be untouched.
	rev2, _ := s.ReviewedFiles("wt2")
	if rev2["z.go"] != "h2" {
		t.Error("clearing wt1 must not affect wt2")
	}
}

// TestLoadV01StateDropsOldReviews pins the whole migration story: a v0.1 state
// file stored a bool under the "reviewed" key. The new store never reads that
// key, so such a file must load without error and simply start with zero
// reviews — old review marks silently drop rather than crashing the daemon.
func TestLoadV01StateDropsOldReviews(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"reviewed": {"wt": {"f": true}}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := OpenJSON(path)
	if err != nil {
		t.Fatalf("loading a v0.1 state file must not error: %v", err)
	}
	rev, err := s.ReviewedFiles("wt")
	if err != nil {
		t.Fatal(err)
	}
	if len(rev) != 0 {
		t.Errorf("old reviews must not carry over, got %+v", rev)
	}

	// The store must still be fully usable after loading the old shape.
	if err := s.SetReviewed("wt", "f", "newhash"); err != nil {
		t.Fatal(err)
	}
	rev, _ = s.ReviewedFiles("wt")
	if rev["f"] != "newhash" {
		t.Errorf("store unusable after loading v0.1 file: %+v", rev)
	}
}
