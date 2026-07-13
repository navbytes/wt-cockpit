package discovery

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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

	repos, err := DiscoverRepos([]string{root}, 4, nil, nil)
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

	repos, err := DiscoverRepos([]string{root}, 4, nil, nil)
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

// fakeRepo creates a minimal repo marker (a .git directory, no real git
// repo) at dir — DiscoverRepos only ever os.Stats "<dir>/.git" to decide a
// directory is a repo, so this is enough and far cheaper than a real `git
// init` for a table test spanning several repos (mirrors the existing
// "notarepo/node_modules/pkg/.git" synthetic case above).
func fakeRepo(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestDiscoverReposIncludeExcludeFilter is the include/exclude table test:
// four repos whose basenames exercise the glob forms named in the brief
// (archive-*, ?tmp, [ab]x) plus one plain name, filtered every which way —
// empty/empty, include-only, exclude-only, and an overlapping include+
// exclude where exclude must win.
func TestDiscoverReposIncludeExcludeFilter(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"archive-2023", "atmp", "ax", "keep-me"} {
		fakeRepo(t, filepath.Join(root, name))
	}

	tests := []struct {
		name    string
		include []string
		exclude []string
		want    []string
	}{
		{
			name: "empty include and empty exclude admit everything",
			want: []string{"archive-2023", "atmp", "ax", "keep-me"},
		},
		{
			name:    "include-only keeps only what matches",
			include: []string{"keep-me"},
			want:    []string{"keep-me"},
		},
		{
			name:    "exclude-only drops matches, keeps the rest",
			exclude: []string{"archive-*", "?tmp", "[ab]x"},
			want:    []string{"keep-me"},
		},
		{
			name:    "exclude wins over an overlapping include",
			include: []string{"archive-*", "keep-me"},
			exclude: []string{"archive-*"},
			want:    []string{"keep-me"},
		},
		{
			name:    "include with no matches admits nothing",
			include: []string{"nonexistent-*"},
			want:    nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repos, err := DiscoverRepos([]string{root}, 4, tt.include, tt.exclude)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, r := range repos {
				got = append(got, r.Name)
			}
			sort.Strings(got)
			sort.Strings(tt.want)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("repos = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestDiscoverReposHandlesRepoNameContainingGlobMetacharacters is candidate
// gap #5 from the discovery-filter test brief: filepath.Match only ever
// interprets glob syntax ("[", "*", "?") from its PATTERN argument
// (include_repos/exclude_repos' own config-authored globs); anyGlobMatch
// passes the repo's folder BASENAME as the NAME argument, which Match
// compares structurally/literally and can never itself cause an error no
// matter what bytes the actual directory on disk happens to contain. This
// pins that a repo named with glob metacharacters neither crashes discovery
// nor gets mismatched by an unrelated pattern.
func TestDiscoverReposHandlesRepoNameContainingGlobMetacharacters(t *testing.T) {
	root := t.TempDir()
	fakeRepo(t, filepath.Join(root, "my[repo"))
	fakeRepo(t, filepath.Join(root, "plain-repo"))
	want := []string{"my[repo", "plain-repo"}

	// No filter at all: the walk itself must not error out over the odd name.
	repos, err := DiscoverRepos([]string{root}, 4, nil, nil)
	if err != nil {
		t.Fatalf("DiscoverRepos returned an error for a repo whose name contains glob metacharacters: %v", err)
	}
	var names []string
	for _, r := range repos {
		names = append(names, r.Name)
	}
	sort.Strings(names)
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("repos = %v, want %v", names, want)
	}

	// An ordinary wildcard include pattern must still match the odd name
	// literally: "*" matches any run of bytes in NAME, including a "[" that
	// would be special if it appeared in PATTERN instead.
	repos, err = DiscoverRepos([]string{root}, 4, []string{"my*"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 1 || repos[0].Name != "my[repo" {
		t.Errorf("include=[my*] = %+v, want just my[repo", repos)
	}

	// An unrelated exclude pattern must not accidentally catch it either.
	repos, err = DiscoverRepos([]string{root}, 4, nil, []string{"other-*"})
	if err != nil {
		t.Fatal(err)
	}
	names = nil
	for _, r := range repos {
		names = append(names, r.Name)
	}
	sort.Strings(names)
	if !reflect.DeepEqual(names, want) {
		t.Errorf("exclude=[other-*] should not affect either repo, got %v want %v", names, want)
	}
}

// ---- anyGlobMatch/repoPasses: fail-closed backstop for a match-time error ----
//
// config.Load's checkGlobSyntax rejects every syntactically invalid pattern
// before it ever reaches discovery, so filepath.Match erroring here should
// be unreachable in practice — these bypass that upstream validation by
// calling anyGlobMatch/repoPasses directly with a malformed pattern, to pin
// what happens if stdlib's own Match grammar ever drifts from what
// checkGlobSyntax mirrors: exclude must fail CLOSED (hide the repo), not
// open (silently leave a "should be hidden" repo visible).

// TestAnyGlobMatchFailsClosedOnMatchTimeError pins onErrorMatch's two
// directions directly: true (exclude's backstop) must count an errored
// pattern as a match; false (include's backstop) must not.
func TestAnyGlobMatchFailsClosedOnMatchTimeError(t *testing.T) {
	const badPattern = "[" // unterminated char class; config.Load would normally reject this
	if _, err := filepath.Match(badPattern, "x"); err == nil {
		t.Fatalf("test premise broken: %q no longer errors from filepath.Match", badPattern)
	}

	if !anyGlobMatch([]string{badPattern}, "x", true) {
		t.Error("onErrorMatch=true (exclude): an errored pattern must count as a match, so the repo is hidden")
	}
	if anyGlobMatch([]string{badPattern}, "x", false) {
		t.Error("onErrorMatch=false (include): an errored pattern must not count as a match")
	}
}

// TestRepoPassesFailsClosedWhenExcludePatternErrorsAtScanTime is the
// security-relevant direction: a malformed exclude pattern must hide the
// repo rather than silently leave it visible.
func TestRepoPassesFailsClosedWhenExcludePatternErrorsAtScanTime(t *testing.T) {
	if repoPasses("anything", nil, []string{"["}) {
		t.Error("a malformed exclude pattern must hide the repo (fail closed), not silently admit it")
	}
}

// TestRepoPassesFailsClosedWhenIncludePatternErrorsAtScanTime: an include
// list made entirely of malformed patterns must admit nothing.
func TestRepoPassesFailsClosedWhenIncludePatternErrorsAtScanTime(t *testing.T) {
	if repoPasses("anything", []string{"["}, nil) {
		t.Error("an include list of only malformed patterns must admit nothing")
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
