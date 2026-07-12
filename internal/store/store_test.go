package store

import (
	"os"
	"path/filepath"
	"testing"
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
	s.AddComment(Comment{WorktreeID: "wt1", File: "x.go", Line: 10, Body: "fix this"})

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
