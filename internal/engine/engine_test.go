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

// fakeAWSKeyID is an AWS-access-key-ID-shaped fixture ("AKIA" + 16
// uppercase-alnum chars) built from two literal fragments so the 20-byte
// secret-shaped string never appears contiguous in this source file —
// GitHub's push-protection scanner matches file text, not the constant Go
// folds this into, so every test still sees the identical value as before.
const fakeAWSKeyID = "AKIA" + "ABCDEFGHIJKLMNOP"

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

// mustResolver builds a guardrail.Resolver over rules with no per-repo packs
// in play — the engine-level test fixtures' equivalent of the old
// guardrail.New (test helpers aren't importable across packages, so this is
// re-declared per-package like every other test helper in this repo).
func mustResolver(t *testing.T, rules []guardrail.Rule) *guardrail.Resolver {
	t.Helper()
	r, err := guardrail.NewResolver(rules, "default")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func newEngine(t *testing.T, root string) *Engine {
	t.Helper()
	reg := registry.New()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	be := gitbackend.NewCLIWithEnv(testGitEnv())
	gr := mustResolver(t, guardrail.DefaultRules())
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

// TestReviewedMapReflectsPerFileHashMatch is the engine-level TDD case for
// the WP3 sanctioned API addition (P3-design.md): GET /api/diff's per-file
// `reviewed` map is computed from the same per-file hash match SetReviewed/
// buildWorktree's aggregate count already use. Unreviewed files are absent
// from ReviewedFiles entirely, so they must read as false, not merely
// "missing".
func TestReviewedMapReflectsPerFileHashMatch(t *testing.T) {
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

	got, ok := e.ReviewedMap(feat.ID)
	if !ok {
		t.Fatal("ReviewedMap() ok = false, want true for a known worktree")
	}
	if !got["new.go"] {
		t.Errorf("ReviewedMap()[new.go] = false, want true after SetReviewed")
	}
	for _, f := range d.Files {
		if f.Path != "new.go" && got[f.Path] {
			t.Errorf("ReviewedMap()[%s] = true, want false (never reviewed)", f.Path)
		}
	}
}

// TestReviewedMapUnknownWorktreeReturnsFalseOK mirrors Diff()'s own
// unknown-id contract (ok=false, zero value) rather than a distinct error —
// cmd/wtd's handleDiff already 404s on Diff()'s ok=false before ever
// reaching ReviewedMap.
func TestReviewedMapUnknownWorktreeReturnsFalseOK(t *testing.T) {
	e := newEngine(t, t.TempDir())
	if _, ok := e.ReviewedMap("no-such-id"); ok {
		t.Error("ReviewedMap() ok = true, want false for an unknown worktree")
	}
}

// TestReviewedMapFlipsFalseAfterEdit is the engine-level half of the TESTS
// brief's "reviewed→commit→still true; edit→false" (the commit-survives half
// is already TestReviewSurvivesCommit; this is the edit-resets half, at the
// ReviewedMap accessor specifically rather than the aggregate count).
func TestReviewedMapFlipsFalseAfterEdit(t *testing.T) {
	root := buildWorkspace(t)
	e := newEngine(t, root)
	e.Refresh(context.Background())
	feat := findByBranch(e.List(), "feature")

	d, _ := e.Diff(feat.ID)
	newFile := findFile(d.Files, "new.go")
	if err := e.SetReviewed(feat.ID, "new.go", true, newFile.Hash); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.ReviewedMap(feat.ID); !got["new.go"] {
		t.Fatal("precondition: new.go should read reviewed before the edit")
	}

	wt := filepath.Join(root, "api-server-feature")
	if err := os.WriteFile(filepath.Join(wt, "new.go"), []byte("package api\n\nfunc C() {}\nfunc E() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	got, _ := e.ReviewedMap(feat.ID)
	if got["new.go"] {
		t.Error("ReviewedMap()[new.go] = true after editing the file, want false (stale review hash)")
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

// buildTwoBranchWorkspace makes a repo where "develop" has diverged from
// "main" by one commit (develop-only.txt), and a "feature" worktree is
// branched off "develop" with its own uncommitted addition (feature.txt).
// It returns the workspace root and the repo's absolute path.
func buildTwoBranchWorkspace(t *testing.T) (root, repoPath string) {
	root = t.TempDir()
	repoPath = filepath.Join(root, "api-server")
	os.MkdirAll(repoPath, 0o755)
	git(t, repoPath, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(repoPath, "shared.txt"), []byte("base\n"), 0o644)
	git(t, repoPath, "add", ".")
	git(t, repoPath, "commit", "-q", "-m", "init")

	git(t, repoPath, "branch", "develop")
	git(t, repoPath, "checkout", "-q", "develop")
	os.WriteFile(filepath.Join(repoPath, "develop-only.txt"), []byte("d\n"), 0o644)
	git(t, repoPath, "add", ".")
	git(t, repoPath, "commit", "-q", "-m", "on develop")
	git(t, repoPath, "checkout", "-q", "main")

	wt := filepath.Join(root, "api-server-feature")
	git(t, repoPath, "worktree", "add", "-q", "-b", "feature", wt, "develop")
	os.WriteFile(filepath.Join(wt, "feature.txt"), []byte("f\n"), 0o644)
	return root, repoPath
}

// TestPerRepoBaseOverridePicksCorrectMergeBase is the per-repo base plumbing
// test: with a global default of "main" but a BaseFor override pointing this
// one repo at "develop", the feature worktree (itself branched off develop)
// must diff against develop's merge-base — so develop's own exclusive commit
// (develop-only.txt) must NOT show up as part of feature's diff, only
// feature's own change should. Without the override (global "main" only),
// the same worktree's diff against main *does* include develop-only.txt,
// proving the override is what changed the outcome.
func TestPerRepoBaseOverridePicksCorrectMergeBase(t *testing.T) {
	root, repoPath := buildTwoBranchWorkspace(t)

	reg := registry.New()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	be := gitbackend.NewCLIWithEnv(testGitEnv())
	gr := mustResolver(t, guardrail.DefaultRules())

	// Baseline: no per-repo override, global base "main".
	eNoOverride := New(Config{
		Roots:          []string{root},
		MaxDepth:       4,
		DefaultBase:    "main",
		ActivityWindow: 30 * time.Second,
	}, be, reg, st, gr)
	if err := eNoOverride.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	feat := findByBranch(eNoOverride.List(), "feature")
	if feat == nil {
		t.Fatalf("feature worktree not tracked; got %+v", eNoOverride.List())
	}
	d, _ := eNoOverride.Diff(feat.ID)
	if findFile(d.Files, "develop-only.txt") == nil {
		t.Fatalf("precondition: diffing against main should include develop-only.txt, got %+v", d.Files)
	}

	// With the override: same workspace, fresh engine/registry/store so caches
	// don't leak between the two assertions.
	reg2 := registry.New()
	st2, err := store.Open(filepath.Join(t.TempDir(), "state2.db"))
	if err != nil {
		t.Fatal(err)
	}
	eOverride := New(Config{
		Roots:          []string{root},
		MaxDepth:       4,
		DefaultBase:    "main",
		BaseFor:        map[string]string{repoPath: "develop"},
		ActivityWindow: 30 * time.Second,
	}, be, reg2, st2, gr)
	if err := eOverride.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	feat2 := findByBranch(eOverride.List(), "feature")
	if feat2 == nil {
		t.Fatalf("feature worktree not tracked; got %+v", eOverride.List())
	}
	if feat2.Base != "develop" {
		t.Errorf("Base = %q, want develop (per-repo override)", feat2.Base)
	}
	d2, _ := eOverride.Diff(feat2.ID)
	if findFile(d2.Files, "develop-only.txt") != nil {
		t.Errorf("diff against develop base should not include develop's own file: %+v", d2.Files)
	}
	if findFile(d2.Files, "feature.txt") == nil {
		t.Errorf("expected feature.txt in diff: %+v", d2.Files)
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

// TestFileRenamePrunesOldPathReviewAndLeavesNewPathUnreviewed covers the
// rename edge of per-file review identity: review app.go, rename it to
// app2.go (git mv, which git detects as a rename since content stays highly
// similar), refresh. The old path's review record must be pruned (it can
// never be hash-matched again — pruneReviews drops any stored path no longer
// in the diff), and the new path must start unreviewed — nothing carries a
// review across a rename automatically.
func TestFileRenamePrunesOldPathReviewAndLeavesNewPathUnreviewed(t *testing.T) {
	root := buildWorkspace(t)
	e := newEngine(t, root)
	e.Refresh(context.Background())
	feat := findByBranch(e.List(), "feature")

	d, _ := e.Diff(feat.ID)
	appFile := findFile(d.Files, "app.go")
	if appFile == nil {
		t.Fatal("precondition: app.go must be in the diff")
	}
	if err := e.SetReviewed(feat.ID, "app.go", true, appFile.Hash); err != nil {
		t.Fatal(err)
	}

	wt := filepath.Join(root, "api-server-feature")
	git(t, wt, "mv", "app.go", "app2.go")
	if err := e.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	rev, _ := e.st.ReviewedFiles(feat.ID)
	if _, ok := rev["app.go"]; ok {
		t.Errorf("old path's review record should be pruned after rename, got %+v", rev)
	}

	d2, _ := e.Diff(feat.ID)
	renamed := findFile(d2.Files, "app2.go")
	if renamed == nil {
		t.Fatalf("expected app2.go (renamed) in the diff: %+v", d2.Files)
	}
	if renamed.Status != model.FileRenamed || renamed.OldPath != "app.go" {
		t.Errorf("renamed file = %+v, want status=renamed oldPath=app.go", renamed)
	}
	if rev[renamed.Path] == renamed.Hash {
		t.Errorf("the new path must not inherit review state from the old path, got %+v", rev)
	}
	got := findByBranch(e.List(), "feature")
	if got.Reviewed != 0 {
		t.Errorf("reviewed count = %d, want 0 (renamed file starts unreviewed)", got.Reviewed)
	}
}

// TestFileRestoredToReviewedContentDoesNotResurrectOldReview: a file is
// reviewed, then edited back to byte-identical-with-base content so it drops
// out of the diff entirely (pruneReviews removes its store entry at that
// point, per the code). Restoring the exact previously-reviewed content later
// reproduces the identical per-file hash — but the store entry is gone for
// good, so the file must NOT resurrect as reviewed. Per the pruning design in
// engine.go (files that drop out "can never be hash-matched again"), this is
// the correct behaviour, not a bug: reappearing is indistinguishable from a
// brand new occurrence of that content once the store record was dropped.
func TestFileRestoredToReviewedContentDoesNotResurrectOldReview(t *testing.T) {
	root := buildWorkspace(t)
	e := newEngine(t, root)
	wt := filepath.Join(root, "api-server-feature")

	e.Refresh(context.Background())
	feat := findByBranch(e.List(), "feature")
	d, _ := e.Diff(feat.ID)
	appFile := findFile(d.Files, "app.go")
	if appFile == nil {
		t.Fatal("precondition: app.go must be in the diff")
	}
	reviewedContent, err := os.ReadFile(filepath.Join(wt, "app.go"))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.SetReviewed(feat.ID, "app.go", true, appFile.Hash); err != nil {
		t.Fatal(err)
	}

	// Revert to base's exact content: app.go drops out of the diff entirely.
	baseContent, err := os.ReadFile(filepath.Join(root, "api-server", "app.go"))
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(wt, "app.go"), baseContent, 0o644)
	if err := e.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	d2, _ := e.Diff(feat.ID)
	if findFile(d2.Files, "app.go") != nil {
		t.Fatalf("precondition: app.go should have dropped out of the diff, got %+v", d2.Files)
	}
	rev, _ := e.st.ReviewedFiles(feat.ID)
	if _, ok := rev["app.go"]; ok {
		t.Fatalf("precondition: app.go's review record should be pruned once it left the diff, got %+v", rev)
	}

	// Restore the exact previously-reviewed content: the file reappears with
	// the identical hash it had when it was reviewed.
	os.WriteFile(filepath.Join(wt, "app.go"), reviewedContent, 0o644)
	if err := e.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	d3, _ := e.Diff(feat.ID)
	appFile3 := findFile(d3.Files, "app.go")
	if appFile3 == nil {
		t.Fatalf("app.go should be back in the diff: %+v", d3.Files)
	}
	if appFile3.Hash != appFile.Hash {
		t.Fatalf("restoring identical content should reproduce the identical hash, got %q want %q", appFile3.Hash, appFile.Hash)
	}

	rev2, _ := e.st.ReviewedFiles(feat.ID)
	if rev2["app.go"] == appFile3.Hash {
		t.Errorf("a re-appeared file must NOT resurrect as reviewed via a stale store entry, got reviewed hash %q", rev2["app.go"])
	}
	got := findByBranch(e.List(), "feature")
	if got.Reviewed != 0 {
		t.Errorf("reviewed count = %d, want 0 (restored file must count as unreviewed)", got.Reviewed)
	}
}

// TestReviewDoesNotCrossContaminateBetweenWorktreesWithSameFilePath: two
// worktrees of the same repo each have their own "app.go" changed
// differently. Reviewing one must not affect the other's review state, even
// though the file path (the store's second-level map key) is identical —
// isolation must come from the worktree id (first-level key).
func TestReviewDoesNotCrossContaminateBetweenWorktreesWithSameFilePath(t *testing.T) {
	root := buildWorkspace(t)
	repo := filepath.Join(root, "api-server")
	wt2 := filepath.Join(root, "api-server-feature2")
	git(t, repo, "worktree", "add", "-q", "-b", "feature2", wt2)
	os.WriteFile(filepath.Join(wt2, "app.go"), []byte("package api\n\nfunc A() {}\nfunc Z() {}\n"), 0o644)

	e := newEngine(t, root)
	if err := e.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	feat1 := findByBranch(e.List(), "feature")
	feat2 := findByBranch(e.List(), "feature2")
	if feat1 == nil || feat2 == nil {
		t.Fatalf("expected both feature worktrees tracked: %+v", e.List())
	}
	if feat1.ID == feat2.ID {
		t.Fatalf("distinct worktrees must have distinct ids, both got %q", feat1.ID)
	}

	d1, _ := e.Diff(feat1.ID)
	f1 := findFile(d1.Files, "app.go")
	if f1 == nil {
		t.Fatal("precondition: feature's app.go must be in its diff")
	}
	if err := e.SetReviewed(feat1.ID, "app.go", true, f1.Hash); err != nil {
		t.Fatal(err)
	}

	got2 := findByBranch(e.List(), "feature2")
	if got2.Reviewed != 0 {
		t.Errorf("reviewing feature's app.go must not mark feature2's app.go reviewed, got reviewed=%d", got2.Reviewed)
	}
	rev2, _ := e.st.ReviewedFiles(feat2.ID)
	if len(rev2) != 0 {
		t.Errorf("feature2's review store should be untouched by feature's review, got %+v", rev2)
	}
	// feature's own review must still hold.
	got1 := findByBranch(e.List(), "feature")
	if got1.Reviewed != 1 {
		t.Errorf("feature's own reviewed count = %d, want 1", got1.Reviewed)
	}
}

// TestSetReviewedUnreviewRoundTrip: review a file, unreview it, review it
// again — the reviewed count and store state must track exactly, and the
// same expectedHash gate that guards reviewed=true must also guard
// reviewed=false (an unreview against a stale hash is rejected too).
func TestSetReviewedUnreviewRoundTrip(t *testing.T) {
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
	if got := findByBranch(e.List(), "feature").Reviewed; got != 1 {
		t.Fatalf("precondition: reviewed = %d, want 1", got)
	}

	if err := e.SetReviewed(feat.ID, "new.go", false, newFile.Hash); err != nil {
		t.Fatalf("unreview with the still-current hash should succeed: %v", err)
	}
	if got := findByBranch(e.List(), "feature").Reviewed; got != 0 {
		t.Errorf("reviewed = %d after unreview, want 0", got)
	}
	rev, _ := e.st.ReviewedFiles(feat.ID)
	if _, ok := rev["new.go"]; ok {
		t.Errorf("unreview should remove the store entry entirely, got %+v", rev)
	}

	// Round-trip: reviewing again with the same (still current) hash succeeds.
	if err := e.SetReviewed(feat.ID, "new.go", true, newFile.Hash); err != nil {
		t.Fatalf("re-reviewing after unreview should succeed: %v", err)
	}
	if got := findByBranch(e.List(), "feature").Reviewed; got != 1 {
		t.Errorf("reviewed = %d after re-review, want 1", got)
	}

	// Unreviewing with a stale expectedHash is rejected exactly like reviewing is.
	if err := e.SetReviewed(feat.ID, "new.go", false, "not-the-real-hash"); !errors.Is(err, ErrFileChanged) {
		t.Errorf("unreview with a stale expectedHash should return ErrFileChanged, got %v", err)
	}
	if got := findByBranch(e.List(), "feature").Reviewed; got != 1 {
		t.Errorf("a rejected unreview must not change the reviewed count, got %d want 1", got)
	}
}

// TestFileHashStableAcrossUntrackedStagedAndCommittedStates walks new.go
// through three distinct git states relative to the same content — untracked
// (synthesized via untrackedDiff's `git diff --no-index`), staged (`git add`,
// picked up by the main DiffAgainstBase call as a "new file"), and committed
// (still "new" relative to the merge-base) — and asserts the per-file hash
// never moves. This is the property per-file review identity depends on: a
// file's hash must be a function of its content, not of which git plumbing
// path happened to produce the diff for it.
func TestFileHashStableAcrossUntrackedStagedAndCommittedStates(t *testing.T) {
	root := buildWorkspace(t)
	e := newEngine(t, root)
	wt := filepath.Join(root, "api-server-feature")

	if err := e.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	feat := findByBranch(e.List(), "feature")
	d1, _ := e.Diff(feat.ID)
	f1 := findFile(d1.Files, "new.go")
	if f1 == nil {
		t.Fatal("precondition: untracked new.go must appear in the diff")
	}
	hUntracked := f1.Hash
	if hUntracked == "" {
		t.Fatal("untracked new.go should have a non-empty hash")
	}

	git(t, wt, "add", "new.go")
	if err := e.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	d2, _ := e.Diff(feat.ID)
	f2 := findFile(d2.Files, "new.go")
	if f2 == nil {
		t.Fatal("new.go missing from the diff once staged")
	}
	if f2.Hash != hUntracked {
		t.Errorf("staged hash %q != untracked hash %q", f2.Hash, hUntracked)
	}

	git(t, wt, "commit", "-q", "-m", "add new.go")
	if err := e.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	d3, _ := e.Diff(feat.ID)
	f3 := findFile(d3.Files, "new.go")
	if f3 == nil {
		t.Fatal("new.go missing from the diff once committed")
	}
	if f3.Hash != hUntracked {
		t.Errorf("committed hash %q != untracked hash %q", f3.Hash, hUntracked)
	}
}

// TestApproveFailsAfterClearWorktreeWipesReviews: if a worktree's review
// state is cleared (the only production caller is Approve's own
// post-merge cleanup, but the store method is part of the Store interface
// and nothing stops another path from calling it), a later Approve attempt
// must refuse — gate 1 re-derives "reviewed" from the store on every call, so
// wiping it must force every file back to unreviewed.
func TestApproveFailsAfterClearWorktreeWipesReviews(t *testing.T) {
	root := buildWorkspace(t)
	commitWorktree(t, root)
	e := newEngine(t, root)
	e.Refresh(context.Background())
	feat := findByBranch(e.List(), "feature")

	d, _ := e.Diff(feat.ID)
	for _, f := range d.Files {
		if err := e.SetReviewed(feat.ID, f.Path, true, f.Hash); err != nil {
			t.Fatalf("review %s: %v", f.Path, err)
		}
	}

	if err := e.st.ClearWorktree(feat.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := e.Approve(feat.ID); err == nil {
		t.Fatal("approve should refuse once ClearWorktree has wiped review state")
	} else if !strings.Contains(err.Error(), "review") {
		t.Errorf("error should mention review, got: %v", err)
	}
}

// TestDoubleApproveFailsCleanlyOnSecondCall: Approve's own success path calls
// ClearWorktree, deletes the cache entry and removes the worktree from the
// registry. Calling Approve again on the same id afterwards (e.g. a
// double-click) must fail cleanly ("unknown worktree"), never panic.
func TestDoubleApproveFailsCleanlyOnSecondCall(t *testing.T) {
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
	if _, err := e.Approve(feat.ID); err != nil {
		t.Fatalf("first approve: %v", err)
	}

	if _, err := e.Approve(feat.ID); err == nil {
		t.Fatal("second approve on an already-approved (removed) worktree should fail, not panic")
	}
}

// buildUnicodeSpaceWorkspace makes a workspace with a feature worktree that
// modifies a tracked file whose name contains a space, and adds an untracked
// file whose name contains non-ASCII (unicode) characters — both legal git
// paths.
func buildUnicodeSpaceWorkspace(t *testing.T) (root string) {
	root = t.TempDir()
	repo := filepath.Join(root, "api-server")
	os.MkdirAll(repo, 0o755)
	git(t, repo, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(repo, "file with space.txt"), []byte("base\n"), 0o644)
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-q", "-m", "init")

	wt := filepath.Join(root, "api-server-feature")
	git(t, repo, "worktree", "add", "-q", "-b", "feature", wt)
	os.WriteFile(filepath.Join(wt, "file with space.txt"), []byte("base\nedited\n"), 0o644)
	os.WriteFile(filepath.Join(wt, "café.txt"), []byte("hello café\n"), 0o644)
	return root
}

// TestReviewPathsWithSpacesAndUnicode is DEFECT D1: git's default
// core.quotePath=true quotes+octal-escapes non-ASCII paths (e.g.
// "caf\303\251.txt" for "café.txt") on the "diff --git"/"+++" lines, and
// separately appends a bare trailing tab after any "+++ "/"--- " path that
// merely *contains a space* (git's own disambiguation marker for such
// paths). diffparse.stripPrefix only trims literal surrounding double
// quotes — it never decodes the octal escapes and never trims the trailing
// tab — so model.DiffFile.Path ends up corrupted for both kinds of path
// (confirmed empirically: a unicode filename parses to the literal escaped
// string, and a space-containing filename picks up an invisible trailing
// tab). This breaks the file-identity contract SetReviewed relies on: a
// caller (UI/API client) that requests review using the real on-disk
// filename gets ErrFileNotFound because the engine's cur.Path never actually
// equals that string.
func TestReviewPathsWithSpacesAndUnicode(t *testing.T) {
	root := buildUnicodeSpaceWorkspace(t)
	e := newEngine(t, root)
	if err := e.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	feat := findByBranch(e.List(), "feature")
	d, _ := e.Diff(feat.ID)

	spaced := findFile(d.Files, "file with space.txt")
	if spaced == nil {
		t.Fatalf(`expected a file literally named "file with space.txt" in the diff, got %+v`, d.Files)
	}
	if err := e.SetReviewed(feat.ID, "file with space.txt", true, spaced.Hash); err != nil {
		t.Errorf("review by the real filename (with space) should succeed, got %v", err)
	}

	unicodeFile := findFile(d.Files, "café.txt")
	if unicodeFile == nil {
		t.Fatalf(`expected a file literally named "café.txt" in the diff, got %+v`, d.Files)
	}
	if err := e.SetReviewed(feat.ID, "café.txt", true, unicodeFile.Hash); err != nil {
		t.Errorf("review by the real filename (unicode) should succeed, got %v", err)
	}
}

// buildBinaryWorkspace makes a workspace with a feature worktree that
// modifies a tracked BINARY file in place (same path, no rename). The base
// content starts with a NUL byte so git's binary heuristic classifies every
// subsequent diff of this path as binary — zero hunks — regardless of what
// the feature branch's bytes look like afterwards (confirmed against real
// git: binary classification depends on either side of the diff, and the
// merge-base side never changes as the feature branch is edited further).
func buildBinaryWorkspace(t *testing.T) (root string) {
	root = t.TempDir()
	repo := filepath.Join(root, "api-server")
	os.MkdirAll(repo, 0o755)
	git(t, repo, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(repo, "asset.bin"), []byte{0x00, 0x01, 0x02, 0x03}, 0o644)
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-q", "-m", "init")

	wt := filepath.Join(root, "api-server-feature")
	git(t, repo, "worktree", "add", "-q", "-b", "feature", wt)
	os.WriteFile(filepath.Join(wt, "asset.bin"), []byte{0xAA, 0xBB, 0xCC, 0xDD}, 0o644)
	return root
}

// TestBinaryFileHashChangesWithContent is DEFECT B1's core parsing-to-hash
// claim: engine.fileHash used to be sha1(path + status + hunk lines), and a
// binary diff always has zero hunks, so two completely different binary
// bodies at the same path produced the identical Hash. Folding OldBlob/NewBlob
// in fixes it — the blob id is a pure function of content.
func TestBinaryFileHashChangesWithContent(t *testing.T) {
	root := buildBinaryWorkspace(t)
	e := newEngine(t, root)
	if err := e.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	feat := findByBranch(e.List(), "feature")
	d, _ := e.Diff(feat.ID)
	f := findFile(d.Files, "asset.bin")
	if f == nil || !f.Binary {
		t.Fatalf("precondition: asset.bin must be a binary diff entry, got %+v", d.Files)
	}
	if len(f.Hunks) != 0 {
		t.Fatalf("precondition: a binary diff must have zero hunks, got %d", len(f.Hunks))
	}
	hash1 := f.Hash
	if hash1 == "" {
		t.Fatal("precondition: asset.bin should have a non-empty hash")
	}

	wt := filepath.Join(root, "api-server-feature")
	os.WriteFile(filepath.Join(wt, "asset.bin"), []byte{0x11, 0x22, 0x33, 0x44, 0x55}, 0o644)
	if err := e.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	d2, _ := e.Diff(findByBranch(e.List(), "feature").ID)
	f2 := findFile(d2.Files, "asset.bin")
	if f2 == nil {
		t.Fatal("asset.bin missing from the diff after a further edit")
	}
	if f2.Hash == hash1 {
		t.Errorf("two different binary bodies at the same path produced the same Hash %q (zero hunks either way) — a reviewed binary would stay reviewed after its bytes changed", hash1)
	}
}

// TestApproveRefusesAfterBinaryFileBytesChangeWithZeroHunks is DEFECT B1's
// end-to-end exploit: review a binary file, then swap its bytes for
// completely different ones (still zero hunks either way) and commit —
// without B1's fix this bypasses the review gate entirely, since the old
// path+status+hunks hash never moved.
func TestApproveRefusesAfterBinaryFileBytesChangeWithZeroHunks(t *testing.T) {
	root := buildBinaryWorkspace(t)
	commitWorktree(t, root)
	e := newEngine(t, root)
	e.Refresh(context.Background())
	feat := findByBranch(e.List(), "feature")

	d, _ := e.Diff(feat.ID)
	f := findFile(d.Files, "asset.bin")
	if f == nil {
		t.Fatal("precondition: asset.bin must be in the diff")
	}
	if err := e.SetReviewed(feat.ID, "asset.bin", true, f.Hash); err != nil {
		t.Fatalf("review asset.bin: %v", err)
	}

	// Swap the bytes for something completely different, then commit — a
	// binary diff renders zero hunks regardless, so only OldBlob/NewBlob can
	// reveal the change.
	wt := filepath.Join(root, "api-server-feature")
	os.WriteFile(filepath.Join(wt, "asset.bin"), []byte{0xDE, 0xAD, 0xBE, 0xEF}, 0o644)
	commitWorktree(t, root)
	if err := e.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	feat2 := findByBranch(e.List(), "feature")
	if feat2.Reviewed != 0 {
		t.Errorf("swapping a reviewed binary file's bytes should unreview it, got reviewed=%d", feat2.Reviewed)
	}
	if _, err := e.Approve(feat2.ID); err == nil {
		t.Fatal("approve should refuse: asset.bin's bytes changed since it was reviewed, even though the binary diff has zero hunks")
	}
}

// TestApproveRefusesWhenWorktreeChangedSinceLastRefresh is FIX M1: Approve
// must never trust a possibly-stale cache entry. Review every file, then
// edit+commit further in the worktree WITHOUT ever calling any engine
// refresh (the real window between polls/fsnotify debounce and an Approve
// call, or a concurrent caller) — Approve itself must re-diff fresh and
// refuse, because the stored review no longer matches the file's *actual
// current* content, even though the engine's cache hasn't been told yet.
func TestApproveRefusesWhenWorktreeChangedSinceLastRefresh(t *testing.T) {
	root := buildWorkspace(t)
	commitWorktree(t, root)
	e := newEngine(t, root)
	e.Refresh(context.Background())
	feat := findByBranch(e.List(), "feature")

	d, _ := e.Diff(feat.ID)
	for _, f := range d.Files {
		if err := e.SetReviewed(feat.ID, f.Path, true, f.Hash); err != nil {
			t.Fatalf("review %s: %v", f.Path, err)
		}
	}

	// Edit app.go further and commit, but deliberately do NOT refresh the
	// engine — Approve itself must catch this via its own fresh re-diff.
	wt := filepath.Join(root, "api-server-feature")
	os.WriteFile(filepath.Join(wt, "app.go"), []byte("package api\n\nfunc A() {}\nfunc B() {}\nfunc D() {}\n"), 0o644)
	commitWorktree(t, root)

	if _, err := e.Approve(feat.ID); err == nil {
		t.Fatal("approve should refuse: app.go changed since it was reviewed, and the engine cache was never refreshed")
	} else if !strings.Contains(err.Error(), "review") {
		t.Errorf("error should mention review, got: %v", err)
	}
}

// ---- v0.5: per-repo rule packs, guardrail.tripped, Rules() provenance ----

// writePack writes a .wtcockpit.toml at dir (a repo's or a worktree's root).
func writePack(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, ".wtcockpit.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestPackFromMainWorktreeDisablesAndAddsRule is the per-repo pack precedence
// e2e: a .wtcockpit.toml checked in at the repo's MAIN worktree root disables
// a tripping default rule and adds a new one, and both take effect.
func TestPackFromMainWorktreeDisablesAndAddsRule(t *testing.T) {
	root := buildWorkspace(t)
	repo := filepath.Join(root, "api-server")
	writePack(t, repo, "disable_rules = [\"touches-migrations\"]\n\n"+
		"[[rules]]\n"+
		"name = \"custom-added\"\n"+
		"severity = \"danger\"\n"+
		"path_glob = \"new.go\"\n"+
		"message = \"custom rule from pack\"\n")

	e := newEngine(t, root)
	if err := e.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	feat := findByBranch(e.List(), "feature")

	for _, h := range feat.Guardrails {
		if h.Rule == "touches-migrations" {
			t.Errorf("touches-migrations should be disabled by the pack, got %+v", feat.Guardrails)
		}
	}
	var sawCustom bool
	for _, h := range feat.Guardrails {
		if h.Rule == "custom-added" && h.File == "new.go" {
			sawCustom = true
		}
	}
	if !sawCustom {
		t.Errorf("expected the pack-added custom-added rule to trip on new.go, got %+v", feat.Guardrails)
	}

	eff, ok := e.Rules(feat.ID)
	if !ok {
		t.Fatal("Rules() ok=false for a known worktree")
	}
	if eff.PackStatus != "ok" {
		t.Errorf("PackStatus = %q, want ok", eff.PackStatus)
	}
	for _, r := range eff.Rules {
		if r.Name == "custom-added" && r.Source != "pack" {
			t.Errorf("custom-added should be tagged source=pack, got %+v", r)
		}
	}
}

// TestPackInFeatureWorktreeHasNoEffect is the trust-boundary test
// (P5-design.md §1.3): the exact same weakening pack placed in the FEATURE
// worktree (not the main one) must be completely inert — an agent working in
// its own worktree cannot disable the guardrails judging its own diff.
func TestPackInFeatureWorktreeHasNoEffect(t *testing.T) {
	root := buildWorkspace(t)
	wt := filepath.Join(root, "api-server-feature")
	writePack(t, wt, `disable_rules = ["touches-migrations"]`+"\n")

	e := newEngine(t, root)
	if err := e.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	feat := findByBranch(e.List(), "feature")

	var sawMigration bool
	for _, h := range feat.Guardrails {
		if h.Rule == "touches-migrations" {
			sawMigration = true
		}
	}
	if !sawMigration {
		t.Errorf("a .wtcockpit.toml placed in the FEATURE worktree must have no effect; touches-migrations should still fire, got %+v", feat.Guardrails)
	}

	eff, ok := e.Rules(feat.ID)
	if !ok {
		t.Fatal("Rules() ok=false")
	}
	if eff.PackPath != "" || eff.PackStatus != "none" {
		t.Errorf("PackPath/PackStatus = %q/%q, want empty/none (pack must be read from the main worktree only)", eff.PackPath, eff.PackStatus)
	}
}

// TestMalformedPackFallsBackToGlobalRules covers the fail-closed contract at
// the engine level: a repo with a malformed .wtcockpit.toml keeps running on
// global rules (never half-applied, never disabled outright), with the
// failure observable via Rules()/RulePackStats.
func TestMalformedPackFallsBackToGlobalRules(t *testing.T) {
	root := buildWorkspace(t)
	repo := filepath.Join(root, "api-server")
	writePack(t, repo, "not valid [ toml")

	e := newEngine(t, root)
	if err := e.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	feat := findByBranch(e.List(), "feature")

	eff, ok := e.Rules(feat.ID)
	if !ok {
		t.Fatal("Rules() ok=false")
	}
	if !strings.HasPrefix(eff.PackStatus, "error:") {
		t.Errorf("PackStatus = %q, want an error: prefix for a malformed pack", eff.PackStatus)
	}

	var sawMigration bool
	for _, h := range feat.Guardrails {
		if h.Rule == "touches-migrations" {
			sawMigration = true
		}
	}
	if !sawMigration {
		t.Errorf("a malformed pack must fail closed to global rules (touches-migrations should still fire), got %+v", feat.Guardrails)
	}

	if _, errs := e.RulePackStats(); errs != 1 {
		t.Errorf("RulePackStats errors = %d, want 1", errs)
	}
}

// TestRulesReturnsSourceProvenanceForDefaultRules pins Rules()'s provenance
// tagging when the engine runs on DefaultRules() with no pack in play: every
// rule reads back tagged "default".
func TestRulesReturnsSourceProvenanceForDefaultRules(t *testing.T) {
	root := buildWorkspace(t)
	e := newEngine(t, root)
	if err := e.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	feat := findByBranch(e.List(), "feature")

	eff, ok := e.Rules(feat.ID)
	if !ok {
		t.Fatal("Rules() ok=false for a known worktree")
	}
	if eff.WorktreeID != feat.ID {
		t.Errorf("WorktreeID = %q, want %q", eff.WorktreeID, feat.ID)
	}
	if eff.RepoPath == "" {
		t.Error("RepoPath should be set")
	}
	if eff.PackStatus != "none" {
		t.Errorf("PackStatus = %q, want none", eff.PackStatus)
	}
	if len(eff.Rules) == 0 {
		t.Fatal("expected at least the default rules")
	}
	for _, r := range eff.Rules {
		if r.Source != "default" {
			t.Errorf("expected every rule tagged default (no config [[rules]], no pack), got %+v", r)
		}
	}
}

// TestRulesUnknownWorktreeReturnsFalseOK mirrors Diff()'s own unknown-id
// contract.
func TestRulesUnknownWorktreeReturnsFalseOK(t *testing.T) {
	e := newEngine(t, t.TempDir())
	if _, ok := e.Rules("no-such-id"); ok {
		t.Error("Rules() ok = true, want false for an unknown worktree")
	}
}

// drainEvents collects every event received on sub within wait — used to
// assert on a NEGATIVE ("nothing of this type published") as well as a
// positive outcome, the same wall-clock-bounded style
// TestUnchangedUpsertDoesNotEmit already uses in the registry package.
func drainEvents(sub <-chan model.Event, wait time.Duration) []model.Event {
	var out []model.Event
	timeout := time.After(wait)
	for {
		select {
		case e := <-sub:
			out = append(out, e)
		case <-timeout:
			return out
		}
	}
}

func guardrailEvents(events []model.Event) []model.Event {
	var out []model.Event
	for _, e := range events {
		if e.Type == model.EventGuardrail {
			out = append(out, e)
		}
	}
	return out
}

// TestGuardrailTrippedSuppressedDuringFirstScanThenEmittedOnNewHit is the
// headline event-plumbing test (P5-design.md §1.4): buildWorkspace's
// worktree already has standing hits (touches-migrations, large-deletion) on
// its very first Refresh — none of those may publish guardrail.tripped. A
// hit that appears afterwards (a brand new binary file, tripping
// binary-added) must publish exactly once, keyed on (rule, file); a further
// no-op re-scan must not re-fire it.
func TestGuardrailTrippedSuppressedDuringFirstScanThenEmittedOnNewHit(t *testing.T) {
	root := buildWorkspace(t)
	e := newEngine(t, root)

	sub, cancel := e.Registry().Subscribe(64)
	defer cancel()

	if err := e.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	feat := findByBranch(e.List(), "feature")
	if len(feat.Guardrails) == 0 {
		t.Fatal("precondition: buildWorkspace's feature worktree should already have standing guardrail hits")
	}
	if ev := guardrailEvents(drainEvents(sub, 200*time.Millisecond)); len(ev) != 0 {
		t.Fatalf("no guardrail.tripped events should publish during the first scan, got %+v", ev)
	}

	wt := filepath.Join(root, "api-server-feature")
	if err := os.WriteFile(filepath.Join(wt, "asset.bin"), []byte{0x00, 0x01, 0x02, 0x03}, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.RefreshOne(context.Background(), wt); err != nil {
		t.Fatal(err)
	}

	events := guardrailEvents(drainEvents(sub, time.Second))
	var saw bool
	for _, ev := range events {
		if ev.ID == feat.ID && ev.Hit != nil && ev.Hit.Rule == "binary-added" && ev.Hit.File == "asset.bin" {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("expected a guardrail.tripped event for the new binary-added hit, got %+v", events)
	}

	// Re-scanning with nothing changed must not re-fire the same (rule, file) hit.
	if err := e.RefreshOne(context.Background(), wt); err != nil {
		t.Fatal(err)
	}
	if ev := guardrailEvents(drainEvents(sub, 200*time.Millisecond)); len(ev) != 0 {
		t.Errorf("re-scanning an unchanged hit must not re-fire it, got %+v", ev)
	}
}

// TestFirstScanErrorDoesNotOpenColdStartGate is the MINOR-1 fix pin: the old
// code marked firstScanDone via an unconditional `defer`, so a discovery
// failure or an early ctx-cancel on the very first Refresh still opened the
// cold-start gate — the FOLLOWING successful scan would then republish every
// standing hit as "new" (the exact restart storm the gate exists to
// prevent). An already-canceled context makes Refresh's own per-repo loop
// return ctx.Err() on its very first iteration, deterministically
// reproducing "the first Refresh errors" without needing a real
// discovery-layer failure.
func TestFirstScanErrorDoesNotOpenColdStartGate(t *testing.T) {
	root := buildWorkspace(t)
	e := newEngine(t, root)

	sub, cancel := e.Registry().Subscribe(64)
	defer cancel()

	ctx, cancelCtx := context.WithCancel(context.Background())
	cancelCtx() // already canceled: Refresh's loop must see ctx.Err() != nil on its first repo
	if err := e.Refresh(ctx); err == nil {
		t.Fatal("expected the first Refresh to fail against an already-canceled context")
	}

	// The gate must still be closed: a normal, successful scan right after
	// must suppress buildWorkspace's standing hits exactly like a genuine
	// first scan would, not replay them as new.
	if err := e.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	feat := findByBranch(e.List(), "feature")
	if feat == nil || len(feat.Guardrails) == 0 {
		t.Fatal("precondition: buildWorkspace's feature worktree should have standing guardrail hits")
	}
	if ev := guardrailEvents(drainEvents(sub, 200*time.Millisecond)); len(ev) != 0 {
		t.Fatalf("a failed first Refresh must not open the cold-start gate: the next successful scan republished standing hits as new: %+v", ev)
	}
}

// TestGuardrailTrippedRefiresAfterHitClearsThenRecurs: a (rule, file) hit
// that disappears (the file is fixed) and later reappears (the same file is
// broken again) must publish a fresh guardrail.tripped the second time —
// publishNewGuardrailHits only compares against the IMMEDIATELY PRECEDING
// scan's hit set (meta.hits), not "have we ever seen this key", so clearing a
// hit must reset it as "new" if it recurs.
func TestGuardrailTrippedRefiresAfterHitClearsThenRecurs(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	os.MkdirAll(repo, 0o755)
	git(t, repo, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(repo, "app.go"), []byte("package app\n"), 0o644)
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-q", "-m", "init")

	wtPath := filepath.Join(root, "repo-feature")
	git(t, repo, "worktree", "add", "-q", "-b", "feature", wtPath)
	secret := fakeAWSKeyID
	os.WriteFile(filepath.Join(wtPath, "config.go"), []byte("package app\n\nvar k = \""+secret+"\"\n"), 0o644)

	e := newEngine(t, root)
	sub, cancel := e.Registry().Subscribe(64)
	defer cancel()

	if err := e.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	drainEvents(sub, 200*time.Millisecond) // cold-start gate suppresses the first scan

	// Clear the hit: rewrite config.go with no secret. This must not itself
	// publish anything (the hit going away isn't a "new hit" appearing).
	os.WriteFile(filepath.Join(wtPath, "config.go"), []byte("package app\n\nvar k = \"clean\"\n"), 0o644)
	if err := e.RefreshOne(context.Background(), wtPath); err != nil {
		t.Fatal(err)
	}
	if ev := guardrailEvents(drainEvents(sub, 200*time.Millisecond)); len(ev) != 0 {
		t.Fatalf("a hit clearing must not itself publish a guardrail.tripped event, got %+v", ev)
	}

	// Recur: put the exact same secret back in the exact same file. Since the
	// hit was absent from the immediately-preceding scan, this must fire again.
	os.WriteFile(filepath.Join(wtPath, "config.go"), []byte("package app\n\nvar k = \""+secret+"\"\n"), 0o644)
	if err := e.RefreshOne(context.Background(), wtPath); err != nil {
		t.Fatal(err)
	}
	events := guardrailEvents(drainEvents(sub, time.Second))
	var saw bool
	for _, ev := range events {
		if ev.Hit != nil && ev.Hit.Rule == "secrets-pattern" && ev.Hit.File == "config.go" {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("a hit that cleared and then recurred must re-fire guardrail.tripped, got %+v", events)
	}
}

// TestGuardrailTrippedKeyedByRuleAndFileTwoFilesSameRuleAreTwoEvents: two
// different files tripping the SAME rule in one scan must publish two
// distinct guardrail.tripped events (hitKey includes file, not just rule).
func TestGuardrailTrippedKeyedByRuleAndFileTwoFilesSameRuleAreTwoEvents(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	os.MkdirAll(filepath.Join(repo, "migrations"), 0o755)
	git(t, repo, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(repo, "app.go"), []byte("package app\n"), 0o644)
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-q", "-m", "init")

	wtPath := filepath.Join(root, "repo-feature")
	git(t, repo, "worktree", "add", "-q", "-b", "feature", wtPath)

	e := newEngine(t, root)
	sub, cancel := e.Registry().Subscribe(64)
	defer cancel()

	if err := e.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	drainEvents(sub, 200*time.Millisecond) // cold-start gate

	// Two NEW files under migrations/ in the SAME scan: both trip
	// touches-migrations, at two distinct (rule, file) keys.
	os.MkdirAll(filepath.Join(wtPath, "migrations"), 0o755)
	os.WriteFile(filepath.Join(wtPath, "migrations", "001.sql"), []byte("create table a();\n"), 0o644)
	os.WriteFile(filepath.Join(wtPath, "migrations", "002.sql"), []byte("create table b();\n"), 0o644)
	if err := e.RefreshOne(context.Background(), wtPath); err != nil {
		t.Fatal(err)
	}

	events := guardrailEvents(drainEvents(sub, time.Second))
	var saw001, saw002 bool
	for _, ev := range events {
		if ev.Hit == nil || ev.Hit.Rule != "touches-migrations" {
			continue
		}
		switch ev.Hit.File {
		case "migrations/001.sql":
			saw001 = true
		case "migrations/002.sql":
			saw002 = true
		}
	}
	if !saw001 || !saw002 {
		t.Fatalf("two files tripping the same rule in one scan must publish two distinct events, got %+v", events)
	}
	if len(events) != 2 {
		t.Errorf("expected exactly 2 guardrail.tripped events (one per file), got %d: %+v", len(events), events)
	}
}

// TestGuardrailTrippedNotKeyedOnLine: a hit's Line moving (an edit above an
// existing match) must NOT re-trip the same (rule, file) pair.
func TestGuardrailTrippedNotKeyedOnLine(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	os.MkdirAll(repo, 0o755)
	git(t, repo, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(repo, "app.go"), []byte("package app\n"), 0o644)
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-q", "-m", "init")

	wtPath := filepath.Join(root, "repo-feature")
	git(t, repo, "worktree", "add", "-q", "-b", "feature", wtPath)
	secret := fakeAWSKeyID
	os.WriteFile(filepath.Join(wtPath, "config.go"), []byte("package app\n\nvar k = \""+secret+"\"\n"), 0o644)

	e := newEngine(t, root)
	sub, cancel := e.Registry().Subscribe(64)
	defer cancel()

	if err := e.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	// First scan: cold-start gate suppresses everything.
	drainEvents(sub, 200*time.Millisecond)

	// Prepend a blank line, moving the match down by one line — the (rule,
	// file) key is unchanged, so this must NOT re-trip.
	os.WriteFile(filepath.Join(wtPath, "config.go"), []byte("package app\n\nvar _ = 0\nvar k = \""+secret+"\"\n"), 0o644)
	if err := e.RefreshOne(context.Background(), wtPath); err != nil {
		t.Fatal(err)
	}
	if ev := guardrailEvents(drainEvents(sub, 200*time.Millisecond)); len(ev) != 0 {
		t.Errorf("a hit whose Line moved but (rule, file) stayed the same must not re-trip, got %+v", ev)
	}
}
