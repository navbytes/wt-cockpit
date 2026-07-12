package discovery

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"
)

func gitInit(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "init", "-q", "-b", "main")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
}

func TestDiscoverReposFindsMainReposWithLang(t *testing.T) {
	root := t.TempDir()

	goRepo := filepath.Join(root, "api-server")
	gitInit(t, goRepo)
	os.WriteFile(filepath.Join(goRepo, "go.mod"), []byte("module x\n"), 0o644)

	tsRepo := filepath.Join(root, "web", "dashboard") // nested one level
	gitInit(t, tsRepo)
	os.WriteFile(filepath.Join(tsRepo, "package.json"), []byte("{}"), 0o644)

	// A directory that is NOT a repo, containing a heavy dir we must skip.
	junk := filepath.Join(root, "notarepo", "node_modules", "pkg")
	os.MkdirAll(filepath.Join(junk, ".git"), 0o755) // fake .git inside node_modules

	repos, err := DiscoverRepos([]string{root}, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 2 {
		t.Fatalf("want 2 repos, got %d: %+v", len(repos), repos)
	}
	sort.Slice(repos, func(i, j int) bool { return repos[i].Name < repos[j].Name })

	if repos[0].Name != "api-server" || repos[0].Lang != "Go" {
		t.Errorf("repo0 = %+v, want api-server/Go", repos[0])
	}
	if repos[1].Name != "dashboard" || repos[1].Lang != "TS/JS" {
		t.Errorf("repo1 = %+v, want dashboard/TS-JS", repos[1])
	}
}

func TestDiscoverSkipsLinkedWorktrees(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "proj")
	gitInit(t, repo)
	os.WriteFile(filepath.Join(repo, "f.txt"), []byte("hi\n"), 0o644)
	commit(t, repo)

	// Add a linked worktree: it has a .git *file*, not directory, so it must not
	// be discovered as a separate repo.
	wt := filepath.Join(root, "proj-feat")
	gitCmd(t, repo, "worktree", "add", "-q", "-b", "feat", wt)

	repos, err := DiscoverRepos([]string{root}, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 1 {
		t.Fatalf("linked worktree should not count as a repo; want 1 got %d: %+v", len(repos), repos)
	}
	if repos[0].Name != "proj" {
		t.Errorf("repo = %q", repos[0].Name)
	}
}

func commit(t *testing.T, dir string) {
	gitCmd(t, dir, "add", ".")
	gitCmd(t, dir, "commit", "-q", "-m", "c")
}

func gitCmd(t *testing.T, dir string, args ...string) {
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
