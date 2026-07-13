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
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
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

// TestIndexPageHasSensibleHeadingOutline pins ux-expert P2-5b: the index
// used to have zero headings at all — a screen reader's "jump to heading"
// navigation had nothing to land on. One <h1> (the brand, page-wide) and one
// <h2> per repo group is the minimum sensible outline.
func TestIndexPageHasSensibleHeadingOutline(t *testing.T) {
	h, feat, _ := buildTestApp(t)
	body := getPage(t, h, "/").Body.String()

	if n := strings.Count(body, "<h1"); n != 1 {
		t.Errorf("index page has %d <h1> elements, want exactly 1, got:\n%s", n, body)
	}
	if !strings.Contains(body, `<h1 class="brand">`) {
		t.Errorf("expected the brand as the page's <h1>, got:\n%s", body)
	}
	if !strings.Contains(body, `<h2 class="proj">`+feat.Repo+`</h2>`) {
		t.Errorf("expected the repo group as an <h2>, got:\n%s", body)
	}
}

// TestIndexPageEmptyWorkspaceMessage pins the "no worktrees" state (§2):
// an engine with nothing tracked renders the friendly empty message naming
// the configured roots, not a blank list.
func TestIndexPageEmptyWorkspaceMessage(t *testing.T) {
	root := t.TempDir() // no git repos at all under it
	reg := registry.New()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
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

// TestIndexPageHasProtoBanner pins the P3-cheap fix: app.js's protocol-
// mismatch handshake (connectEvents' "hello" listener) looks for
// #proto-banner and unhides it on a mismatch — the index used to have no
// such element at all, so a stale-index-after-a-wtd-upgrade reload prompt
// silently no-op'd there (room.tmpl already had it).
func TestIndexPageHasProtoBanner(t *testing.T) {
	h, _, _ := buildTestApp(t)
	body := getPage(t, h, "/").Body.String()
	if !strings.Contains(body, `id="proto-banner" class="banner hidden"`) {
		t.Errorf("expected the (initially hidden) proto-banner element on the index page, got:\n%s", body)
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

// ---- index worktree badge severity grading (ux-expert P1-1c) ----
//
// buildRepoEngine/buildRepoEngineWithRules/mustWriteFile come from
// room_test.go (same package): a fixture worktree whose populate func writes
// files that trip specific default guardrail rules.

// TestIndexBadgeGradesDangerForDangerHit pins that the index's worktree
// badge grades red for a danger-severity hit, matching the TUI sidebar badge
// and CLI radar (both already correct) instead of a fixed amber.
func TestIndexBadgeGradesDangerForDangerHit(t *testing.T) {
	eng, _ := buildRepoEngine(t, func(_, wt string) {
		mustWriteFile(t, filepath.Join(wt, "migrations", "014_drop.sql"), []byte("DROP TABLE x;\n"))
	})
	h := New(eng, stubAPI(), Config{BoundAddr: testBoundAddr, CSRFToken: "tok"})

	body := getPage(t, h, "/").Body.String()
	if !strings.Contains(body, `class="badge-alert danger"`) {
		t.Errorf("expected the danger-hit worktree's badge graded danger, got:\n%s", body)
	}
}

// TestIndexBadgeGradesWarnForWarnOnlyHit is the other half: a worktree
// tripping only a warn-severity rule must show the amber grade, not danger.
func TestIndexBadgeGradesWarnForWarnOnlyHit(t *testing.T) {
	eng, _ := buildRepoEngine(t, func(_, wt string) {
		mustWriteFile(t, filepath.Join(wt, "go.mod"), []byte("module x\n\ngo 1.22\n"))
	})
	h := New(eng, stubAPI(), Config{BoundAddr: testBoundAddr, CSRFToken: "tok"})

	body := getPage(t, h, "/").Body.String()
	if !strings.Contains(body, `class="badge-alert warn"`) {
		t.Errorf("expected the warn-only worktree's badge graded warn, got:\n%s", body)
	}
	if strings.Contains(body, `class="badge-alert danger"`) {
		t.Errorf("a warn-only worktree must not show a danger-graded badge, got:\n%s", body)
	}
}

// TestIndexBadgeGradesDangerForMixedWorktree pins the consistency-check's
// "mixed worktree" case at the worktree level: worst severity (danger) wins
// on the index badge even though the same worktree also has a warn hit.
func TestIndexBadgeGradesDangerForMixedWorktree(t *testing.T) {
	eng, _ := buildRepoEngine(t, func(_, wt string) {
		mustWriteFile(t, filepath.Join(wt, "go.mod"), []byte("module x\n\ngo 1.22\n"))
		mustWriteFile(t, filepath.Join(wt, "migrations", "014_drop.sql"), []byte("DROP TABLE x;\n"))
	})
	h := New(eng, stubAPI(), Config{BoundAddr: testBoundAddr, CSRFToken: "tok"})

	body := getPage(t, h, "/").Body.String()
	if !strings.Contains(body, `class="badge-alert danger"`) {
		t.Errorf("expected a mixed warn+danger worktree's badge graded danger (worst wins), got:\n%s", body)
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

// TestRoomPageHasSensibleHeadingOutline pins ux-expert P2-5b: the room page
// used to expose h4 ("Files in this worktree")/h3 (orphaned comments) with
// nothing above them — now there's exactly one <h1> (the brand) and the
// pane's own repo/name title is an <h2>, so the outline is h1 > h2 > h3 > h4.
func TestRoomPageHasSensibleHeadingOutline(t *testing.T) {
	h, feat, _ := buildTestApp(t)
	body := getPage(t, h, "/wt/"+feat.ID).Body.String()

	if n := strings.Count(body, "<h1"); n != 1 {
		t.Errorf("room page has %d <h1> elements, want exactly 1, got:\n%s", n, body)
	}
	if !strings.Contains(body, `<h1 class="brand">`) {
		t.Errorf("expected the brand as the page's <h1>, got:\n%s", body)
	}
	if !strings.Contains(body, `<h2 class="title">`+feat.Repo+" / "+feat.Name+`</h2>`) {
		t.Errorf("expected the pane's repo/name title as an <h2>, got:\n%s", body)
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
