package engine

import (
	"context"
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

	if err := e.SetReviewed(feat.ID, "new.go", true); err != nil {
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

func TestReviewResetsWhenDiffChanges(t *testing.T) {
	root := buildWorkspace(t)
	e := newEngine(t, root)
	e.Refresh(context.Background())
	feat := findByBranch(e.List(), "feature")
	e.SetReviewed(feat.ID, "new.go", true)

	// Change the diff: edit app.go further in the worktree.
	wt := filepath.Join(root, "api-server-feature")
	os.WriteFile(filepath.Join(wt, "app.go"), []byte("package api\n\nfunc A() {}\nfunc B() {}\nfunc D() {}\n"), 0o644)
	e.Refresh(context.Background())

	got := findByBranch(e.List(), "feature")
	if got.Reviewed != 0 {
		t.Errorf("review state should reset when the diff changes, got reviewed=%d", got.Reviewed)
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
	git(t, wt, "add", ".")
	git(t, wt, "commit", "-q", "-m", "feature work")
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
		if err := e.SetReviewed(feat.ID, f.Path, true); err != nil {
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
		e.SetReviewed(feat.ID, f.Path, true)
	}
	_, err := e.Approve(feat.ID)
	if err == nil {
		t.Fatal("approve should refuse a dirty worktree")
	}
	if !strings.Contains(err.Error(), "uncommitted") {
		t.Errorf("error should mention uncommitted changes, got: %v", err)
	}
}
