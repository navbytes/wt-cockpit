package gitbackend

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitEnv gives deterministic, isolated git behaviour in tests.
func gitEnv() []string {
	return append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	)
}

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = gitEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// newRepoWithWorktree builds a repo on branch main with one commit, then adds a
// worktree on branch feature that modifies and adds files. Returns repo + worktree paths.
func newRepoWithWorktree(t *testing.T) (repo, wt string) {
	t.Helper()
	root := t.TempDir()
	repo = filepath.Join(root, "proj")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, repo, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(repo, "app.go"), []byte("package app\n\nfunc A() {}\n"), 0o644)
	os.WriteFile(filepath.Join(repo, "README.md"), []byte("# proj\n"), 0o644)
	run(t, repo, "add", ".")
	run(t, repo, "commit", "-q", "-m", "init")

	wt = filepath.Join(root, "proj-feature")
	run(t, repo, "worktree", "add", "-q", "-b", "feature", wt)
	// Modify an existing file and add a new one in the worktree.
	os.WriteFile(filepath.Join(wt, "app.go"), []byte("package app\n\nfunc A() {}\nfunc B() {}\n"), 0o644)
	os.WriteFile(filepath.Join(wt, "new.go"), []byte("package app\n\nfunc C() {}\n"), 0o644)
	return repo, wt
}

func TestListWorktrees(t *testing.T) {
	repo, wt := newRepoWithWorktree(t)
	b := NewCLI()
	refs, err := b.ListWorktrees(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 {
		t.Fatalf("want 2 worktrees (main + feature), got %d: %+v", len(refs), refs)
	}
	var main, feat *WorktreeRef
	for i := range refs {
		switch refs[i].Branch {
		case "main":
			main = &refs[i]
		case "feature":
			feat = &refs[i]
		}
	}
	if main == nil || feat == nil {
		t.Fatalf("missing branches in %+v", refs)
	}
	if !sameFile(t, main.Path, repo) {
		t.Errorf("main worktree path = %q, want %q", main.Path, repo)
	}
	if !sameFile(t, feat.Path, wt) {
		t.Errorf("feature worktree path = %q, want %q", feat.Path, wt)
	}
	if !main.IsMain {
		t.Errorf("first worktree should be flagged IsMain")
	}
}

func TestCurrentBranch(t *testing.T) {
	_, wt := newRepoWithWorktree(t)
	b := NewCLI()
	br, err := b.CurrentBranch(wt)
	if err != nil {
		t.Fatal(err)
	}
	if br != "feature" {
		t.Errorf("branch = %q, want feature", br)
	}
}

func TestDiffAgainstBaseIncludesCommittedAndUncommitted(t *testing.T) {
	_, wt := newRepoWithWorktree(t)
	// Commit one change in the worktree, leave another uncommitted.
	run(t, wt, "add", "new.go")
	run(t, wt, "commit", "-q", "-m", "add C")
	// app.go modification stays uncommitted.

	b := NewCLI()
	diff, err := b.DiffAgainstBase(wt, "main")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff, "new.go") {
		t.Errorf("diff should include committed new.go:\n%s", diff)
	}
	if !strings.Contains(diff, "func B()") {
		t.Errorf("diff should include uncommitted func B change:\n%s", diff)
	}
}

func TestIsDirty(t *testing.T) {
	repo, wt := newRepoWithWorktree(t)
	b := NewCLI()
	dirty, err := b.IsDirty(wt)
	if err != nil {
		t.Fatal(err)
	}
	if !dirty {
		t.Error("worktree with uncommitted edits should be dirty")
	}
	clean, err := b.IsDirty(repo)
	if err != nil {
		t.Fatal(err)
	}
	if clean {
		t.Error("untouched main repo should be clean")
	}
}

func TestDefaultBranch(t *testing.T) {
	repo, _ := newRepoWithWorktree(t)
	b := NewCLI()
	def, err := b.DefaultBranch(repo)
	if err != nil {
		t.Fatal(err)
	}
	if def != "main" {
		t.Errorf("default branch = %q, want main", def)
	}
}

func sameFile(t *testing.T, a, b string) bool {
	t.Helper()
	ra, _ := filepath.EvalSymlinks(a)
	rb, _ := filepath.EvalSymlinks(b)
	return ra == rb
}

// testCLI returns a CLI backend with deterministic, isolated git identity/env so
// merge commits succeed in the sandbox.
func testCLI() *CLI { return &CLI{gitPath: "git", env: gitEnv()} }

func TestMergeFastForward(t *testing.T) {
	repo, wt := newRepoWithWorktree(t)
	// Commit the worktree's changes so the feature branch is ahead of main.
	run(t, wt, "add", ".")
	run(t, wt, "commit", "-q", "-m", "feature work")

	b := testCLI()
	// main is checked out in the repo (main) worktree; merge feature into it there.
	if err := b.Merge(repo, "feature"); err != nil {
		t.Fatalf("merge: %v", err)
	}
	// main should now contain func B and new.go.
	got, _ := os.ReadFile(filepath.Join(repo, "app.go"))
	if !strings.Contains(string(got), "func B()") {
		t.Errorf("main app.go missing merged change:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(repo, "new.go")); err != nil {
		t.Errorf("main missing merged new.go: %v", err)
	}
}

func TestMergeAbortsOnConflict(t *testing.T) {
	repo, wt := newRepoWithWorktree(t)
	// Create a conflicting edit to the SAME line of app.go on both branches.
	os.WriteFile(filepath.Join(repo, "app.go"), []byte("package app\n\nfunc A() {}\nfunc MAIN() {}\n"), 0o644)
	run(t, repo, "commit", "-qam", "main edit")
	os.WriteFile(filepath.Join(wt, "app.go"), []byte("package app\n\nfunc A() {}\nfunc FEAT() {}\n"), 0o644)
	run(t, wt, "commit", "-qam", "feat edit")

	b := testCLI()
	err := b.Merge(repo, "feature")
	if err == nil {
		t.Fatal("expected conflict error")
	}
	// P7-ux.md P1-2: a real conflict must be distinguishable from any other
	// merge failure via errors.Is, so engine.Approve can give the human a
	// clear message instead of relaying git's raw exit-status text (git
	// writes "CONFLICT ..." to stdout, not stderr, so the ordinary stderr-only
	// GitError wrapping never even saw it before this fix).
	if !errors.Is(err, ErrMergeConflict) {
		t.Errorf("expected errors.Is(err, ErrMergeConflict), got %v", err)
	}
	// Critically, the base worktree must be left CLEAN (merge aborted), not stuck
	// mid-conflict with markers.
	dirty, _ := b.IsDirty(repo)
	if dirty {
		t.Error("base worktree should be clean after a failed/aborted merge")
	}
	got, _ := os.ReadFile(filepath.Join(repo, "app.go"))
	if strings.Contains(string(got), "<<<<<<<") {
		t.Error("conflict markers left in base worktree")
	}
}

func TestRemoveWorktree(t *testing.T) {
	repo, wt := newRepoWithWorktree(t)
	b := testCLI()
	if err := b.RemoveWorktree(repo, wt); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Errorf("worktree dir should be gone, stat err = %v", err)
	}
	refs, _ := b.ListWorktrees(repo)
	for _, r := range refs {
		if r.Branch == "feature" {
			t.Errorf("feature worktree still listed after removal: %+v", refs)
		}
	}
}
