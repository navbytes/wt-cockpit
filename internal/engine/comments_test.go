package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/navbytes/wt-cockpit/internal/model"
)

// findCommentView looks up a CommentView by ID within a slice returned by
// Engine.Comments — the comment-level counterpart to findByBranch/findFile
// above.
func findCommentView(views []model.CommentView, id string) *model.CommentView {
	for i := range views {
		if views[i].ID == id {
			return &views[i]
		}
	}
	return nil
}

// assertCommentChangedCount drains everything currently buffered on ch and
// asserts exactly want comment.changed events are present — pins WP1's
// "every mutation publishes comment.changed exactly once" contract. Safe to
// call without any goroutine/timing concerns: every mutation under test runs
// synchronously in the calling goroutine, so by the time it returns, its
// Publish call has already landed in the buffered channel.
func assertCommentChangedCount(t *testing.T, ch <-chan model.Event, want int) {
	t.Helper()
	var got int
loop:
	for {
		select {
		case ev := <-ch:
			if ev.Type == model.EventCommentChanged {
				got++
			}
		default:
			break loop
		}
	}
	if got != want {
		t.Errorf("comment.changed events = %d, want %d", got, want)
	}
}

// ---- AddComment: identity, defaults, anchoring ----

func TestAddCommentStampsIdentityAndFileHash(t *testing.T) {
	root := buildWorkspace(t)
	e := newEngine(t, root)
	e.Refresh(context.Background())
	feat := findByBranch(e.List(), "feature")

	d, _ := e.Diff(feat.ID)
	newFile := findFile(d.Files, "new.go")
	if newFile == nil {
		t.Fatal("precondition: new.go must be in the diff")
	}

	c, err := e.AddComment(feat.ID, "new.go", 1, "", "looks good", "")
	if err != nil {
		t.Fatal(err)
	}
	if c.ID == "" || !strings.HasPrefix(c.ID, "c-") {
		t.Errorf("ID = %q, want a non-empty c-prefixed id", c.ID)
	}
	if c.FileHash != newFile.Hash {
		t.Errorf("FileHash = %q, want the file's current hash %q", c.FileHash, newFile.Hash)
	}
	if c.Side != "new" {
		t.Errorf("Side = %q, want default \"new\"", c.Side)
	}
	if c.State != "open" {
		t.Errorf("State = %q, want \"open\"", c.State)
	}
	if c.Author == "" {
		t.Error("Author should default to the OS user, not be empty")
	}
	if c.At.IsZero() {
		t.Error("At should be stamped")
	}
	if c.WorktreeID != feat.ID {
		t.Errorf("WorktreeID = %q, want %q", c.WorktreeID, feat.ID)
	}
}

func TestAddCommentRespectsExplicitAuthor(t *testing.T) {
	root := buildWorkspace(t)
	e := newEngine(t, root)
	e.Refresh(context.Background())
	feat := findByBranch(e.List(), "feature")

	c, err := e.AddComment(feat.ID, "new.go", 0, "", "who wrote this", "naveen")
	if err != nil {
		t.Fatal(err)
	}
	if c.Author != "naveen" {
		t.Errorf("Author = %q, want the explicit value naveen", c.Author)
	}
}

func TestAddCommentAllowsLineZeroFileLevel(t *testing.T) {
	root := buildWorkspace(t)
	e := newEngine(t, root)
	e.Refresh(context.Background())
	feat := findByBranch(e.List(), "feature")

	c, err := e.AddComment(feat.ID, "new.go", 0, "", "file-level note", "")
	if err != nil {
		t.Fatal(err)
	}
	if c.Line != 0 {
		t.Errorf("Line = %d, want 0", c.Line)
	}
}

func TestAddCommentAllowsOldSide(t *testing.T) {
	root := buildWorkspace(t)
	e := newEngine(t, root)
	e.Refresh(context.Background())
	feat := findByBranch(e.List(), "feature")

	c, err := e.AddComment(feat.ID, "new.go", 3, "old", "was this deleted?", "")
	if err != nil {
		t.Fatal(err)
	}
	if c.Side != "old" {
		t.Errorf("Side = %q, want old", c.Side)
	}
}

func TestAddCommentRejectsNegativeLine(t *testing.T) {
	root := buildWorkspace(t)
	e := newEngine(t, root)
	e.Refresh(context.Background())
	feat := findByBranch(e.List(), "feature")

	if _, err := e.AddComment(feat.ID, "new.go", -1, "", "bad line", ""); !errors.Is(err, ErrInvalidComment) {
		t.Errorf("expected ErrInvalidComment for a negative line, got %v", err)
	}
}

func TestAddCommentRejectsBadSide(t *testing.T) {
	root := buildWorkspace(t)
	e := newEngine(t, root)
	e.Refresh(context.Background())
	feat := findByBranch(e.List(), "feature")

	if _, err := e.AddComment(feat.ID, "new.go", 1, "sideways", "bad side", ""); !errors.Is(err, ErrInvalidComment) {
		t.Errorf("expected ErrInvalidComment for an invalid side, got %v", err)
	}
}

func TestAddCommentRejectsEmptyBody(t *testing.T) {
	root := buildWorkspace(t)
	e := newEngine(t, root)
	e.Refresh(context.Background())
	feat := findByBranch(e.List(), "feature")

	if _, err := e.AddComment(feat.ID, "new.go", 1, "", "", ""); !errors.Is(err, ErrInvalidComment) {
		t.Errorf("expected ErrInvalidComment for an empty body, got %v", err)
	}
}

func TestAddCommentRejectsInvalidUTF8Body(t *testing.T) {
	root := buildWorkspace(t)
	e := newEngine(t, root)
	e.Refresh(context.Background())
	feat := findByBranch(e.List(), "feature")

	if _, err := e.AddComment(feat.ID, "new.go", 1, "", "bad \xff\xfe body", ""); !errors.Is(err, ErrInvalidComment) {
		t.Errorf("expected ErrInvalidComment for invalid UTF-8, got %v", err)
	}
}

func TestAddCommentRejectsBodyOverSizeLimit(t *testing.T) {
	root := buildWorkspace(t)
	e := newEngine(t, root)
	e.Refresh(context.Background())
	feat := findByBranch(e.List(), "feature")

	big := strings.Repeat("a", maxCommentBodyBytes+1)
	if _, err := e.AddComment(feat.ID, "new.go", 1, "", big, ""); !errors.Is(err, ErrCommentTooLarge) {
		t.Errorf("expected ErrCommentTooLarge for a %d-byte body, got %v", len(big), err)
	}
}

func TestAddCommentAcceptsBodyExactlyAtSizeLimit(t *testing.T) {
	root := buildWorkspace(t)
	e := newEngine(t, root)
	e.Refresh(context.Background())
	feat := findByBranch(e.List(), "feature")

	exact := strings.Repeat("a", maxCommentBodyBytes)
	if _, err := e.AddComment(feat.ID, "new.go", 1, "", exact, ""); err != nil {
		t.Errorf("a body exactly at the %d-byte limit should be accepted, got %v", maxCommentBodyBytes, err)
	}
}

// TestAddCommentRejectsUnknownWorktree pins the distinct ErrWorktreeNotFound
// sentinel (P7-ux.md backlog): an unknown worktree id used to share
// ErrFileNotFound's "file not found in current diff" text with an
// out-of-diff file, a confusing 404 body for a request that never named a
// real worktree at all.
func TestAddCommentRejectsUnknownWorktree(t *testing.T) {
	e := newEngine(t, t.TempDir())
	if _, err := e.AddComment("no-such-id", "new.go", 1, "", "hi", ""); !errors.Is(err, ErrWorktreeNotFound) {
		t.Errorf("expected ErrWorktreeNotFound for an unknown worktree, got %v", err)
	}
}

// ---- AddComment: body/author control-byte sanitization + author validation
// (P7 security MEDIUM-1/LOW-2) ----

// TestAddCommentSanitizesControlBytesInBodyAndAuthor is the security fix's
// headline pin: raw ESC/OSC bytes in either field must never reach storage —
// they come back out as their caret-notation equivalent, matching
// diffparse.SanitizeControl's own contract, so `wt comments` (which prints
// these fields verbatim) can never emit a raw escape sequence.
func TestAddCommentSanitizesControlBytesInBodyAndAuthor(t *testing.T) {
	root := buildWorkspace(t)
	e := newEngine(t, root)
	e.Refresh(context.Background())
	feat := findByBranch(e.List(), "feature")

	c, err := e.AddComment(feat.ID, "new.go", 1, "", "evil\x1b]0;pwned\x07 body", "evil\x1bauthor")
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(c.Body, "\x1b\x07") || strings.ContainsAny(c.Author, "\x1b\x07") {
		t.Fatalf("raw ESC/BEL bytes leaked into stored comment: %+v", c)
	}
	if c.Body != "evil^[]0;pwned^G body" {
		t.Errorf("Body = %q, want the caret-sanitized text", c.Body)
	}
	if c.Author != "evil^[author" {
		t.Errorf("Author = %q, want the caret-sanitized text", c.Author)
	}

	// Comments() (what `wt comments`/GET /api/comments actually serve) must
	// read back the same sanitized text, not the raw original.
	views, err := e.Comments(feat.ID)
	if err != nil {
		t.Fatal(err)
	}
	v := findCommentView(views, c.ID)
	if v == nil {
		t.Fatal("comment missing from Comments() immediately after AddComment")
	}
	if strings.ContainsAny(v.Body, "\x1b\x07") || strings.ContainsAny(v.Author, "\x1b\x07") {
		t.Errorf("raw ESC/BEL bytes leaked into Comments() read-back: %+v", v)
	}
}

func TestAddCommentRejectsOverlongAuthor(t *testing.T) {
	root := buildWorkspace(t)
	e := newEngine(t, root)
	e.Refresh(context.Background())
	feat := findByBranch(e.List(), "feature")

	big := strings.Repeat("a", maxCommentAuthorBytes+1)
	if _, err := e.AddComment(feat.ID, "new.go", 1, "", "hi", big); !errors.Is(err, ErrInvalidComment) {
		t.Errorf("expected ErrInvalidComment for a %d-byte author, got %v", len(big), err)
	}
}

func TestAddCommentAcceptsAuthorExactlyAtSizeLimit(t *testing.T) {
	root := buildWorkspace(t)
	e := newEngine(t, root)
	e.Refresh(context.Background())
	feat := findByBranch(e.List(), "feature")

	exact := strings.Repeat("a", maxCommentAuthorBytes)
	if _, err := e.AddComment(feat.ID, "new.go", 1, "", "hi", exact); err != nil {
		t.Errorf("an author exactly at the %d-byte limit should be accepted, got %v", maxCommentAuthorBytes, err)
	}
}

func TestAddCommentRejectsInvalidUTF8Author(t *testing.T) {
	root := buildWorkspace(t)
	e := newEngine(t, root)
	e.Refresh(context.Background())
	feat := findByBranch(e.List(), "feature")

	if _, err := e.AddComment(feat.ID, "new.go", 1, "", "hi", "bad \xff\xfe author"); !errors.Is(err, ErrInvalidComment) {
		t.Errorf("expected ErrInvalidComment for invalid UTF-8 author, got %v", err)
	}
}

func TestAddCommentRejectsFileNotInDiff(t *testing.T) {
	root := buildWorkspace(t)
	e := newEngine(t, root)
	e.Refresh(context.Background())
	feat := findByBranch(e.List(), "feature")

	if _, err := e.AddComment(feat.ID, "does-not-exist.go", 1, "", "hi", ""); !errors.Is(err, ErrFileNotFound) {
		t.Errorf("expected ErrFileNotFound for a file outside the diff, got %v", err)
	}
}

func TestAddCommentPublishesCommentChangedExactlyOnce(t *testing.T) {
	root := buildWorkspace(t)
	e := newEngine(t, root)
	e.Refresh(context.Background())
	feat := findByBranch(e.List(), "feature")

	ch, cancel := e.Registry().Subscribe(8)
	defer cancel()

	if _, err := e.AddComment(feat.ID, "new.go", 1, "", "hi", ""); err != nil {
		t.Fatal(err)
	}
	assertCommentChangedCount(t, ch, 1)
}

// A rejected AddComment must not publish anything at all.
func TestAddCommentRejectedValidationDoesNotPublish(t *testing.T) {
	root := buildWorkspace(t)
	e := newEngine(t, root)
	e.Refresh(context.Background())
	feat := findByBranch(e.List(), "feature")

	ch, cancel := e.Registry().Subscribe(8)
	defer cancel()

	if _, err := e.AddComment(feat.ID, "new.go", 1, "", "", ""); err == nil {
		t.Fatal("expected the empty body to be rejected")
	}
	assertCommentChangedCount(t, ch, 0)
}

// ---- Comments: listing, stale/orphaned anchor computation ----

func TestCommentsUnknownWorktreeReturnsError(t *testing.T) {
	e := newEngine(t, t.TempDir())
	if _, err := e.Comments("no-such-id"); !errors.Is(err, ErrWorktreeNotFound) {
		t.Errorf("expected ErrWorktreeNotFound for an unknown worktree, got %v", err)
	}
}

func TestCommentsReturnsEmptySliceNotNilForAWorktreeWithNoComments(t *testing.T) {
	root := buildWorkspace(t)
	e := newEngine(t, root)
	e.Refresh(context.Background())
	feat := findByBranch(e.List(), "feature")

	got, err := e.Comments(feat.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Error("Comments() = nil, want a non-nil empty slice (json encodes nil as null, not [])")
	}
	if len(got) != 0 {
		t.Errorf("Comments() = %+v, want empty", got)
	}
}

func TestCommentsFreshlyAddedReadsNotStaleNotOrphaned(t *testing.T) {
	root := buildWorkspace(t)
	e := newEngine(t, root)
	e.Refresh(context.Background())
	feat := findByBranch(e.List(), "feature")

	c, err := e.AddComment(feat.ID, "new.go", 1, "", "note", "")
	if err != nil {
		t.Fatal(err)
	}
	views, err := e.Comments(feat.ID)
	if err != nil {
		t.Fatal(err)
	}
	v := findCommentView(views, c.ID)
	if v == nil {
		t.Fatal("comment missing from Comments() immediately after AddComment")
	}
	if v.Stale || v.Orphaned {
		t.Errorf("a freshly added comment should read stale=false orphaned=false, got %+v", v)
	}
}

// TestCommentGoesStaleWhenItsFileIsEditedAfterward is the headline anchor-
// staleness case (P4-design.md §1.5): a comment anchored to new.go's hash at
// creation time must flip Stale=true once the file's content (and so its
// diff hash) moves, without the stored comment itself ever being mutated.
func TestCommentGoesStaleWhenItsFileIsEditedAfterward(t *testing.T) {
	root := buildWorkspace(t)
	e := newEngine(t, root)
	e.Refresh(context.Background())
	feat := findByBranch(e.List(), "feature")

	c, err := e.AddComment(feat.ID, "new.go", 1, "", "please rename this", "")
	if err != nil {
		t.Fatal(err)
	}

	wt := filepath.Join(root, "api-server-feature")
	if err := os.WriteFile(filepath.Join(wt, "new.go"), []byte("package api\n\nfunc C() {}\nfunc D() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	views, err := e.Comments(feat.ID)
	if err != nil {
		t.Fatal(err)
	}
	v := findCommentView(views, c.ID)
	if v == nil {
		t.Fatal("comment missing after editing its file")
	}
	if !v.Stale {
		t.Errorf("Stale = false after editing new.go, want true")
	}
	if v.Orphaned {
		t.Errorf("Orphaned = true, want false (the file is still in the diff, just edited)")
	}
	if v.FileHash != c.FileHash {
		t.Errorf("the stored FileHash must never change on its own, got %q want %q", v.FileHash, c.FileHash)
	}
}

// TestCommentStaleFlagClearsAgainWhenFileRevertsToOriginalContent is the
// content-addressed-hash half of the same property: reverting the file to
// the exact bytes it had when the comment was made must reproduce the
// identical per-file hash, so Stale must read false again.
func TestCommentStaleFlagClearsAgainWhenFileRevertsToOriginalContent(t *testing.T) {
	root := buildWorkspace(t)
	e := newEngine(t, root)
	e.Refresh(context.Background())
	feat := findByBranch(e.List(), "feature")
	wt := filepath.Join(root, "api-server-feature")

	original, err := os.ReadFile(filepath.Join(wt, "new.go"))
	if err != nil {
		t.Fatal(err)
	}

	c, err := e.AddComment(feat.ID, "new.go", 1, "", "note", "")
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(wt, "new.go"), []byte("package api\n\nfunc C() {}\nfunc D() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	views, _ := e.Comments(feat.ID)
	if v := findCommentView(views, c.ID); v == nil || !v.Stale {
		t.Fatalf("precondition: comment should be stale after the edit, got %+v", v)
	}

	if err := os.WriteFile(filepath.Join(wt, "new.go"), original, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	views2, _ := e.Comments(feat.ID)
	v2 := findCommentView(views2, c.ID)
	if v2 == nil {
		t.Fatal("comment missing after reverting the file")
	}
	if v2.Stale {
		t.Errorf("Stale = true after reverting to the exact original content, want false")
	}
}

// TestCommentGoesOrphanedWhenItsFileLeavesTheDiff is the anchor-orphan case:
// removing the (untracked) commented file entirely drops it out of the diff,
// and the comment must survive (never auto-deleted) with Orphaned=true.
func TestCommentGoesOrphanedWhenItsFileLeavesTheDiff(t *testing.T) {
	root := buildWorkspace(t)
	e := newEngine(t, root)
	e.Refresh(context.Background())
	feat := findByBranch(e.List(), "feature")
	wt := filepath.Join(root, "api-server-feature")

	c, err := e.AddComment(feat.ID, "new.go", 1, "", "note", "")
	if err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(filepath.Join(wt, "new.go")); err != nil {
		t.Fatal(err)
	}
	if err := e.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	views, err := e.Comments(feat.ID)
	if err != nil {
		t.Fatal(err)
	}
	v := findCommentView(views, c.ID)
	if v == nil {
		t.Fatal("comment must still be listed even once its file leaves the diff (never auto-deleted)")
	}
	if !v.Orphaned {
		t.Errorf("Orphaned = false after removing new.go entirely, want true")
	}
	if v.Stale {
		t.Errorf("Stale = true for an orphaned comment, want false (no current hash to compare against)")
	}
}

// ---- ResolveComment / DeleteComment ----

func TestResolveCommentMarksStateResolved(t *testing.T) {
	root := buildWorkspace(t)
	e := newEngine(t, root)
	e.Refresh(context.Background())
	feat := findByBranch(e.List(), "feature")

	c, err := e.AddComment(feat.ID, "new.go", 1, "", "note", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.ResolveComment(feat.ID, c.ID); err != nil {
		t.Fatal(err)
	}
	views, _ := e.Comments(feat.ID)
	v := findCommentView(views, c.ID)
	if v == nil || v.State != "resolved" {
		t.Errorf("comment after resolve = %+v, want state=resolved", v)
	}
}

func TestResolveCommentUnknownIDReturnsErrCommentNotFound(t *testing.T) {
	root := buildWorkspace(t)
	e := newEngine(t, root)
	e.Refresh(context.Background())
	feat := findByBranch(e.List(), "feature")

	if err := e.ResolveComment(feat.ID, "c-doesnotexist"); !errors.Is(err, ErrCommentNotFound) {
		t.Errorf("expected ErrCommentNotFound, got %v", err)
	}
}

func TestResolveCommentUnknownWorktreeReturnsErrFileNotFound(t *testing.T) {
	e := newEngine(t, t.TempDir())
	if err := e.ResolveComment("no-such-id", "c-anything"); !errors.Is(err, ErrFileNotFound) {
		t.Errorf("expected ErrFileNotFound, got %v", err)
	}
}

func TestDeleteCommentRemovesItEntirely(t *testing.T) {
	root := buildWorkspace(t)
	e := newEngine(t, root)
	e.Refresh(context.Background())
	feat := findByBranch(e.List(), "feature")

	c, err := e.AddComment(feat.ID, "new.go", 1, "", "note", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.DeleteComment(feat.ID, c.ID); err != nil {
		t.Fatal(err)
	}
	views, _ := e.Comments(feat.ID)
	if v := findCommentView(views, c.ID); v != nil {
		t.Errorf("comment should be gone after delete, got %+v", v)
	}
}

func TestDeleteCommentUnknownIDReturnsErrCommentNotFound(t *testing.T) {
	root := buildWorkspace(t)
	e := newEngine(t, root)
	e.Refresh(context.Background())
	feat := findByBranch(e.List(), "feature")

	if err := e.DeleteComment(feat.ID, "c-doesnotexist"); !errors.Is(err, ErrCommentNotFound) {
		t.Errorf("expected ErrCommentNotFound, got %v", err)
	}
}

func TestResolveCommentPublishesCommentChangedExactlyOnce(t *testing.T) {
	root := buildWorkspace(t)
	e := newEngine(t, root)
	e.Refresh(context.Background())
	feat := findByBranch(e.List(), "feature")
	c, err := e.AddComment(feat.ID, "new.go", 1, "", "note", "")
	if err != nil {
		t.Fatal(err)
	}

	ch, cancel := e.Registry().Subscribe(8)
	defer cancel()
	if err := e.ResolveComment(feat.ID, c.ID); err != nil {
		t.Fatal(err)
	}
	assertCommentChangedCount(t, ch, 1)
}

func TestDeleteCommentPublishesCommentChangedExactlyOnce(t *testing.T) {
	root := buildWorkspace(t)
	e := newEngine(t, root)
	e.Refresh(context.Background())
	feat := findByBranch(e.List(), "feature")
	c, err := e.AddComment(feat.ID, "new.go", 1, "", "note", "")
	if err != nil {
		t.Fatal(err)
	}

	ch, cancel := e.Registry().Subscribe(8)
	defer cancel()
	if err := e.DeleteComment(feat.ID, c.ID); err != nil {
		t.Fatal(err)
	}
	assertCommentChangedCount(t, ch, 1)
}

// ---- Approve clears comments ----

// TestApproveClearsComments proves the design's ClearWorktree extension end
// to end at the engine level: a successful Approve wipes the worktree's
// comments along with its review state, so comments never outlive the
// worktree they were made on.
func TestApproveClearsComments(t *testing.T) {
	root := buildWorkspace(t)
	commitWorktree(t, root)
	e := newEngine(t, root)
	e.Refresh(context.Background())
	feat := findByBranch(e.List(), "feature")

	if _, err := e.AddComment(feat.ID, "new.go", 1, "", "note", ""); err != nil {
		t.Fatal(err)
	}
	d, _ := e.Diff(feat.ID)
	for _, f := range d.Files {
		if err := e.SetReviewed(feat.ID, f.Path, true, f.Hash); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := e.Approve(feat.ID); err != nil {
		t.Fatalf("approve: %v", err)
	}

	cs, err := e.st.Comments(feat.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 0 {
		t.Errorf("comments after approve = %+v, want none (ClearWorktree must wipe them)", cs)
	}
}
