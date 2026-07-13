// store_test.go now holds only the JSON-loader-specific tests: they feed
// OpenJSON raw pre-shape bytes from disk, which has no SQLite equivalent to
// conform to (SQLite has its own schema-version story, sqlite_test.go). The
// 9 backend-agnostic behaviors that used to live here moved to
// conformance_test.go, run against every Store impl (P6-design.md §1.4).
package store

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/navbytes/wt-cockpit/internal/model"
)

// TestLoadV03StateFileWithoutNewCommentFieldsStillLoads pins the store-shape
// change's own migration story (P4-design.md §1.5): the design's
// justification for changing the shape in place (rather than versioning it)
// is that zero production callers of AddComment/Comments ever shipped, so no
// real state.json in the wild has a "comments" entry missing id/side/
// fileHash. This proves that if one somehow existed anyway (e.g. a v0.3
// pre-release state file), it still loads without error — the new fields
// just default to their Go zero values, same silent-tolerance philosophy as
// TestLoadV01StateDropsOldReviews below.
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
