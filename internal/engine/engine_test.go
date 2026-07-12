package engine

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/navbytes/wt-cockpit/internal/gitbackend"
	"github.com/navbytes/wt-cockpit/internal/guardrail"
	"github.com/navbytes/wt-cockpit/internal/model"
	"github.com/navbytes/wt-cockpit/internal/registry"
	"github.com/navbytes/wt-cockpit/internal/store"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// buildWorkspace makes a ~/code-like root with one repo that has a feature worktree
// containing a modification, an addition, and a large migration deletion.
func buildWorkspace(t *testing.T) (root string) {
	root = t.TempDir()
	repo := filepath.Join(root, "api-server")
	os.MkdirAll(filepath.Join(repo, "migrations"), 0o755)
	git(t, repo, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module api\n"), 0o644)
	os.WriteFile(filepath.Join(repo, "app.go"), []byte("package api\n\nfunc A() {}\n"), 0o644)
	big := "package m\n"
	for i := 0; i < 90; i++ {
		big += "// legacy line\n"
	}
	os.WriteFile(filepath.Join(repo, "migrations", "001.sql"), []byte(big), 0o644)
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-q", "-m", "init")

	wt := filepath.Join(root, "api-server-feature")
	git(t, repo, "worktree", "add", "-q", "-b", "feature", wt)
	// modify app.go (uncommitted), add new file, gut the migration file
	os.WriteFile(filepath.Join(wt, "app.go"), []byte("package api\n\nfunc A() {}\nfunc B() {}\n"), 0o644)
	os.WriteFile(filepath.Join(wt, "new.go"), []byte("package api\n\nfunc C() {}\n"), 0o644)
	os.WriteFile(filepath.Join(wt, "migrations", "001.sql"), []byte("package m\n// kept\n"), 0o644)
	return root
}

func newEngine(t *testing.T, root string) *Engine {
	t.Helper()
	reg := registry.New()
	st, err := store.OpenJSON(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	be := gitbackend.NewCLIWithEnv(testGitEnv())
	gr := guardrail.New(guardrail.DefaultRules())
	return New(Config{
		Roots:          []string{root},
		MaxDepth:       4,
		ActivityWindow: 30 * time.Second,
	}, be, reg, st, gr)
}

func findByBranch(list []model.Worktree, branch string) *model.Worktree {
	for i := range list {
		if list[i].Branch == branch {
			return &list[i]
		}
	}
	return nil
}

func findFile(files []model.DiffFile, path string) *model.DiffFile {
	for i := range files {
		if files[i].Path == path {
			return &files[i]
		}
	}
	return nil
}

func TestRefreshPopulatesWorktreeWithStatsAndGuardrails(t *testing.T) {
	root := buildWorkspace(t)
	e := newEngine(t, root)
	if err := e.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	feat := findByBranch(e.List(), "feature")
	if feat == nil {
		t.Fatalf("feature worktree not tracked; got %+v", e.List())
	}
	if feat.Repo != "api-server" {
		t.Errorf("repo = %q", feat.Repo)
	}
	// Should reflect committed+uncommitted: func B (add), new.go (add), migration deletions.
	if feat.Stats.Add < 2 {
		t.Errorf("adds = %d, want >=2", feat.Stats.Add)
	}
	if feat.Stats.Del < 80 {
		t.Errorf("dels = %d, want the big migration deletion (>=80)", feat.Stats.Del)
	}
	// Guardrails: migrations touched + large deletion should trip.
	if len(feat.Guardrails) == 0 {
		t.Errorf("expected guardrail hits for migrations/large-deletion, got none")
	}
	var sawMigration bool
	for _, h := range feat.Guardrails {
		if h.Rule == "touches-migrations" || h.Rule == "touches-migrations-root" {
			sawMigration = true
		}
	}
	if !sawMigration {
		t.Errorf("expected a migrations guardrail, got %+v", feat.Guardrails)
	}
	if feat.State != model.StateActive {
		t.Errorf("freshly-changed worktree should be active, got %q", feat.State)
	}
}

func TestDiffReturnsStructuredFiles(t *testing.T) {
	root := buildWorkspace(t)
	e := newEngine(t, root)
	e.Refresh(context.Background())
	feat := findByBranch(e.List(), "feature")

	d, ok := e.Diff(feat.ID)
	if !ok {
		t.Fatal("diff not found")
	}
	if len(d.Files) < 2 {
		t.Fatalf("want >=2 changed files, got %d", len(d.Files))
	}
	var sawNew bool
	for _, f := range d.Files {
		if f.Path == "new.go" && f.Status == model.FileAdded {
			sawNew = true
			if f.Hash == "" {
				t.Error("new.go should have a non-empty per-file hash")
			}
		}
	}
	if !sawNew {
		t.Errorf("expected new.go as an added file: %+v", d.Files)
	}
}

func TestSetReviewedUpdatesCount(t *testing.T) {
	root := buildWorkspace(t)
	e := newEngine(t, root)
	e.Refresh(context.Background())
	feat := findByBranch(e.List(), "feature")

	if err := e.SetReviewed(feat.ID, "new.go", true, ""); err != nil {
		t.Fatal(err)
	}
	got := findByBranch(e.List(), "feature")
	if got.Reviewed != 1 {
		t.Errorf("reviewed count = %d, want 1", got.Reviewed)
	}
}

func TestRefreshDetectsRemovedWorktree(t *testing.T) {
	root := buildWorkspace(t)
	e := newEngine(t, root)
	e.Refresh(context.Background())
	feat := findByBranch(e.List(), "feature")
	if feat == nil {
		t.Fatal("precondition: feature must exist")
	}

	// Remove the worktree on disk, then refresh.
	repo := filepath.Join(root, "api-server")
	git(t, repo, "worktree", "remove", "--force", filepath.Join(root, "api-server-feature"))
	if err := e.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if findByBranch(e.List(), "feature") != nil {
		t.Errorf("removed worktree should be gone from the registry")
	}
}

// TestRefreshOneUpdatesOnlyTargetWorktree is the targeted-refresh contract the
// fsnotify watcher relies on: a path hint must re-diff only that worktree,
// leaving every other worktree's cached state untouched.
func TestRefreshOneUpdatesOnlyTargetWorktree(t *testing.T) {
	root := buildWorkspace(t)
	e := newEngine(t, root)
	if err := e.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	feat := findByBranch(e.List(), "feature")
	main := findByBranch(e.List(), "main")
	if feat == nil || main == nil {
		t.Fatalf("precondition: need both main and feature worktrees, got %+v", e.List())
	}

	// Edit the feature worktree further so its diff (and hash) must change.
	wt := filepath.Join(root, "api-server-feature")
	os.WriteFile(filepath.Join(wt, "app.go"), []byte("package api\n\nfunc A() {}\nfunc B() {}\nfunc D() {}\n"), 0o644)

	if err := e.RefreshOne(context.Background(), wt); err != nil {
		t.Fatal(err)
	}

	feat2 := findByBranch(e.List(), "feature")
	main2 := findByBranch(e.List(), "main")
	if feat2.DiffHash == feat.DiffHash {
		t.Errorf("RefreshOne did not pick up the edit: diff hash unchanged (%s)", feat2.DiffHash)
	}
	if !main2.LastChange.Equal(main.LastChange) {
		t.Errorf("RefreshOne touched the unrelated main worktree: lastChange %v -> %v", main.LastChange, main2.LastChange)
	}
	if main2.DiffHash != main.DiffHash {
		t.Errorf("RefreshOne touched the unrelated main worktree's diff hash: %s -> %s", main.DiffHash, main2.DiffHash)
	}
}

// TestRefreshOneFallsBackForUnknownPath covers the "unknown path" branch: if
// the engine has no cached worktree for the given path (e.g. Refresh has never
// run yet), RefreshOne must fall back to a full Refresh rather than silently
// doing nothing.
func TestRefreshOneFallsBackForUnknownPath(t *testing.T) {
	root := buildWorkspace(t)
	e := newEngine(t, root)

	wt := filepath.Join(root, "api-server-feature")
	if err := e.RefreshOne(context.Background(), wt); err != nil {
		t.Fatal(err)
	}

	if findByBranch(e.List(), "feature") == nil {
		t.Errorf("RefreshOne on an unknown path should fall back to a full Refresh, got %+v", e.List())
	}
}

// TestReviewSurvivesUnrelatedFileEdit replaces the old
// TestReviewResetsWhenDiffChanges: under per-file review identity, editing a
// file that was never reviewed must not touch another file's review state (the
// old behaviour cleared ALL reviews whenever the whole-diff hash moved).
func TestReviewSurvivesUnrelatedFileEdit(t *testing.T) {
	root := buildWorkspace(t)
	e := newEngine(t, root)
	e.Refresh(context.Background())
	feat := findByBranch(e.List(), "feature")
	d, _ := e.Diff(feat.ID)
	newFile := findFile(d.Files, "new.go")
	if newFile == nil {
		t.Fatal("precondition: new.go must be in the diff")
	}
	if err := e.SetReviewed(feat.ID, "new.go", true, newFile.Hash); err != nil {
		t.Fatal(err)
	}

	// Change a different file's diff: edit app.go further in the worktree.
	wt := filepath.Join(root, "api-server-feature")
	os.WriteFile(filepath.Join(wt, "app.go"), []byte("package api\n\nfunc A() {}\nfunc B() {}\nfunc D() {}\n"), 0o644)
	e.Refresh(context.Background())

	got := findByBranch(e.List(), "feature")
	if got.Reviewed != 1 {
		t.Errorf("reviewing new.go must survive an unrelated edit to app.go, got reviewed=%d", got.Reviewed)
	}
}

// TestEditingOneFileUnreviewsOnlyThatFile is AC2: review two files, edit one of
// them, and only the edited file should flip back to unreviewed.
func TestEditingOneFileUnreviewsOnlyThatFile(t *testing.T) {
	root := buildWorkspace(t)
	e := newEngine(t, root)
	e.Refresh(context.Background())
	feat := findByBranch(e.List(), "feature")

	d, _ := e.Diff(feat.ID)
	if len(d.Files) < 2 {
		t.Fatalf("precondition: need >=2 files, got %d", len(d.Files))
	}
	for _, f := range d.Files {
		if err := e.SetReviewed(feat.ID, f.Path, true, f.Hash); err != nil {
			t.Fatalf("review %s: %v", f.Path, err)
		}
	}
	if got := findByBranch(e.List(), "feature").Reviewed; got != len(d.Files) {
		t.Fatalf("precondition: reviewed = %d, want all %d files reviewed", got, len(d.Files))
	}

	// Edit app.go only.
	wt := filepath.Join(root, "api-server-feature")
	os.WriteFile(filepath.Join(wt, "app.go"), []byte("package api\n\nfunc A() {}\nfunc B() {}\nfunc D() {}\n"), 0o644)
	if err := e.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	rev, err := e.st.ReviewedFiles(feat.ID)
	if err != nil {
		t.Fatal(err)
	}
	d2, _ := e.Diff(feat.ID)
	for _, f := range d2.Files {
		wantReviewed := f.Path != "app.go"
		gotReviewed := rev[f.Path] == f.Hash
		if gotReviewed != wantReviewed {
			t.Errorf("file %s: reviewed=%v, want %v", f.Path, gotReviewed, wantReviewed)
		}
	}
}

func TestSetReviewedRejectsStaleExpectedHash(t *testing.T) {
	root := buildWorkspace(t)
	e := newEngine(t, root)
	e.Refresh(context.Background())
	feat := findByBranch(e.List(), "feature")

	err := e.SetReviewed(feat.ID, "new.go", true, "not-the-real-hash")
	if !errors.Is(err, ErrFileChanged) {
		t.Errorf("expected ErrFileChanged, got %v", err)
	}
	// The stale attempt must not have recorded anything.
	rev, _ := e.st.ReviewedFiles(feat.ID)
	if _, ok := rev["new.go"]; ok {
		t.Errorf("a rejected review must not be recorded, got %+v", rev)
	}
}

func TestSetReviewedRejectsUnknownFile(t *testing.T) {
	root := buildWorkspace(t)
	e := newEngine(t, root)
	e.Refresh(context.Background())
	feat := findByBranch(e.List(), "feature")

	if err := e.SetReviewed(feat.ID, "does-not-exist.go", true, ""); !errors.Is(err, ErrFileNotFound) {
		t.Errorf("expected ErrFileNotFound, got %v", err)
	}
}

func testGitEnv() []string {
	return append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
}

// commitWorktree stages and commits everything in the feature worktree so it can
// be merged (the approve path requires a clean, committed worktree).
func commitWorktree(t *testing.T, root string) {
	wt := filepath.Join(root, "api-server-feature")
	git(t, wt, "add", "-A")
	git(t, wt, "commit", "-q", "-m", "feature work")
}

// TestReviewSurvivesCommit is the headline case this task exists for: review
// every file, commit the worktree, refresh — every file must still be reviewed
// and approve must proceed. Under the old whole-diff-hash reset, committing
// (review → commit → everything resets) broke this.
func TestReviewSurvivesCommit(t *testing.T) {
	root := buildWorkspace(t)
	e := newEngine(t, root)
	e.Refresh(context.Background())
	feat := findByBranch(e.List(), "feature")

	d, _ := e.Diff(feat.ID)
	for _, f := range d.Files {
		if err := e.SetReviewed(feat.ID, f.Path, true, f.Hash); err != nil {
			t.Fatalf("review %s: %v", f.Path, err)
		}
	}
	if got := findByBranch(e.List(), "feature").Reviewed; got != len(d.Files) {
		t.Fatalf("precondition: reviewed = %d before commit, want %d", got, len(d.Files))
	}

	commitWorktree(t, root)
	if err := e.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	feat2 := findByBranch(e.List(), "feature")
	if feat2.Reviewed != len(d.Files) {
		t.Errorf("reviewed = %d after commit, want %d (commit must not reset review)", feat2.Reviewed, len(d.Files))
	}
	if _, err := e.Approve(feat2.ID); err != nil {
		t.Fatalf("approve should proceed after commit+refresh, got: %v", err)
	}
}

func TestApproveRequiresFullReview(t *testing.T) {
	root := buildWorkspace(t)
	commitWorktree(t, root)
	e := newEngine(t, root)
	e.Refresh(context.Background())
	feat := findByBranch(e.List(), "feature")

	// Not reviewed yet → refuse.
	if _, err := e.Approve(feat.ID); err == nil {
		t.Fatal("approve should refuse an unreviewed worktree")
	} else if !strings.Contains(err.Error(), "review") {
		t.Errorf("error should mention review, got: %v", err)
	}

	// Review every file, then approve.
	d, _ := e.Diff(feat.ID)
	for _, f := range d.Files {
		if err := e.SetReviewed(feat.ID, f.Path, true, ""); err != nil {
			t.Fatal(err)
		}
	}
	res, err := e.Approve(feat.ID)
	if err != nil {
		t.Fatalf("approve after full review failed: %v", err)
	}
	if res.Merged != "feature" || res.Into != "main" {
		t.Errorf("unexpected result: %+v", res)
	}

	// The worktree should be gone from the registry...
	if findByBranch(e.List(), "feature") != nil {
		t.Error("approved worktree should be removed from the registry")
	}
	// ...and main should now contain the merged change.
	repo := filepath.Join(root, "api-server")
	appGo, _ := os.ReadFile(filepath.Join(repo, "app.go"))
	if !strings.Contains(string(appGo), "func B()") {
		t.Errorf("main did not receive merged change:\n%s", appGo)
	}
}

func TestApproveRequiresCleanWorktree(t *testing.T) {
	root := buildWorkspace(t) // feature worktree has UNcommitted changes
	e := newEngine(t, root)
	e.Refresh(context.Background())
	feat := findByBranch(e.List(), "feature")

	// Review everything so only the cleanliness gate can block.
	d, _ := e.Diff(feat.ID)
	for _, f := range d.Files {
		e.SetReviewed(feat.ID, f.Path, true, "")
	}
	_, err := e.Approve(feat.ID)
	if err == nil {
		t.Fatal("approve should refuse a dirty worktree")
	}
	if !strings.Contains(err.Error(), "uncommitted") {
		t.Errorf("error should mention uncommitted changes, got: %v", err)
	}
}

// TestApproveBlockedOnStaleReviewHash is AC3's stale-review case: a file is
// reviewed, then edited again (and re-committed, so only the review gate is in
// play) — approve must refuse because the stored hash no longer matches.
func TestApproveBlockedOnStaleReviewHash(t *testing.T) {
	root := buildWorkspace(t)
	commitWorktree(t, root)
	e := newEngine(t, root)
	e.Refresh(context.Background())
	feat := findByBranch(e.List(), "feature")

	d, _ := e.Diff(feat.ID)
	for _, f := range d.Files {
		if err := e.SetReviewed(feat.ID, f.Path, true, f.Hash); err != nil {
			t.Fatal(err)
		}
	}

	// Edit app.go again after review, then commit — worktree stays clean, but
	// app.go's stored review hash is now stale.
	wt := filepath.Join(root, "api-server-feature")
	os.WriteFile(filepath.Join(wt, "app.go"), []byte("package api\n\nfunc A() {}\nfunc B() {}\nfunc D() {}\n"), 0o644)
	commitWorktree(t, root)
	if err := e.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	feat = findByBranch(e.List(), "feature")

	if _, err := e.Approve(feat.ID); err == nil {
		t.Fatal("approve should refuse when a reviewed file's hash is stale")
	} else if !strings.Contains(err.Error(), "review") {
		t.Errorf("error should mention review, got: %v", err)
	}
}
