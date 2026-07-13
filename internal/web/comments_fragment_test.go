package web

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This file is WP4's comments-UI suite: fragment rendering (escaping torture,
// stale/orphaned badges, resolve/delete round trip), the room page's inline
// per-file strip, the rail fragment, and the composer's CSRF-gated POST path.
// buildRepoEngine/mustWriteFile/testGit/stubAPI/getPage/testBoundAddr come
// from room_test.go/web_test.go (same package).

// TestAppJSCSRFStaleMessageMatchesMiddleware pins the one string app.js
// compares a 403 body against to decide "reload the page" (P4-design.md
// §1.3/§2's "CSRF stale after daemon restart" state) — a silent edit to
// either copy without the other would quietly break that reload path.
func TestAppJSCSRFStaleMessageMatchesMiddleware(t *testing.T) {
	src, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), `"`+csrfMismatchMsg+`"`) {
		t.Errorf("app.js does not contain middleware.go's csrfMismatchMsg (%q) as a JS string literal", csrfMismatchMsg)
	}
}

func TestFragmentCommentsRendersGroupAndEscapesHostileBody(t *testing.T) {
	eng, feat := buildTestEngine(t) // fixture: app.go changed vs base
	h := New(eng, stubAPI(), Config{BoundAddr: testBoundAddr, CSRFToken: "tok"})

	hostile := `<script>alert(1)</script>"><img src=x onerror=alert(2)>`
	if _, err := eng.AddComment(feat.ID, "app.go", 3, "new", hostile, "reviewer1"); err != nil {
		t.Fatalf("AddComment: %v", err)
	}

	rec := getPage(t, h, "/fragment/comments?id="+feat.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	if strings.Contains(body, "<html") || strings.Contains(body, "<!doctype") {
		t.Errorf("fragment must not include the page shell, got:\n%s", body)
	}
	if !strings.Contains(body, `data-anchor="0:new:3"`) {
		t.Errorf("expected the group's fileIdx:side:line anchor, got:\n%s", body)
	}
	if !strings.Contains(body, `data-comment-id`) || !strings.Contains(body, "reviewer1") {
		t.Errorf("expected the comment card with its author, got:\n%s", body)
	}
	if strings.Contains(body, "<script>alert(1)</script>") || strings.Contains(body, "<img src=x") {
		t.Errorf("hostile comment body leaked unescaped into the fragment, got:\n%s", body)
	}
	if !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Errorf("expected the hostile body to render escaped, got:\n%s", body)
	}
	if strings.Contains(body, "<script") {
		t.Errorf("fragment (no layout, no static includes) must contain zero <script tags, got:\n%s", body)
	}
}

func TestFragmentCommentsFileLevelCommentAnchorsAtLineZero(t *testing.T) {
	eng, feat := buildTestEngine(t)
	h := New(eng, stubAPI(), Config{BoundAddr: testBoundAddr, CSRFToken: "tok"})

	if _, err := eng.AddComment(feat.ID, "app.go", 0, "", "needs a broader look", ""); err != nil {
		t.Fatalf("AddComment: %v", err)
	}
	rec := getPage(t, h, "/fragment/comments?id="+feat.ID)
	body := rec.Body.String()
	if !strings.Contains(body, `data-anchor="0:new:0"`) {
		t.Errorf("expected line-0 file-level anchor (side defaults to new), got:\n%s", body)
	}
	if !strings.Contains(body, "file-level") {
		t.Errorf("expected the file-level label, got:\n%s", body)
	}
}

func TestFragmentCommentsStaleBadgeAppearsAfterFileEdit(t *testing.T) {
	eng, feat := buildTestEngine(t)
	h := New(eng, stubAPI(), Config{BoundAddr: testBoundAddr, CSRFToken: "tok"})

	if _, err := eng.AddComment(feat.ID, "app.go", 3, "new", "looks fine here", ""); err != nil {
		t.Fatalf("AddComment: %v", err)
	}
	if strings.Contains(getPage(t, h, "/fragment/comments?id="+feat.ID).Body.String(), `class="cbadge stale"`) {
		t.Fatal("precondition: no stale badge before the file changes")
	}

	// Edit the commented file's content, then re-diff (mirrors the daemon's
	// own refresh cycle) — the file's hash moves, so the comment's anchor no
	// longer matches: engine.Comments recomputes Stale fresh on every read.
	if err := os.WriteFile(filepath.Join(feat.Path, "app.go"), []byte("package app\n\nfunc B() {}\nfunc C() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := eng.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	body := getPage(t, h, "/fragment/comments?id="+feat.ID).Body.String()
	if !strings.Contains(body, `class="cbadge stale"`) {
		t.Errorf("expected the stale badge once the file changed underneath the comment, got:\n%s", body)
	}
}

// TestFragmentCommentsOrphanedSectionWhenFileLeavesDiff also pins the
// P4-fixes.md #7 fix: the section used to render its "comments on files no
// longer in this diff" heading (and a "none" placeholder) on every single
// room page, orphaned comments or not. The empty state must now render a
// genuinely childless container (style.css's ":empty" selector hides it) —
// app.js still always finds #orphaned-comments as a live-refresh mount point.
func TestFragmentCommentsOrphanedSectionWhenFileLeavesDiff(t *testing.T) {
	eng, feat := buildTestEngine(t)
	h := New(eng, stubAPI(), Config{BoundAddr: testBoundAddr, CSRFToken: "tok"})

	// Precondition: nothing orphaned yet — the section must render as an
	// empty, childless container, not the heading + "none" placeholder.
	pre := getPage(t, h, "/fragment/comments?id="+feat.ID).Body.String()
	if !strings.Contains(pre, `<div class="card orphaned-comments" id="orphaned-comments"></div>`) {
		t.Fatalf("precondition: expected a genuinely empty orphaned-comments div, got:\n%s", pre)
	}
	if strings.Contains(pre, "comments on files no longer in this diff") || strings.Contains(pre, "none") {
		t.Errorf("precondition: empty orphaned section must not render its heading/placeholder, got:\n%s", pre)
	}

	if _, err := eng.AddComment(feat.ID, "app.go", 0, "", "file-level note", ""); err != nil {
		t.Fatalf("AddComment: %v", err)
	}

	// Revert app.go back to base's exact content: the file drops out of the
	// diff entirely (no changes vs base left to report).
	if err := os.WriteFile(filepath.Join(feat.Path, "app.go"), []byte("package app\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := eng.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	body := getPage(t, h, "/fragment/comments?id="+feat.ID).Body.String()
	if !strings.Contains(body, `id="orphaned-comments"`) {
		t.Fatalf("expected the orphaned section, got:\n%s", body)
	}
	if !strings.Contains(body, "comments on files no longer in this diff") {
		t.Errorf("expected the section's heading now that it's populated, got:\n%s", body)
	}
	if !strings.Contains(body, `class="cbadge orphaned"`) || !strings.Contains(body, "app.go") {
		t.Errorf("expected the orphaned comment naming its file, got:\n%s", body)
	}
	// Regression guard: an orphaned file no longer has a fileIdx at all, so
	// it must not still be rendered under some stale numbered file strip too.
	if strings.Contains(body, `id="comments-0"`) && strings.Contains(strings.SplitN(body, `id="comments-0"`, 2)[1], "file-level note") {
		t.Errorf("orphaned comment must not also appear in a numbered file strip, got:\n%s", body)
	}
}

func TestFragmentCommentsResolveAndDeleteRoundTrip(t *testing.T) {
	eng, feat := buildTestEngine(t)
	h := New(eng, stubAPI(), Config{BoundAddr: testBoundAddr, CSRFToken: "tok"})

	c, err := eng.AddComment(feat.ID, "app.go", 3, "new", "please rename this", "")
	if err != nil {
		t.Fatalf("AddComment: %v", err)
	}
	body := getPage(t, h, "/fragment/comments?id="+feat.ID).Body.String()
	if !strings.Contains(body, `class="btn ghost c-resolve"`) {
		t.Errorf("expected a resolve button on an open comment, got:\n%s", body)
	}
	// P4-fixes.md #2: delete is destructive and gets an inline two-step
	// confirm (app.js turns "delete" into "delete?" + reveals this button on
	// a first click) rather than a native confirm() dialog — both buttons
	// are server-rendered up front, the cancel one just starts hidden.
	if !strings.Contains(body, `class="btn ghost c-delete-cancel hidden"`) {
		t.Errorf("expected a hidden delete-confirm cancel button alongside delete, got:\n%s", body)
	}

	if err := eng.ResolveComment(feat.ID, c.ID); err != nil {
		t.Fatalf("ResolveComment: %v", err)
	}
	body = getPage(t, h, "/fragment/comments?id="+feat.ID).Body.String()
	if !strings.Contains(body, `class="ccard resolved"`) {
		t.Errorf("expected the resolved styling class after resolving, got:\n%s", body)
	}
	if strings.Contains(body, "c-resolve") {
		t.Errorf("a resolved comment must not still offer a resolve button, got:\n%s", body)
	}
	if !strings.Contains(body, "c-delete") {
		t.Errorf("a resolved comment must still offer delete (no CLI verb exists for it), got:\n%s", body)
	}

	if err := eng.DeleteComment(feat.ID, c.ID); err != nil {
		t.Fatalf("DeleteComment: %v", err)
	}
	body = getPage(t, h, "/fragment/comments?id="+feat.ID).Body.String()
	if strings.Contains(body, c.ID) {
		t.Errorf("expected the deleted comment gone from the fragment entirely, got:\n%s", body)
	}
}

// TestRoomPageHostileCommentBodyRendersFullyEscaped is the design's own
// torture case (P4-design.md WP4 done-when: "<script> body renders inert
// text") run through the full room page, on top of the hostile FILE fixture
// WP3 already exercises (buildHostileTestEngine/assertNoLiveInjection,
// room_test.go) — proving a hostile comment body neither reintroduces a live
// <script>/<img> nor rides in on the file's own already-tested escaping path.
// Also pins that markdown-ish text stays inert plain text (P4-design.md §2):
// this package renders comment bodies through the same {{.Body}} contextual
// escaping every other dynamic value on the page already goes through —
// there is no markdown processor to accidentally turn "**bold**" into
// emphasis.
func TestRoomPageHostileCommentBodyRendersFullyEscaped(t *testing.T) {
	eng, wt := buildHostileTestEngine(t)
	h := New(eng, stubAPI(), Config{BoundAddr: testBoundAddr, CSRFToken: "tok"})

	hostileBody := "</script><script>alert(9)</script> \"><img src=x onerror=alert(9)> **bold** `code` # heading"
	if _, err := eng.AddComment(wt.ID, "<script>alert(1)</script>.go", 0, "", hostileBody, "attacker"); err != nil {
		t.Fatalf("AddComment: %v", err)
	}

	rec := getPage(t, h, "/wt/"+wt.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	assertNoLiveInjection(t, body) // WP3's page-wide invariant: still holds with a hostile comment body added
	if !strings.Contains(body, "&lt;/script&gt;&lt;script&gt;alert(9)&lt;/script&gt;") {
		t.Errorf("expected the comment body's script payload escaped, got:\n%s", body)
	}
	if !strings.Contains(body, "**bold**") || !strings.Contains(body, "`code`") || !strings.Contains(body, "# heading") {
		t.Errorf("expected markdown-ish text to render as inert plain text (no processor), got:\n%s", body)
	}
	if strings.Contains(body, "<b>bold</b>") || strings.Contains(body, "<code>code</code>") || strings.Contains(body, "<h1>") {
		t.Error("comment body must never be interpreted as markdown")
	}
}

// ---- room page: comments render inline, alongside the diff ----

func TestRoomPageRendersCommentStripsInline(t *testing.T) {
	eng, feat := buildTestEngine(t)
	h := New(eng, stubAPI(), Config{BoundAddr: testBoundAddr, CSRFToken: "tok"})

	if _, err := eng.AddComment(feat.ID, "app.go", 3, "new", "a specific line note", "alice"); err != nil {
		t.Fatalf("AddComment: %v", err)
	}
	if _, err := eng.AddComment(feat.ID, "app.go", 0, "", "a whole-file note", "bob"); err != nil {
		t.Fatalf("AddComment: %v", err)
	}

	body := getPage(t, h, "/wt/"+feat.ID).Body.String()
	if !strings.Contains(body, "a specific line note") || !strings.Contains(body, "alice") {
		t.Errorf("expected the line comment inline in the room page, got:\n%s", body)
	}
	if !strings.Contains(body, "a whole-file note") || !strings.Contains(body, "bob") {
		t.Errorf("expected the file-level comment inline in the room page, got:\n%s", body)
	}
	if !strings.Contains(body, `id="tpl-comment-composer"`) {
		t.Errorf("expected the static composer <template> on the room page, got:\n%s", body)
	}
	if !strings.Contains(body, ` c-add"`) {
		t.Errorf("expected a file-level \"comment\" affordance in the file header, got:\n%s", body)
	}
	if !strings.Contains(body, `data-line="3"`) || !strings.Contains(body, `data-side="new"`) {
		t.Errorf("expected gutter cells carrying data-line/data-side for the composer prefill, got:\n%s", body)
	}
	// P4-fixes.md #5: a commentable gutter cell is a real <button> (focusable,
	// keyboard-operable — WCAG 2.1.1), not a hover-only <span>.
	if !strings.Contains(body, `<button type="button" class="ln" data-file="app.go" data-file-idx="0" data-line="3" data-side="new"`) {
		t.Errorf("expected the line-3/new gutter cell to be a real <button>, got:\n%s", body)
	}
	// P4-fixes.md #10: the strip's own container carries data-file (path),
	// what app.js's live-refresh reconciles on instead of the index-derived id.
	if !strings.Contains(body, `id="comments-0" data-file-idx="0" data-file="app.go"`) {
		t.Errorf("expected the comments strip to carry data-file=\"app.go\", got:\n%s", body)
	}
}

func TestRoomPageEmptyDiffStillRendersOrphanedCommentsSection(t *testing.T) {
	eng, feat := buildRepoEngine(t, nil) // no changes vs base at all
	if _, err := eng.AddComment(feat.ID, "README", 0, "", "stray note", ""); err == nil {
		t.Fatal("precondition failed: README isn't in an empty diff, AddComment should have refused it")
	}
	// Empty-diff rooms can't carry a comment in the first place (nothing is
	// in the diff to anchor to) -- this pins that the page still renders the
	// (empty) orphaned section container without erroring, since app.js
	// always expects to find it.
	h := New(eng, stubAPI(), Config{BoundAddr: testBoundAddr, CSRFToken: "tok"})
	rec := getPage(t, h, "/wt/"+feat.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `id="orphaned-comments"`) {
		t.Errorf("expected the always-present orphaned section even on an empty diff, got:\n%s", rec.Body.String())
	}
}

// ---- rail fragment ----

func TestFragmentRailReflectsReviewedStateAndUnknownID404s(t *testing.T) {
	eng, feat := buildTestEngine(t)
	if err := eng.SetReviewed(feat.ID, "app.go", true, ""); err != nil {
		t.Fatalf("SetReviewed: %v", err)
	}
	h := New(eng, stubAPI(), Config{BoundAddr: testBoundAddr, CSRFToken: "tok"})

	rec := getPage(t, h, "/fragment/rail?id="+feat.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "<html") {
		t.Errorf("rail fragment must not include the page shell, got:\n%s", body)
	}
	if !strings.Contains(body, `id="prog-txt"`) || !strings.Contains(body, "1 / 1") {
		t.Errorf("expected the reviewed count to reflect SetReviewed, got:\n%s", body)
	}
	if !strings.Contains(body, `id="approve-btn"`) {
		t.Errorf("expected the approve button in the rail fragment, got:\n%s", body)
	}
	// P4-fixes.md #9: style-src 'self' (no unsafe-inline) silently no-ops an
	// inline style="width:...%" attribute — the CSP audit's NOTE. The width
	// must come from app.js (bar.style.width, not CSP-restricted) instead.
	if strings.Contains(body, `id="prog-bar" style=`) {
		t.Errorf("prog-bar must not use an inline style attribute (blocked by our own CSP), got:\n%s", body)
	}
	if !strings.Contains(body, `<i id="prog-bar"></i>`) {
		t.Errorf("expected a bare prog-bar element with its width left to app.js, got:\n%s", body)
	}

	rec = getPage(t, h, "/fragment/rail?id=no-such-id")
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown id: status = %d, want 404", rec.Code)
	}
}

// ---- composer POST: CSRF-gated, through the full web.New stack ----

func commentsStubAPI() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/comments", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Body string `json:"body"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Body == "" {
			http.Error(w, "body must not be empty", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "c-stub"})
	})
	return mux
}

func TestRoomCommentsPOSTRequiresCSRFTokenThenSucceeds(t *testing.T) {
	h := New(nil, commentsStubAPI(), Config{BoundAddr: testBoundAddr, CSRFToken: "the-token"})
	body := `{"id":"a1","file":"app.go","line":3,"side":"new","body":"looks off"}`

	rec := postWithHost(h, "/api/comments", body, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("without X-Csrf-Token: status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	rec = postWithHost(h, "/api/comments", body, "wrong-token")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("with a wrong token: status = %d, want 403", rec.Code)
	}
	rec = postWithHost(h, "/api/comments", body, "the-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("with the correct token: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Security-Policy"); got == "" {
		t.Error("expected the CSP header on the /api/comments response reached via the web listener")
	}
}

// Template-source hygiene for fragments.tmpl (no inline handlers, no
// javascript: URLs) is covered by room_test.go's
// TestTemplatesHaveNoInlineEventHandlerAttributes, extended in WP4 to include
// this file in its list — one canonical check, not a second copy of it here.
