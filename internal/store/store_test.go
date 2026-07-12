package store

import (
	"path/filepath"
	"testing"
)

func TestReviewedRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := OpenJSON(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetReviewed("wt1", "a.go", true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetReviewed("wt1", "b.go", true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetReviewed("wt1", "a.go", false); err != nil {
		t.Fatal(err)
	}

	got, err := s.ReviewedFiles("wt1")
	if err != nil {
		t.Fatal(err)
	}
	if got["a.go"] {
		t.Error("a.go should be un-reviewed after toggle off")
	}
	if !got["b.go"] {
		t.Error("b.go should be reviewed")
	}
}

func TestPersistenceAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, _ := OpenJSON(path)
	s.SetReviewed("wt1", "x.go", true)
	s.AddComment(Comment{WorktreeID: "wt1", File: "x.go", Line: 10, Body: "fix this"})

	// Reopen from disk.
	s2, err := OpenJSON(path)
	if err != nil {
		t.Fatal(err)
	}
	rev, _ := s2.ReviewedFiles("wt1")
	if !rev["x.go"] {
		t.Error("reviewed state not persisted")
	}
	cs, _ := s2.Comments("wt1")
	if len(cs) != 1 || cs[0].Body != "fix this" {
		t.Errorf("comment not persisted: %+v", cs)
	}
}

func TestClearWorktreeResetsReview(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, _ := OpenJSON(path)
	s.SetReviewed("wt1", "a.go", true)
	s.SetReviewed("wt2", "z.go", true)

	if err := s.ClearWorktree("wt1"); err != nil {
		t.Fatal(err)
	}
	rev, _ := s.ReviewedFiles("wt1")
	if len(rev) != 0 {
		t.Errorf("wt1 review state should be cleared, got %+v", rev)
	}
	// wt2 must be untouched.
	rev2, _ := s.ReviewedFiles("wt2")
	if !rev2["z.go"] {
		t.Error("clearing wt1 must not affect wt2")
	}
}

func TestReviewedCount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, _ := OpenJSON(path)
	s.SetReviewed("wt1", "a.go", true)
	s.SetReviewed("wt1", "b.go", true)
	if n := s.ReviewedCount("wt1"); n != 2 {
		t.Errorf("count = %d, want 2", n)
	}
}
