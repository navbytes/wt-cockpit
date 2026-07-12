package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/navbytes/wt-cockpit/internal/config"
	"github.com/navbytes/wt-cockpit/internal/engine"
	"github.com/navbytes/wt-cockpit/internal/gitbackend"
	"github.com/navbytes/wt-cockpit/internal/guardrail"
	"github.com/navbytes/wt-cockpit/internal/model"
	"github.com/navbytes/wt-cockpit/internal/registry"
	"github.com/navbytes/wt-cockpit/internal/store"
)

// These test the merge logic directly (per the design note: testing flag.Parse
// itself is awkward, since flag.CommandLine is a package-level global).

func TestMergeSettingExplicitFlagWinsOverConfig(t *testing.T) {
	got := mergeSetting("Y", true, "X")
	if got != "Y" {
		t.Errorf("got %q, want the explicit flag value Y", got)
	}
}

func TestMergeSettingConfigWinsOverBuiltinDefaultWhenNotExplicit(t *testing.T) {
	got := mergeSetting("builtin-default", false, "from-config")
	if got != "from-config" {
		t.Errorf("got %q, want from-config", got)
	}
}

func TestMergeSettingFallsBackToBuiltinDefaultWhenConfigUnset(t *testing.T) {
	got := mergeSetting("builtin-default", false, "")
	if got != "builtin-default" {
		t.Errorf("got %q, want builtin-default", got)
	}
}

func TestMergeSettingWorksForDuration(t *testing.T) {
	if got := mergeSetting(2*time.Second, false, 5*time.Second); got != 5*time.Second {
		t.Errorf("got %v, want config's 5s", got)
	}
	if got := mergeSetting(2*time.Second, true, 5*time.Second); got != 2*time.Second {
		t.Errorf("got %v, want the explicit flag's 2s", got)
	}
}

func TestMergeRootsExplicitFlagReplacesConfigEntirely(t *testing.T) {
	got := mergeRoots([]string{"/cli/root"}, true, []string{"/cfg/a", "/cfg/b"})
	if len(got) != 1 || got[0] != "/cli/root" {
		t.Errorf("got %v, want just [/cli/root] (no merging with config roots)", got)
	}
}

func TestMergeRootsUsesConfigWhenFlagNotExplicit(t *testing.T) {
	got := mergeRoots(nil, false, []string{"/cfg/a", "/cfg/b"})
	if len(got) != 2 || got[0] != "/cfg/a" || got[1] != "/cfg/b" {
		t.Errorf("got %v, want the config roots", got)
	}
}

func TestMergeRootsFallsBackToFlagDefaultWhenNeitherSet(t *testing.T) {
	got := mergeRoots([]string{"/cwd"}, false, nil)
	if len(got) != 1 || got[0] != "/cwd" {
		t.Errorf("got %v, want the flag's own default", got)
	}
}

func TestBaseForBuildsMapFromConfigRepos(t *testing.T) {
	cfg := config.Config{Repos: map[string]config.RepoConfig{
		"/repos/api":    {Base: "develop"},
		"/repos/ignore": {}, // no override set -> must be excluded
	}}
	got := baseFor(cfg)
	if got["/repos/api"] != "develop" {
		t.Errorf(`baseFor["/repos/api"] = %q, want "develop"`, got["/repos/api"])
	}
	if _, ok := got["/repos/ignore"]; ok {
		t.Errorf("repo with no base override should be excluded, got %+v", got)
	}
}

// TestBaseForCleansTrailingSlashPath: discovery.DiscoverRepos always produces
// repo.Path via filepath.Join (which never leaves a trailing slash), so a
// config key with a trailing slash would silently never match unless baseFor
// normalizes it too. It does (filepath.Clean on the key) — this pins that
// behavior rather than assuming it, per the brief's "test actual behavior"
// instruction for this exact edge.
func TestBaseForCleansTrailingSlashPath(t *testing.T) {
	cfg := config.Config{Repos: map[string]config.RepoConfig{
		"/repos/api-server/": {Base: "develop"},
	}}
	got := baseFor(cfg)
	if got["/repos/api-server"] != "develop" {
		t.Errorf(`baseFor should clean a trailing-slash config key so it matches repo.Path ("/repos/api-server") exactly; got %+v`, got)
	}
}

// TestBaseForExpandsHomeAndMakesKeyAbsolute is MN1: discovery.DiscoverRepos
// resolves each root with ~-expansion + filepath.Abs before comparing it to
// any config key, so a `[repos."~/code/x"]` override must be expanded and
// made absolute the exact same way, or it silently never matches repo.Path
// and the override just no-ops.
func TestBaseForExpandsHomeAndMakesKeyAbsolute(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfg := config.Config{Repos: map[string]config.RepoConfig{
		"~/code/api-server": {Base: "develop"},
	}}
	got := baseFor(cfg)
	want := filepath.Join(home, "code", "api-server")
	if got[want] != "develop" {
		t.Errorf(`baseFor should expand "~" against $HOME and produce an absolute key %q, got %+v`, want, got)
	}
}

// TestMergeSettingZeroConfigIntervalTreatedAsUnset documents the interval-0s
// edge from the config brief: mergeSetting's "cfgVal != zero" check can't
// distinguish "config explicitly set interval to 0s" from "config never
// mentioned interval" — both look like the Duration zero value, so both fall
// back to the flag's own value. This is the generic mergeSetting contract,
// pinned specifically for the field named in the brief.
func TestMergeSettingZeroConfigIntervalTreatedAsUnset(t *testing.T) {
	got := mergeSetting(2*time.Second, false, 0*time.Second)
	if got != 2*time.Second {
		t.Errorf("got %v, want the flag default 2s (a 0s config interval must be indistinguishable from unset)", got)
	}
}

// TestMergeSettingNegativeConfigIntervalPassesThrough: unlike 0, a negative
// config interval is nonzero, so mergeSetting has no reason to reject it —
// it passes straight through unvalidated. The final safety net against a
// non-positive time.NewTicker duration lives in the watcher package (Poller
// and FSWatcher both default on `interval <= 0`), not here.
func TestMergeSettingNegativeConfigIntervalPassesThrough(t *testing.T) {
	got := mergeSetting(2*time.Second, false, -5*time.Second)
	if got != -5*time.Second {
		t.Errorf("got %v, want the negative config value passed through as-is (-5s)", got)
	}
}

// TestClampIntervalRaisesSubSecondPositiveValueToOneSecond is NT2: a tiny but
// positive user-supplied interval (e.g. a "10ms" config/flag typo) would peg
// a CPU core on a full-rescan poll/reconciliation loop, so cmd/wtd (only —
// not the watcher constructors themselves, which tests deliberately build
// with sub-second intervals for speed) clamps it up to 1s.
func TestClampIntervalRaisesSubSecondPositiveValueToOneSecond(t *testing.T) {
	if got := clampInterval(10 * time.Millisecond); got != time.Second {
		t.Errorf("got %v, want the 1s floor", got)
	}
}

// TestClampIntervalLeavesOneSecondOrMoreUntouched: a valid interval already
// at or above the floor must pass through unchanged.
func TestClampIntervalLeavesOneSecondOrMoreUntouched(t *testing.T) {
	if got := clampInterval(2 * time.Second); got != 2*time.Second {
		t.Errorf("got %v, want 2s unchanged", got)
	}
	if got := clampInterval(time.Second); got != time.Second {
		t.Errorf("got %v, want 1s unchanged", got)
	}
}

// TestClampIntervalLeavesZeroAndNegativeUntouched: 0/negative already have
// their own documented fallback further downstream (Poller and FSWatcher both
// default on interval<=0) — clampInterval must not interfere with that by
// e.g. bumping a negative value up to the 1s floor too.
func TestClampIntervalLeavesZeroAndNegativeUntouched(t *testing.T) {
	if got := clampInterval(0); got != 0 {
		t.Errorf("got %v, want 0 unchanged (its own <=0 default lives in the watcher package)", got)
	}
	if got := clampInterval(-5 * time.Second); got != -5*time.Second {
		t.Errorf("got %v, want -5s unchanged", got)
	}
}

// testGit runs git with a deterministic, isolated identity/config, mirroring
// the git() helpers in internal/engine and internal/watcher's test files
// (test helpers aren't importable across packages, so this is re-declared
// locally per that existing convention).
func testGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = testGitEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func testGitEnv() []string {
	return append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
}

// buildTestServer wires up a real *server (the exact type routes() and every
// handler hang off) over a real engine backed by a real temp git repo with
// one worktree with a pending change — no git or engine mocks, matching the
// rest of the repo's test conventions. It returns the server and the
// worktree the caller can exercise handlers against.
func buildTestServer(t *testing.T) (*server, model.Worktree) {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	testGit(t, repo, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(repo, "app.go"), []byte("package app\n"), 0o644)
	testGit(t, repo, "add", ".")
	testGit(t, repo, "commit", "-q", "-m", "init")

	wt := filepath.Join(root, "repo-feature")
	testGit(t, repo, "worktree", "add", "-q", "-b", "feature", wt)
	os.WriteFile(filepath.Join(wt, "app.go"), []byte("package app\n\nfunc B() {}\n"), 0o644)

	reg := registry.New()
	st, err := store.OpenJSON(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	be := gitbackend.NewCLIWithEnv(testGitEnv())
	gr := guardrail.New(guardrail.DefaultRules())
	eng := engine.New(engine.Config{
		Roots:          []string{root},
		MaxDepth:       4,
		ActivityWindow: 30 * time.Second,
	}, be, reg, st, gr)
	if err := eng.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	var feat *model.Worktree
	for _, w := range eng.List() {
		if w.Branch == "feature" {
			wc := w
			feat = &wc
		}
	}
	if feat == nil {
		t.Fatalf("feature worktree not found in %+v", eng.List())
	}
	return &server{eng: eng}, *feat
}

func postJSON(t *testing.T, handler http.Handler, path string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// TestHandleReviewReturns409ForStaleHash is the handler-level counterpart to
// engine.TestSetReviewedRejectsStaleExpectedHash, flagged as skipped-but-
// wanted in T1's handoff: a POST /api/review with a hash that no longer
// matches the file's current diff hash must surface as 409 Conflict over the
// real HTTP surface (the mux from routes()), not just at the engine level.
func TestHandleReviewReturns409ForStaleHash(t *testing.T) {
	srv, feat := buildTestServer(t)
	handler := srv.routes()

	rec := postJSON(t, handler, "/api/review", map[string]any{
		"id": feat.ID, "file": "app.go", "reviewed": true, "hash": "not-the-real-hash",
	})
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want %d (Conflict); body=%s", rec.Code, http.StatusConflict, rec.Body.String())
	}
}

// TestHandleReviewReturns404ForUnknownFile mirrors
// engine.TestSetReviewedRejectsUnknownFile at the HTTP layer.
func TestHandleReviewReturns404ForUnknownFile(t *testing.T) {
	srv, feat := buildTestServer(t)
	handler := srv.routes()

	rec := postJSON(t, handler, "/api/review", map[string]any{
		"id": feat.ID, "file": "does-not-exist.go", "reviewed": true,
	})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d (Not Found); body=%s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
}

// TestHandleReviewReturns200AndAppliesReview is the control case proving the
// two error-path tests above are actually distinguishing a real gate, not a
// handler that always fails: a correct hash succeeds (200) and is reflected
// in the engine's own state.
func TestHandleReviewReturns200AndAppliesReview(t *testing.T) {
	srv, feat := buildTestServer(t)
	handler := srv.routes()

	d, ok := srv.eng.Diff(feat.ID)
	if !ok {
		t.Fatal("diff not found")
	}
	var appHash string
	for _, f := range d.Files {
		if f.Path == "app.go" {
			appHash = f.Hash
		}
	}
	if appHash == "" {
		t.Fatal("precondition: app.go must be in the diff")
	}

	rec := postJSON(t, handler, "/api/review", map[string]any{
		"id": feat.ID, "file": "app.go", "reviewed": true, "hash": appHash,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	for _, w := range srv.eng.List() {
		if w.ID == feat.ID && w.Reviewed != 1 {
			t.Errorf("reviewed count = %d, want 1 after a successful review via the handler", w.Reviewed)
		}
	}
}
