// sqlite_test.go holds SQLite-only tests: schema/migration mechanics that
// have no JSON equivalent, plus explicit pins for the §1.2 "JSON-impl
// accidents" the SQLite impl must replicate (P6-design.md §1.2, WP1). The
// 9 behaviors both backends share live in conformance_test.go instead.
package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/navbytes/wt-cockpit/internal/model"
)

func openSQLiteT(t *testing.T, path string) *sqliteStore {
	t.Helper()
	s, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	cs := s.(*sqliteStore)
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func userVersion(t *testing.T, s *sqliteStore) int {
	t.Helper()
	var v int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestSQLiteFreshOpenSetsUserVersionTo1(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s := openSQLiteT(t, path)
	if got := userVersion(t, s); got != 1 {
		t.Errorf("user_version after fresh open = %d, want 1", got)
	}
}

// TestSQLiteReopenAfterCloseFullyPersists is the SQLite-specific rigor
// version of the shared conformance reopen test: it explicitly Closes the
// first handle (checkpointing WAL) before reopening, proving real
// close-then-restart persistence rather than just two live handles sharing
// one WAL file.
func TestSQLiteReopenAfterCloseFullyPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetReviewed("wt1", "a.go", "h1"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddComment(model.Comment{ID: "c-1", WorktreeID: "wt1", File: "a.go", Body: "note"}); err != nil {
		t.Fatal(err)
	}
	if err := s.(*sqliteStore).Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s2 := openSQLiteT(t, path)
	rf, err := s2.ReviewedFiles("wt1")
	if err != nil || rf["a.go"] != "h1" {
		t.Errorf("review state not persisted across close+reopen: %+v, err=%v", rf, err)
	}
	cs, err := s2.Comments("wt1")
	if err != nil || len(cs) != 1 || cs[0].ID != "c-1" {
		t.Errorf("comment not persisted across close+reopen: %+v, err=%v", cs, err)
	}
}

// TestSQLiteRefusesNewerSchemaVersion pins §4.1: a db whose user_version
// exceeds what this build's migrations slice supports must be refused with
// a clear error, never silently degraded.
func TestSQLiteRefusesNewerSchemaVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	cs := s.(*sqliteStore)
	if _, err := cs.db.Exec(`PRAGMA user_version = 999`); err != nil {
		t.Fatal(err)
	}
	if err := cs.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = OpenSQLite(path)
	if err == nil {
		t.Fatal("expected an error opening a db with a newer schema version")
	}
	if !strings.Contains(err.Error(), "newer") {
		t.Errorf("error should mention the db being newer, got: %v", err)
	}
}

// TestSQLiteCorruptFileReturnsOpenError pins §4.3's quick_check: a file
// that isn't a valid SQLite database must fail at Open with a loud error.
// Moving the file aside and starting fresh is store.Open's job (WP2), not
// OpenSQLite's — WP1 only needs the error to surface.
func TestSQLiteCorruptFileReturnsOpenError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corrupt.db")
	if err := os.WriteFile(path, []byte("not a sqlite database, just garbage bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenSQLite(path); err == nil {
		t.Fatal("expected an error opening a corrupt db file")
	}
}

// TestSQLiteInsertionOrderSurvivesDeleteThenAdd pins §1.2 #1 combined with
// §3's rowid-reuse note: deleting an earlier (non-max) comment and then
// adding a new one must not disturb the surviving comments' relative order,
// and the new comment must land after them.
func TestSQLiteInsertionOrderSurvivesDeleteThenAdd(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s := openSQLiteT(t, path)

	for _, id := range []string{"c-1", "c-2", "c-3"} {
		if err := s.AddComment(model.Comment{ID: id, WorktreeID: "wt1", File: "a.go", Body: id}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DeleteComment("wt1", "c-1"); err != nil { // delete the earliest, not the last
		t.Fatal(err)
	}
	if err := s.AddComment(model.Comment{ID: "c-4", WorktreeID: "wt1", File: "a.go", Body: "c-4"}); err != nil {
		t.Fatal(err)
	}

	cs, err := s.Comments("wt1")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range cs {
		got = append(got, c.ID)
	}
	want := []string{"c-2", "c-3", "c-4"}
	if len(got) != len(want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("order = %v, want %v", got, want)
			break
		}
	}
}

// TestSQLiteUnreviewAndClearWorktreeOnUnknownAreNilOps pins §1.2 #3: unlike
// ResolveComment/DeleteComment (which return ErrCommentNotFound), Unreview
// and ClearWorktree treat an unknown worktree/file as a successful no-op —
// RowsAffected()==0 is not an error for these two.
func TestSQLiteUnreviewAndClearWorktreeOnUnknownAreNilOps(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s := openSQLiteT(t, path)

	if err := s.Unreview("no-such-wt", "no-such-file.go"); err != nil {
		t.Errorf("Unreview on unknown worktree/file should be a nil-error no-op, got: %v", err)
	}
	if err := s.ClearWorktree("no-such-wt"); err != nil {
		t.Errorf("ClearWorktree on unknown worktree should be a nil-error no-op, got: %v", err)
	}
}

// TestSQLiteReviewedFilesAndCommentsReturnNonNilEmpty pins §1.2 #2 directly
// against the SQLite impl (the conformance suite exercises this
// incidentally; this test asserts it explicitly).
func TestSQLiteReviewedFilesAndCommentsReturnNonNilEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s := openSQLiteT(t, path)

	rf, err := s.ReviewedFiles("no-such-wt")
	if err != nil {
		t.Fatal(err)
	}
	if rf == nil {
		t.Error("ReviewedFiles for an unknown worktree must return a non-nil empty map")
	}
	if len(rf) != 0 {
		t.Errorf("ReviewedFiles for an unknown worktree = %+v, want empty", rf)
	}

	cs, err := s.Comments("no-such-wt")
	if err != nil {
		t.Fatal(err)
	}
	if cs == nil {
		t.Error("Comments for an unknown worktree must return a non-nil empty slice")
	}
	if len(cs) != 0 {
		t.Errorf("Comments for an unknown worktree = %+v, want empty", cs)
	}
}

// TestSQLiteAddCommentDefaultsStateAndAt pins §1.2 #4 directly: a comment
// added with a zero State and zero At must be persisted with State="open"
// and a stamped (non-zero) At — the same defaulting jsonStore.AddComment
// applies before appending.
func TestSQLiteAddCommentDefaultsStateAndAt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s := openSQLiteT(t, path)

	before := time.Now()
	if err := s.AddComment(model.Comment{ID: "c-1", WorktreeID: "wt1", File: "a.go", Body: "note"}); err != nil {
		t.Fatal(err)
	}
	after := time.Now()

	cs, err := s.Comments("wt1")
	if err != nil || len(cs) != 1 {
		t.Fatalf("cs=%+v err=%v", cs, err)
	}
	if cs[0].State != "open" {
		t.Errorf("State = %q, want default %q", cs[0].State, "open")
	}
	if cs[0].At.IsZero() {
		t.Error("At should be stamped, not zero")
	}
	if cs[0].At.Before(before.Add(-time.Second)) || cs[0].At.After(after.Add(time.Second)) {
		t.Errorf("At = %v, want between %v and %v", cs[0].At, before, after)
	}
}

// TestSQLiteCommentTimeRoundTripsWithOriginalOffset pins §1.2 #5: At
// round-trips through storage preserving the original zone offset, exactly
// like JSON's RFC3339Nano marshaling. Per the design, no test compares with
// == (the monotonic-clock reading is already lost across any reopen, same
// as the JSON impl) — wall-clock equality (via RFC3339Nano formatting) is
// the contract.
func TestSQLiteCommentTimeRoundTripsWithOriginalOffset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s := openSQLiteT(t, path)

	loc := time.FixedZone("+05:30", 5*3600+30*60)
	at := time.Date(2026, 3, 1, 10, 30, 0, 123456789, loc)
	if err := s.AddComment(model.Comment{ID: "c-1", WorktreeID: "wt1", File: "a.go", Body: "note", At: at}); err != nil {
		t.Fatal(err)
	}

	cs, err := s.Comments("wt1")
	if err != nil || len(cs) != 1 {
		t.Fatalf("cs=%+v err=%v", cs, err)
	}
	want := at.Format(time.RFC3339Nano)
	got := cs[0].At.Format(time.RFC3339Nano)
	if got != want {
		t.Errorf("At round-trip = %q, want %q (original offset preserved)", got, want)
	}
}

// TestSQLiteResolveAndDeleteStillErrorOnUnknownComment is a belt-and-braces
// companion to the conformance suite's ErrCommentNotFound tests, pinned
// here specifically against RowsAffected()==0 mapping (the mechanism this
// impl uses, distinct from JSON's linear scan) so the distinction from
// Unreview/ClearWorktree's silent no-op above is exercised in the same
// file.
func TestSQLiteResolveAndDeleteStillErrorOnUnknownComment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s := openSQLiteT(t, path)

	if err := s.AddComment(model.Comment{ID: "c-1", WorktreeID: "wt1", File: "a.go", Body: "note"}); err != nil {
		t.Fatal(err)
	}
	if err := s.ResolveComment("wt1", "c-unknown"); !errors.Is(err, ErrCommentNotFound) {
		t.Errorf("ResolveComment(unknown) = %v, want ErrCommentNotFound", err)
	}
	if err := s.ResolveComment("unknown-wt", "c-1"); !errors.Is(err, ErrCommentNotFound) {
		t.Errorf("ResolveComment(known id, wrong worktree) = %v, want ErrCommentNotFound", err)
	}
	if err := s.DeleteComment("wt1", "c-unknown"); !errors.Is(err, ErrCommentNotFound) {
		t.Errorf("DeleteComment(unknown) = %v, want ErrCommentNotFound", err)
	}
}

// TestSQLiteRefusesNewerSchemaVersionMessageMentionsBuildSupport is a
// tighter pin on the refuse-newer message shape than the "contains newer"
// check above, guarding against the message silently losing the versions
// it's meant to report (useful for a user pasting the line into a bug
// report).
func TestSQLiteRefusesNewerSchemaVersionMessageMentionsBuildSupport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	cs := s.(*sqliteStore)
	if _, err := cs.db.Exec(`PRAGMA user_version = 2`); err != nil {
		t.Fatal(err)
	}
	if err := cs.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = OpenSQLite(path)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "schema v2") || !strings.Contains(err.Error(), "supports v1") {
		t.Errorf("error should name both versions, got: %v", err)
	}
}
