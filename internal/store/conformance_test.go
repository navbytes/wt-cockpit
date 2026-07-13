// conformance_test.go is the drop-in proof (P6-design.md §1.4/WP1): the 9
// backend-agnostic behaviors from the original store_test.go, extracted
// once and run against every Store implementation via the backends table
// below. Assertions are unchanged from their original form — only the
// construction line (OpenJSON → a table-driven b.open) differs. The 2
// JSON-loader-specific tests (raw pre-shape JSON bytes) stay in
// store_test.go; they have no SQLite equivalent to conform to.
package store

import (
	"errors"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/navbytes/wt-cockpit/internal/model"
)

// storeBackend names one Store implementation and how to open it at a
// given path — the seam this whole file exists to exercise on both sides
// of.
type storeBackend struct {
	name string
	ext  string
	open func(t *testing.T, path string) Store
}

var storeBackends = []storeBackend{
	{"json", ".json", func(t *testing.T, path string) Store {
		t.Helper()
		s, err := OpenJSON(path)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}},
	{"sqlite", ".db", func(t *testing.T, path string) Store {
		t.Helper()
		s, err := OpenSQLite(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.(*sqliteStore).Close() })
		return s
	}},
}

// statePath returns a fresh path for backend b inside t's temp dir, named
// so a second call in the same test (reopen) can reuse it deliberately by
// capturing the returned path once.
func statePath(t *testing.T, b storeBackend) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "state"+b.ext)
}

func TestReviewedRoundTrip(t *testing.T) {
	for _, b := range storeBackends {
		t.Run(b.name, func(t *testing.T) {
			s := b.open(t, statePath(t, b))
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
		})
	}
}

func TestPersistenceAcrossReopen(t *testing.T) {
	for _, b := range storeBackends {
		t.Run(b.name, func(t *testing.T) {
			path := statePath(t, b)
			s := b.open(t, path)
			s.SetReviewed("wt1", "x.go", "hash-x")
			s.AddComment(model.Comment{
				ID: "c-1", WorktreeID: "wt1", File: "x.go", Line: 10, Side: "new",
				Body: "fix this", Author: "nav", FileHash: "hash-x",
			})

			// Reopen from disk.
			s2 := b.open(t, path)
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
		})
	}
}

// TestResolveCommentMarksStateResolved and its siblings below pin the P4-
// design.md §1.5 store extension: ResolveComment/DeleteComment address a
// comment by ID (the whole reason ID was added to the shape).
func TestResolveCommentMarksStateResolved(t *testing.T) {
	for _, b := range storeBackends {
		t.Run(b.name, func(t *testing.T) {
			s := b.open(t, statePath(t, b))
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
		})
	}
}

func TestResolveCommentUnknownIDReturnsErrCommentNotFound(t *testing.T) {
	for _, b := range storeBackends {
		t.Run(b.name, func(t *testing.T) {
			s := b.open(t, statePath(t, b))
			if err := s.ResolveComment("wt1", "c-nope"); !errors.Is(err, ErrCommentNotFound) {
				t.Errorf("got %v, want ErrCommentNotFound", err)
			}
		})
	}
}

func TestDeleteCommentRemovesIt(t *testing.T) {
	for _, b := range storeBackends {
		t.Run(b.name, func(t *testing.T) {
			s := b.open(t, statePath(t, b))
			s.AddComment(model.Comment{ID: "c-1", WorktreeID: "wt1", File: "a.go", Body: "one"})
			s.AddComment(model.Comment{ID: "c-2", WorktreeID: "wt1", File: "a.go", Body: "two"})

			if err := s.DeleteComment("wt1", "c-1"); err != nil {
				t.Fatal(err)
			}
			cs, _ := s.Comments("wt1")
			if len(cs) != 1 || cs[0].ID != "c-2" {
				t.Errorf("comments after delete = %+v, want just c-2 remaining", cs)
			}
		})
	}
}

func TestDeleteCommentUnknownIDReturnsErrCommentNotFound(t *testing.T) {
	for _, b := range storeBackends {
		t.Run(b.name, func(t *testing.T) {
			s := b.open(t, statePath(t, b))
			if err := s.DeleteComment("wt1", "c-nope"); !errors.Is(err, ErrCommentNotFound) {
				t.Errorf("got %v, want ErrCommentNotFound", err)
			}
		})
	}
}

// TestAddCommentEnforcesPerWorktreeCap pins the security-audit LOW-1 fix
// (P4-security.md): AddComment is otherwise unbounded per worktree. The
// JSON impl enforces this in Go before appending; the SQLite impl must not
// regress on it just because a database could otherwise hold the rows
// cheaply — matching behavior, not matching implementation technique, is
// the point of the drop-in.
func TestAddCommentEnforcesPerWorktreeCap(t *testing.T) {
	for _, b := range storeBackends {
		t.Run(b.name, func(t *testing.T) {
			s := b.open(t, statePath(t, b))

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
		})
	}
}

// TestClearWorktreeAlsoWipesComments pins the design's ClearWorktree
// extension: engine.Approve already calls it on merge, so comments must not
// outlive the worktree they were made on. A second worktree's comments must
// be untouched (same isolation contract ClearWorktree already has for review
// state).
func TestClearWorktreeAlsoWipesComments(t *testing.T) {
	for _, b := range storeBackends {
		t.Run(b.name, func(t *testing.T) {
			s := b.open(t, statePath(t, b))
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
		})
	}
}

func TestClearWorktreeResetsReview(t *testing.T) {
	for _, b := range storeBackends {
		t.Run(b.name, func(t *testing.T) {
			s := b.open(t, statePath(t, b))
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
		})
	}
}
