package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
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

// This file is WP3's room-handler suite: the mock-faithful side-by-side
// rendering, its guardrails (collapse/expand, binary, highlight-off), the
// 404/empty states, the escaping torture matrix, and the review/approve
// CSRF plumbing. buildTestEngine/stubAPI/getPage/testBoundAddr etc. come
// from web_test.go (same package, shared test fixtures).

// buildRepoEngine wires a real *engine.Engine over a temp repo with one
// worktree, letting the caller populate the worktree's working tree
// (untracked files show up as pure "added" diffs — gitbackend's
// untrackedDiff, no need to commit or even `git add`) before Refresh runs.
// Rules default to guardrail.DefaultRules(); see buildRepoEngineWithRules for
// the parameterized variant (the guardrail-message severity/escaping tests
// need a custom rule no default one produces).
func buildRepoEngine(t *testing.T, populate func(repo, wt string)) (*engine.Engine, model.Worktree) {
	t.Helper()
	return buildRepoEngineWithRules(t, guardrail.DefaultRules(), populate)
}

// buildRepoEngineWithRules is buildRepoEngine's parameterized-rules variant.
func buildRepoEngineWithRules(t *testing.T, rules []guardrail.Rule, populate func(repo, wt string)) (*engine.Engine, model.Worktree) {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	testGit(t, repo, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "README"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	testGit(t, repo, "add", ".")
	testGit(t, repo, "commit", "-q", "-m", "init")

	wt := filepath.Join(root, "repo-feature")
	testGit(t, repo, "worktree", "add", "-q", "-b", "feature", wt)
	if populate != nil {
		populate(repo, wt)
	}

	reg := registry.New()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	be := gitbackend.NewCLIWithEnv(testGitEnv())
	gr := mustResolver(t, rules)
	eng := engine.New(engine.Config{Roots: []string{root}, MaxDepth: 4, ActivityWindow: 30 * time.Second}, be, reg, st, gr)
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

func mustWriteFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// ---- 404 / empty / binary / collapse-expand states ----

func TestRoomHandler404ForUnknownWorktree(t *testing.T) {
	h, _, _ := buildTestApp(t)
	rec := getPage(t, h, "/wt/no-such-id")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "no-such-id") || !strings.Contains(rec.Body.String(), "not found") {
		t.Errorf("expected a 404 page naming the unknown id, got:\n%s", rec.Body.String())
	}
	if got := rec.Header().Get("Content-Security-Policy"); got == "" {
		t.Error("expected the CSP header on the 404 page too")
	}
}

func TestRoomHandlerEmptyDiffCard(t *testing.T) {
	eng, feat := buildRepoEngine(t, nil) // no changes at all vs base
	h := New(eng, stubAPI(), Config{BoundAddr: testBoundAddr, CSRFToken: "tok"})

	rec := getPage(t, h, "/wt/"+feat.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no changes vs") {
		t.Errorf("expected the empty-diff card, got:\n%s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `id="approve-btn"`) {
		t.Errorf("an empty diff has nothing to review/approve, expected no approve button, got:\n%s", rec.Body.String())
	}
}

func TestRoomHandlerBinaryFileShowsNote(t *testing.T) {
	eng, feat := buildRepoEngine(t, func(_, wt string) {
		mustWriteFile(t, filepath.Join(wt, "logo.png"), []byte("PNG\x00\x01\x02binarydata"))
	})
	h := New(eng, stubAPI(), Config{BoundAddr: testBoundAddr, CSRFToken: "tok"})

	rec := getPage(t, h, "/wt/"+feat.ID)
	body := rec.Body.String()
	if !strings.Contains(body, "logo.png") || !strings.Contains(body, "binary file") {
		t.Errorf("expected a binary-file note for logo.png, got:\n%s", body)
	}
	if strings.Contains(body, `class="sxs"`) {
		t.Errorf("a binary file must not render side-by-side rows, got:\n%s", body)
	}
}

func TestRoomHandlerCollapsesLargeFileAndExpandLinkRoundTrips(t *testing.T) {
	eng, feat := buildRepoEngine(t, func(_, wt string) {
		var b bytes.Buffer
		for i := 0; i < 5; i++ {
			b.WriteString("dummyhash")
			b.WriteString(strings.Repeat("0", 40))
			b.WriteByte('\n')
		}
		// A nested path whose basename is "go.sum": the collapse glob matches
		// on path.Base, so this proves the whole nested path round-trips
		// through the expand link's URL-query escaping, not just a bare name.
		mustWriteFile(t, filepath.Join(wt, "vendor", "go.sum"), b.Bytes())
	})
	h := New(eng, stubAPI(), Config{BoundAddr: testBoundAddr, CSRFToken: "tok"})

	rec := getPage(t, h, "/wt/"+feat.ID)
	body := rec.Body.String()
	if !strings.Contains(body, "collapsed (") {
		t.Fatalf("expected the lockfile-glob file to collapse by default, got:\n%s", body)
	}
	if strings.Contains(body, `class="sxs"`) {
		t.Fatalf("a collapsed file must not render its hunks, got:\n%s", body)
	}

	href := scrapeExpandHref(t, body)
	rec2 := getPage(t, h, "/wt/"+feat.ID+href)
	if rec2.Code != http.StatusOK {
		t.Fatalf("expand link %q: status = %d, want 200", href, rec2.Code)
	}
	if !strings.Contains(rec2.Body.String(), `class="sxs"`) {
		t.Errorf("expected the expanded file's side-by-side rows to render via %q, got:\n%s", href, rec2.Body.String())
	}
	if strings.Contains(rec2.Body.String(), "collapsed (") {
		t.Errorf("expanded page should no longer show the collapsed note, got:\n%s", rec2.Body.String())
	}
}

var expandHrefRE = regexp.MustCompile(`href="(\?expand=[^"]*)"`)

func scrapeExpandHref(t *testing.T, body string) string {
	t.Helper()
	m := expandHrefRE.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no expand href found in:\n%s", body)
	}
	return html.UnescapeString(m[1])
}

func TestRoomHandlerHighlightOffNoteForLargeFile(t *testing.T) {
	eng, feat := buildRepoEngine(t, func(_, wt string) {
		var b bytes.Buffer
		// Over highlightMaxLines but under the collapse threshold's line
		// count would be a contradiction (collapse triggers on Add+Del, same
		// axis) -- so this deliberately uses many *short* lines, comfortably
		// over highlightMaxLines, to land on the "too large to highlight,
		// but not collapsed" path distinctly from the collapse test above.
		// (isCollapsedByDefault's size axis and the highlight-off axis both
		// key off line/byte count, so a file here trips both -- the note we
		// assert is highlight.go's, reached by first expanding.)
		for i := 0; i < highlightMaxLines+50; i++ {
			b.WriteString("x\n")
		}
		mustWriteFile(t, filepath.Join(wt, "generated.go"), b.Bytes())
	})
	h := New(eng, stubAPI(), Config{BoundAddr: testBoundAddr, CSRFToken: "tok"})

	rec := getPage(t, h, "/wt/"+feat.ID+"?expand=generated.go")
	body := rec.Body.String()
	if !strings.Contains(body, "highlighting off (large file)") {
		t.Errorf("expected the highlight-off note once expanded, got:\n%s", body)
	}
}

// TestRoomHandlerRendersGuardrailBannerWhenTripped pins the phase
// acceptance test's explicit example (P4-design.md §5.5): a file under
// migrations/ trips the default "touches-migrations" danger rule, and its
// message must appear verbatim in the room's amber banner, with the file's
// own tag tinted "danger".
func TestRoomHandlerRendersGuardrailBannerWhenTripped(t *testing.T) {
	eng, feat := buildRepoEngine(t, func(_, wt string) {
		mustWriteFile(t, filepath.Join(wt, "migrations", "014_drop.sql"), []byte("DROP TABLE x;\n"))
	})
	h := New(eng, stubAPI(), Config{BoundAddr: testBoundAddr, CSRFToken: "tok"})

	rec := getPage(t, h, "/wt/"+feat.ID)
	body := rec.Body.String()
	if !strings.Contains(body, `<b>Guardrail:</b> touches database migrations`) {
		t.Errorf("expected the guardrail hit's message verbatim in the banner, got:\n%s", body)
	}
	if !strings.Contains(body, `class="tag danger"`) {
		t.Errorf("expected the migrations file's tag tinted danger, got:\n%s", body)
	}
}

var approveBtnDisabledRE = regexp.MustCompile(`id="approve-btn"[^>]*\sdisabled`)

// TestRoomHandlerApproveGateReflectsDirtyWorktreeState pins the P4-fixes.md
// #3 fix: the engine's Approve gates on a clean worktree in addition to full
// review (engine.go's Gate 2, IsDirty) but the room previously only ever
// reflected the review gate, so a fully-reviewed-but-dirty worktree looked
// approvable right up until the 409. The room must now surface the knowable
// State-derived dirty signal (registry snapshot, no new engine call) before
// the click, and the 409 stays as the real backstop.
func TestRoomHandlerApproveGateReflectsDirtyWorktreeState(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	testGit(t, repo, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "app.go"), []byte("package app\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	testGit(t, repo, "add", ".")
	testGit(t, repo, "commit", "-q", "-m", "init")

	wt := filepath.Join(root, "repo-feature")
	testGit(t, repo, "worktree", "add", "-q", "-b", "feature", wt)
	if err := os.WriteFile(filepath.Join(wt, "app.go"), []byte("package app\n\nfunc B() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	reg := registry.New()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	be := gitbackend.NewCLIWithEnv(testGitEnv())
	gr := mustResolver(t, guardrail.DefaultRules())
	// A vanishingly small (but > 0 — engine.New clamps <= 0 to a 30s default)
	// ActivityWindow: "recently changed" can never mask the dirty signal
	// here — buildWorktree's own IsDirty git subprocess call alone takes far
	// longer than this, so by the time State is computed the window has
	// already elapsed. This test is about the dirty-vs-clean gate
	// specifically, not activity recency (which engine.state also folds
	// into State).
	eng := engine.New(engine.Config{Roots: []string{root}, MaxDepth: 4, ActivityWindow: 1 * time.Microsecond}, be, reg, st, gr)
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
	if feat.State != model.StateDirty {
		t.Fatalf("precondition: worktree state = %q, want dirty (uncommitted change, ActivityWindow already elapsed)", feat.State)
	}
	if err := eng.SetReviewed(feat.ID, "app.go", true, ""); err != nil {
		t.Fatal(err)
	}

	h := New(eng, stubAPI(), Config{BoundAddr: testBoundAddr, CSRFToken: "tok"})
	body := getPage(t, h, "/wt/"+feat.ID).Body.String()
	if !strings.Contains(body, `data-dirty="true"`) {
		t.Errorf("expected the approve button to carry data-dirty=true, got:\n%s", body)
	}
	if !strings.Contains(body, "uncommitted changes") {
		t.Errorf("expected a pre-check line naming uncommitted changes, got:\n%s", body)
	}
	if !approveBtnDisabledRE.MatchString(body) {
		t.Errorf("expected the approve button disabled even though every file is reviewed (dirty gate), got:\n%s", body)
	}

	// Commit the change: identical diff content vs base (same blob), but no
	// longer dirty.
	testGit(t, wt, "add", ".")
	testGit(t, wt, "commit", "-q", "-m", "wip")
	if err := eng.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	body = getPage(t, h, "/wt/"+feat.ID).Body.String()
	if !strings.Contains(body, `data-dirty="false"`) {
		t.Errorf("expected data-dirty=false once the worktree is committed, got:\n%s", body)
	}
	if strings.Contains(body, "uncommitted changes") {
		t.Errorf("expected the dirty pre-check line gone once clean, got:\n%s", body)
	}
	if approveBtnDisabledRE.MatchString(body) {
		t.Errorf("expected the approve button enabled once clean and fully reviewed, got:\n%s", body)
	}
}

// ---- side-by-side rendering + chroma classes on a real repo diff ----

func TestRoomHandlerRendersSideBySideWithChromaClasses(t *testing.T) {
	h, feat, _ := buildTestApp(t) // fixture: app.go gains 2 lines vs base
	rec := getPage(t, h, "/wt/"+feat.ID)
	body := rec.Body.String()

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, body)
	}
	if !strings.Contains(body, "before · main") {
		t.Errorf("expected the mock's \"before · <base>\" label, got:\n%s", body)
	}
	if !strings.Contains(body, "after · working tree") {
		t.Errorf("expected the mock's \"after · working tree\" label, got:\n%s", body)
	}
	if !strings.Contains(body, `class="srow`) {
		t.Errorf("expected side-by-side rows, got:\n%s", body)
	}
	if !strings.Contains(body, `class="srow empty"`) {
		t.Errorf("app.go's fixture change is add-only, so the \"before\" side should show empty filler rows, got:\n%s", body)
	}
	if !regexp.MustCompile(`class="[a-z]+ chroma"`).MatchString(body) {
		t.Errorf("expected at least one chroma-classed code cell, got:\n%s", body)
	}
	if got := rec.Header().Get("Content-Security-Policy"); got == "" {
		t.Error("expected the CSP header on the room page")
	}
}

// ---- escaping torture matrix (NON-NEGOTIABLE) ----

// buildHostileTestEngine builds a fixture repo whose feature worktree
// contains every torture-matrix probe: a filename that is itself markup
// (built as a real nested path, since "</script>" contains a literal '/'
// that can't live inside one path component), file content carrying
// </script><script> and "><img onerror payloads, ANSI/control bytes
// (already caret-sanitized upstream by diffparse -- this proves that holds
// end to end), a U+202E RTL override, an invalid UTF-8 byte sequence, and a
// unicode path.
func buildHostileTestEngine(t *testing.T) (*engine.Engine, model.Worktree) {
	t.Helper()
	return buildRepoEngine(t, func(_, wt string) {
		scriptContent := "package hostile\n\n" +
			"// </script><script>alert(2)</script>\n" +
			`var payload = "\"><img src=x onerror=alert(3)>"` + "\n"
		// The reported diff path ends up exactly "<script>alert(1)</script>.go"
		// -- git joins the directory and file with '/', reproducing the
		// literal hostile string from the design's torture matrix.
		mustWriteFile(t, filepath.Join(wt, "<script>alert(1)<", "script>.go"), []byte(scriptContent))

		var weird bytes.Buffer
		weird.WriteString("package hostile\n\n")
		weird.WriteString("// ansi: \x1b[31mred\x1b[0m\x07 escape\n")
		weird.WriteString("// rtl: name‮gnp.exe override\n")
		weird.WriteString("// invalid utf8 marker: ")
		weird.Write([]byte{0xff, 0xfe})
		weird.WriteString("\n")
		mustWriteFile(t, filepath.Join(wt, "café", "日本語.go"), weird.Bytes())
	})
}

func TestRoomRendersHostileFixtureFullyEscaped(t *testing.T) {
	eng, wt := buildHostileTestEngine(t)
	h := New(eng, stubAPI(), Config{BoundAddr: testBoundAddr, CSRFToken: "tok"})

	rec := getPage(t, h, "/wt/"+wt.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	// The hostile filename rendered, but only escaped.
	if !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt;.go") {
		t.Errorf("expected the hostile filename escaped, got:\n%s", body)
	}
	// The content payloads rendered, but only escaped.
	if !strings.Contains(body, "&lt;/script&gt;&lt;script&gt;alert(2)&lt;/script&gt;") {
		t.Errorf("expected the </script><script> payload escaped, got:\n%s", body)
	}
	if !strings.Contains(body, "&gt;&lt;img") {
		t.Errorf(`expected the "><img payload's angle brackets escaped, got:%s`, body)
	}

	assertNoLiveInjection(t, body)

	// ANSI control bytes: caret notation, never raw bytes (diffparse's choke
	// point, exercised end to end through highlighting too).
	if strings.ContainsAny(body, "\x1b\x07") {
		t.Error("raw control bytes (ESC/BEL) leaked into the response body")
	}
	if !strings.Contains(body, "^[") {
		t.Error(`expected the sanitized ESC byte to render as caret notation "^["`)
	}

	// Unicode path renders.
	if !strings.Contains(body, "café") || !strings.Contains(body, "日本語.go") {
		t.Errorf("expected the unicode path to render, got:\n%s", body)
	}
}

// assertNoLiveInjection is the grep-style proof the phase acceptance test
// (P4-design.md §5.6) asks for: none of the raw payloads survive unescaped,
// there is no live <img tag anywhere, and the only <script tag on the whole
// page is the one sanctioned static asset include. It deliberately does NOT
// grep the rendered body for "on[a-z]+=" — the hostile fixture's own
// (safely escaped) payload text legitimately contains the characters
// "onerror=" as DATA, which would false-positive a body-wide regex; the
// inline-handler check that actually matters belongs to the template
// SOURCE, not to attacker-influenced rendered output — see
// TestTemplatesHaveNoInlineEventHandlerAttributes below.
func assertNoLiveInjection(t *testing.T, body string) {
	t.Helper()
	for _, payload := range []string{
		"<script>alert(1)</script>",
		"</script><script>alert(2)</script>",
		`"><img src=x onerror=alert(3)>`,
	} {
		if strings.Contains(body, payload) {
			t.Errorf("raw unescaped payload %q found in response body", payload)
		}
	}
	if strings.Contains(body, "<img") {
		t.Error("a live <img tag must never appear on this page at all")
	}
	if n := strings.Count(body, "<script"); n != 1 {
		t.Errorf("found %d <script tags, want exactly 1 (the static app.js include)", n)
	}
	if !strings.Contains(body, `<script src="/static/app.js">`) {
		t.Error("expected the one sanctioned <script> tag to be the static app.js include")
	}
}

// TestTemplatesHaveNoInlineEventHandlerAttributes greps the template SOURCE
// files (not runtime output, which can legitimately contain attacker text
// that mentions "onclick=" etc. as escaped data) for a live onXxx="..."
// attribute — the NON-NEGOTIABLE "zero inline event handlers in templates"
// requirement, checked at the artifact it actually governs.
func TestTemplatesHaveNoInlineEventHandlerAttributes(t *testing.T) {
	inlineAttr := regexp.MustCompile(`\son[a-z]+\s*=\s*"`)
	for _, name := range []string{"templates/layout.tmpl", "templates/index.tmpl", "templates/room.tmpl", "templates/fragments.tmpl"} {
		src, err := templateFS.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if inlineAttr.Match(src) {
			t.Errorf("%s contains what looks like an inline event handler attribute (onXxx=\"...\"), want none", name)
		}
		if bytes.Contains(bytes.ToLower(src), []byte("javascript:")) {
			t.Errorf("%s contains a javascript: URL, want none", name)
		}
	}
}

func TestRoomPageHasNoInlineEventHandlersOrJavascriptURLs(t *testing.T) {
	h, feat, _ := buildTestApp(t)
	rec := getPage(t, h, "/wt/"+feat.ID)
	body := rec.Body.String()
	if strings.Contains(strings.ToLower(body), "javascript:") {
		t.Errorf("found a javascript: URL, got:\n%s", body)
	}
}

// app.js itself: the boundary rules by grep (mirrors app.js's own doc
// comment) -- the actual innerHTML *assignment* happens exactly once (the
// pre-existing fragment-swap helper WP2 shipped; the word "innerHTML" also
// appears once more, in that helper's own doc comment, which this counts by
// the real mutation site, not the bare word); no eval/document.write.
func TestAppJSKeepsBoundaryRulesInForce(t *testing.T) {
	src, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(src)
	if n := strings.Count(js, ".innerHTML ="); n != 1 {
		t.Errorf(".innerHTML = assignment appears %d times in app.js, want exactly 1 (the whole-fragment swap helper)", n)
	}
	if strings.Contains(js, "eval(") || strings.Contains(js, "document.write") {
		t.Error("app.js must never eval() or document.write()")
	}
	if !strings.Contains(js, "X-Csrf-Token") {
		t.Error("expected app.js's review/approve POSTs to send X-Csrf-Token")
	}
}

// ---- review/approve: CSRF plumbing through the full web.New stack ----
//
// The real /api/review and /api/approve handlers (validation, gating, the
// engine) are cmd/wtd's, already tested there and in internal/engine; this
// package cannot import cmd/wtd's "main" package (see web_test.go's own
// comment on stubAPI). What belongs here is proving the room's POSTs reach
// /api/* through the CSRF/Host/Origin middleware correctly -- a stand-in
// API is enough for that.

func reviewApproveStubAPI() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/review", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			File string `json:"file"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.File == "conflict.go" {
			http.Error(w, "file changed since it was reviewed", http.StatusConflict)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("/api/approve", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID string `json:"id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.ID == "gated" {
			http.Error(w, "cannot approve: 1 of 1 files not yet reviewed", http.StatusConflict)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(model.ApproveResult{WorktreeID: req.ID, Merged: "feature", Into: "main", Removed: "/tmp/x"})
	})
	return mux
}

func postWithHost(h http.Handler, path, body, csrf string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Host = testBoundAddr
	req.Header.Set("Content-Type", "application/json")
	if csrf != "" {
		req.Header.Set("X-Csrf-Token", csrf)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestRoomReviewPOSTRequiresCSRFTokenThenSucceeds(t *testing.T) {
	h := New(nil, reviewApproveStubAPI(), Config{BoundAddr: testBoundAddr, CSRFToken: "the-token"})
	body := `{"id":"a1","file":"app.go","reviewed":true,"hash":"h1"}`

	rec := postWithHost(h, "/api/review", body, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("without X-Csrf-Token: status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}

	rec = postWithHost(h, "/api/review", body, "wrong-token")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("with a wrong token: status = %d, want 403", rec.Code)
	}

	rec = postWithHost(h, "/api/review", body, "the-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("with the correct token: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Security-Policy"); got == "" {
		t.Error("expected the CSP header on the /api/review response reached via the web listener")
	}
}

func TestRoomReview409RevertsAndApprove409RendersGateVerbatim(t *testing.T) {
	h := New(nil, reviewApproveStubAPI(), Config{BoundAddr: testBoundAddr, CSRFToken: "tok"})

	rec := postWithHost(h, "/api/review", `{"id":"a1","file":"conflict.go","reviewed":true}`, "tok")
	if rec.Code != http.StatusConflict {
		t.Fatalf("review conflict: status = %d, want 409", rec.Code)
	}

	rec = postWithHost(h, "/api/approve", `{"id":"gated"}`, "tok")
	if rec.Code != http.StatusConflict {
		t.Fatalf("approve gate: status = %d, want 409", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "1 of 1 files not yet reviewed") {
		t.Errorf("expected the gate message verbatim in the 409 body, got %q", rec.Body.String())
	}
}

func TestRoomApproveSuccessReturnsApproveResultJSON(t *testing.T) {
	h := New(nil, reviewApproveStubAPI(), Config{BoundAddr: testBoundAddr, CSRFToken: "tok"})
	rec := postWithHost(h, "/api/approve", `{"id":"a1"}`, "tok")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var res model.ApproveResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	if res.Into != "main" || res.Merged != "feature" {
		t.Errorf("ApproveResult = %+v, want Into=main Merged=feature", res)
	}
}

// ---- render bench (P4-design.md §6 budget) ----

// BenchmarkRoomRender5kLines renders a fixture spread across several files
// totalling ~5k diff lines (each individually under highlightMaxLines, so
// chroma highlighting is actually exercised, not skipped) through the full
// GET /wt/{id} handler stack, warm-cache (matches the design's "warm cache"
// budget row) -- one throwaway request primes the per-file highlight cache
// before the timed loop starts.
func BenchmarkRoomRender5kLines(b *testing.B) {
	root := b.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		b.Fatal(err)
	}
	run := func(dir string, args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = testGitEnv()
		if out, err := cmd.CombinedOutput(); err != nil {
			b.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run(repo, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "README"), []byte("base\n"), 0o644); err != nil {
		b.Fatal(err)
	}
	run(repo, "add", ".")
	run(repo, "commit", "-q", "-m", "init")

	wt := filepath.Join(root, "repo-feature")
	run(repo, "worktree", "add", "-q", "-b", "feature", wt)

	const files, linesPerFile = 10, 500 // 5,000 lines total, each file under highlightMaxLines
	for f := 0; f < files; f++ {
		var buf bytes.Buffer
		fmt.Fprintf(&buf, "package f%d\n\n", f)
		for i := 0; i < linesPerFile; i++ {
			fmt.Fprintf(&buf, "func g%d() int { return %d } // line %d\n", i, i, i)
		}
		if err := os.WriteFile(filepath.Join(wt, fmt.Sprintf("f%d.go", f)), buf.Bytes(), 0o644); err != nil {
			b.Fatal(err)
		}
	}

	reg := registry.New()
	st, err := store.Open(filepath.Join(b.TempDir(), "state.db"))
	if err != nil {
		b.Fatal(err)
	}
	be := gitbackend.NewCLIWithEnv(testGitEnv())
	gr := mustResolver(b, guardrail.DefaultRules())
	eng := engine.New(engine.Config{Roots: []string{root}, MaxDepth: 4, ActivityWindow: 30 * time.Second}, be, reg, st, gr)
	if err := eng.Refresh(context.Background()); err != nil {
		b.Fatal(err)
	}
	var id string
	for _, w := range eng.List() {
		if w.Branch == "feature" {
			id = w.ID
		}
	}
	if id == "" {
		b.Fatal("feature worktree not found")
	}

	h := New(eng, stubAPI(), Config{BoundAddr: testBoundAddr, CSRFToken: "tok"})
	get := func() *httptest.ResponseRecorder { return getPageB(h, "/wt/"+id) }

	if rec := get(); rec.Code != http.StatusOK {
		b.Fatalf("warm-up request: status = %d", rec.Code)
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if rec := get(); rec.Code != http.StatusOK {
			b.Fatalf("status = %d", rec.Code)
		}
	}
}

func getPageB(h http.Handler, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Host = testBoundAddr
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}
