package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/navbytes/wt-cockpit/internal/engine"
	"github.com/navbytes/wt-cockpit/internal/gitbackend"
	"github.com/navbytes/wt-cockpit/internal/guardrail"
	"github.com/navbytes/wt-cockpit/internal/model"
	"github.com/navbytes/wt-cockpit/internal/registry"
	"github.com/navbytes/wt-cockpit/internal/store"
)

// This file exercises web.New's page/fragment rendering (index, room stub,
// worktrees fragment) plus the fully-composed handler's security posture
// with a minimal stand-in API handler. The bypass matrix against the real,
// production api mux (cmd/wtd's server.routes()) lives in cmd/wtd's own test
// suite, where that mux actually is — internal/web cannot import a "main"
// package. See middleware_test.go for the middleware-unit-level matrix.

// mustResolver builds a guardrail.Resolver over rules with no per-repo packs
// in play — this package's test-only equivalent of the old guardrail.New
// (test helpers aren't importable across packages, so this is re-declared
// per-package like every other test helper in this repo).
func mustResolver(t testing.TB, rules []guardrail.Rule) *guardrail.Resolver {
	t.Helper()
	r, err := guardrail.NewResolver(rules, "default")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// testGit/testGitEnv mirror the identical helpers in cmd/wtd/integration_test.go
// (test helpers aren't importable across packages, so this is re-declared
// locally per that existing repo convention).
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

// buildTestEngine wires a real *engine.Engine over a real temp git repo with
// one worktree with a pending change, same fixture shape as cmd/wtd's
// buildTestServer.
func buildTestEngine(t *testing.T) (*engine.Engine, model.Worktree) {
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
	gr := mustResolver(t, guardrail.DefaultRules())
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
	return eng, *feat
}

// stubAPI stands in for cmd/wtd's server.routes(): enough of the same shape
// (a couple of GET/POST routes) to exercise New's mounting and the security
// stack without depending on the real (unimportable) "main" package mux.
func stubAPI() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/version", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"protocol":1}`))
	})
	mux.HandleFunc("/api/refresh", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true}`))
	})
	return mux
}

func buildTestApp(t *testing.T) (http.Handler, model.Worktree, Config) {
	t.Helper()
	eng, feat := buildTestEngine(t)
	cfg := Config{BoundAddr: testBoundAddr, CSRFToken: "test-token", Roots: []string{"/roots/example"}}
	return New(eng, stubAPI(), cfg), feat, cfg
}

func getPage(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Host = testBoundAddr
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// ---- index page ----

func TestIndexPageContainsFixtureWorktreeName(t *testing.T) {
	h, feat, _ := buildTestApp(t)
	rec := getPage(t, h, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), feat.Name) {
		t.Errorf("index page body does not contain the fixture worktree name %q:\n%s", feat.Name, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `href="/wt/`+feat.ID+`"`) {
		t.Errorf("index page should link to /wt/%s:\n%s", feat.ID, rec.Body.String())
	}
	// P4-fixes.md #6: the SSE-drop indicator lives in the topbar on both the
	// index and room pages (app.js's EventSource onerror unhides it).
	if !strings.Contains(rec.Body.String(), `id="sse-chip" class="sse-chip hidden"`) {
		t.Errorf("expected the (initially hidden) SSE-drop chip in the topbar, got:\n%s", rec.Body.String())
	}
}

// TestIndexPageEmptyWorkspaceMessage pins the "no worktrees" state (§2):
// an engine with nothing tracked renders the friendly empty message naming
// the configured roots, not a blank list.
func TestIndexPageEmptyWorkspaceMessage(t *testing.T) {
	root := t.TempDir() // no git repos at all under it
	reg := registry.New()
	st, err := store.OpenJSON(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	be := gitbackend.NewCLIWithEnv(testGitEnv())
	gr := mustResolver(t, guardrail.DefaultRules())
	eng := engine.New(engine.Config{Roots: []string{root}, ActivityWindow: 30 * time.Second}, be, reg, st, gr)
	if err := eng.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	h := New(eng, stubAPI(), Config{BoundAddr: testBoundAddr, CSRFToken: "tok", Roots: []string{root}})
	rec := getPage(t, h, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no worktrees under") {
		t.Errorf("expected the empty-workspace message, got:\n%s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), root) {
		t.Errorf("expected the empty-workspace message to name the configured root %q, got:\n%s", root, rec.Body.String())
	}
}

// TestIndexPageEmbedsCSRFMetaTag pins the surfacing mechanism (§1.3): the
// process token must appear verbatim in a <meta name="csrf-token"> on every
// rendered page.
func TestIndexPageEmbedsCSRFMetaTag(t *testing.T) {
	h, _, cfg := buildTestApp(t)
	rec := getPage(t, h, "/")
	tok := scrapeCSRFToken(rec.Body.String())
	if tok == "" {
		t.Fatalf("no <meta name=\"csrf-token\"> found in:\n%s", rec.Body.String())
	}
	if tok != cfg.CSRFToken {
		t.Errorf("scraped csrf token = %q, want %q", tok, cfg.CSRFToken)
	}
}

// ---- room: see room_test.go for the full WP3 suite (side-by-side rendering,
// escaping torture, collapse/expand, 404/empty states, review/approve) ----

// TestRoomHandlerRendersFixtureWorktree pins the smoke-level contract this
// file's buildTestApp fixture gives every other page/fragment test: a known
// id renders 200 with the fixture's own file in it, an unknown id 404s. The
// deep room-rendering behavior lives in room_test.go, close to sxs/highlight.
func TestRoomHandlerRendersFixtureWorktree(t *testing.T) {
	h, feat, _ := buildTestApp(t)

	rec := getPage(t, h, "/wt/"+feat.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("known id: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "app.go") {
		t.Errorf("expected the fixture's changed file in the room page, got:\n%s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `id="sse-chip" class="sse-chip hidden"`) {
		t.Errorf("expected the (initially hidden) SSE-drop chip in the topbar, got:\n%s", rec.Body.String())
	}

	rec = getPage(t, h, "/wt/definitely-unknown")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown id: status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "definitely-unknown") {
		t.Errorf("expected the unknown id echoed in the 404 page, got:\n%s", rec.Body.String())
	}
}

// ---- worktrees fragment ----

func TestFragmentWorktreesReturnsBareListReflectingFixture(t *testing.T) {
	h, feat, _ := buildTestApp(t)
	rec := getPage(t, h, "/fragment/worktrees")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "<html") || strings.Contains(body, "<!doctype") {
		t.Errorf("fragment must not include the page shell, got:\n%s", body)
	}
	if !strings.Contains(body, feat.Name) {
		t.Errorf("fragment should contain the fixture worktree name %q:\n%s", feat.Name, body)
	}
}

// ---- static assets ----

func TestStaticAssetsServedWithContentAndSecurityHeaders(t *testing.T) {
	h, _, _ := buildTestApp(t)
	for _, path := range []string{"/static/style.css", "/static/app.js"} {
		rec := getPage(t, h, path)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", path, rec.Code)
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s: Cache-Control = %q, want no-store (assets included, §1.3)", path, got)
		}
		if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s: X-Content-Type-Options = %q, want nosniff", path, got)
		}
	}
}

// TestStaticAssetsAlsoRefuseWrongHost pins that the Host allowlist applies
// to every web-listener route, not just pages/api.
func TestStaticAssetsAlsoRefuseWrongHost(t *testing.T) {
	h, _, _ := buildTestApp(t)
	req := httptest.NewRequest(http.MethodGet, "/static/style.css", nil)
	req.Host = "evil.com"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 for a bad Host on a static asset; body=%s", rec.Code, rec.Body.String())
	}
}

// ---- helpers ----

var csrfMetaRE = regexp.MustCompile(`<meta name="csrf-token" content="([^"]*)">`)

func scrapeCSRFToken(html string) string {
	m := csrfMetaRE.FindStringSubmatch(html)
	if m == nil {
		return ""
	}
	return m[1]
}
