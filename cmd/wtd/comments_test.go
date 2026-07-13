package main

// Handler-level tests for the three comments routes (P4-design.md §1.5):
// GET/POST /api/comments, POST /api/comments/resolve, POST
// /api/comments/delete. buildTestServer/postJSON come from main_test.go
// (same package) — its fixture "feature" worktree has "app.go" in the diff,
// which every test here comments against.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/navbytes/wt-cockpit/internal/model"
)

// getComments GETs /api/comments?<query> through the real mux and decodes
// the body when the status is 200 — the comments-list counterpart to
// main_test.go's getDiff, but also returning the recorder so error-status
// tests can inspect the response directly.
func getComments(t *testing.T, handler http.Handler, query string) (model.CommentsPayload, *httptest.ResponseRecorder) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/comments?"+query, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	var payload model.CommentsPayload
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decoding /api/comments response: %v (body=%s)", err, rec.Body.String())
		}
	}
	return payload, rec
}

// ---- POST /api/comments (create) ----

func TestHandleCommentsCreateReturns200AndTheCreatedComment(t *testing.T) {
	srv, feat := buildTestServer(t)
	handler := srv.routes()

	rec := postJSON(t, handler, "/api/comments", map[string]any{
		"id": feat.ID, "file": "app.go", "line": 1, "body": "why here?",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got model.Comment
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if got.ID == "" {
		t.Error("created comment has no ID")
	}
	if got.File != "app.go" || got.Body != "why here?" || got.WorktreeID != feat.ID {
		t.Errorf("created comment = %+v, unexpected fields", got)
	}
	if got.Side != "new" {
		t.Errorf("Side = %q, want default new", got.Side)
	}
	if got.State != "open" {
		t.Errorf("State = %q, want open", got.State)
	}
}

func TestHandleCommentsCreateAllowsLineZeroAndOldSide(t *testing.T) {
	srv, feat := buildTestServer(t)
	handler := srv.routes()

	rec := postJSON(t, handler, "/api/comments", map[string]any{
		"id": feat.ID, "file": "app.go", "line": 0, "side": "old", "body": "file-level, old side",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got model.Comment
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if got.Line != 0 || got.Side != "old" {
		t.Errorf("got = %+v, want line=0 side=old", got)
	}
}

func TestHandleCommentsCreateReturns404ForUnknownWorktree(t *testing.T) {
	srv, _ := buildTestServer(t)
	handler := srv.routes()

	rec := postJSON(t, handler, "/api/comments", map[string]any{
		"id": "no-such-id", "file": "app.go", "line": 1, "body": "hi",
	})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleCommentsCreateReturns404ForFileNotInDiff(t *testing.T) {
	srv, feat := buildTestServer(t)
	handler := srv.routes()

	rec := postJSON(t, handler, "/api/comments", map[string]any{
		"id": feat.ID, "file": "does-not-exist.go", "line": 1, "body": "hi",
	})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleCommentsCreateReturns400ForEmptyBody(t *testing.T) {
	srv, feat := buildTestServer(t)
	handler := srv.routes()

	rec := postJSON(t, handler, "/api/comments", map[string]any{
		"id": feat.ID, "file": "app.go", "line": 1, "body": "",
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleCommentsCreateReturns400ForBadSide(t *testing.T) {
	srv, feat := buildTestServer(t)
	handler := srv.routes()

	rec := postJSON(t, handler, "/api/comments", map[string]any{
		"id": feat.ID, "file": "app.go", "line": 1, "side": "sideways", "body": "hi",
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleCommentsCreateReturns400ForNegativeLine(t *testing.T) {
	srv, feat := buildTestServer(t)
	handler := srv.routes()

	rec := postJSON(t, handler, "/api/comments", map[string]any{
		"id": feat.ID, "file": "app.go", "line": -1, "body": "hi",
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// TestHandleCommentsCreateReturns413ForOversizedBody pins the body-size
// validation matrix's 413 case (P4-design.md §1.5): a 65 KiB comment body
// crosses the 64 KiB limit.
func TestHandleCommentsCreateReturns413ForOversizedBody(t *testing.T) {
	srv, feat := buildTestServer(t)
	handler := srv.routes()

	rec := postJSON(t, handler, "/api/comments", map[string]any{
		"id": feat.ID, "file": "app.go", "line": 1, "body": strings.Repeat("a", 65*1024),
	})
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleCommentsCreateReturns400ForMalformedJSON(t *testing.T) {
	srv, _ := buildTestServer(t)
	handler := srv.routes()

	req := httptest.NewRequest(http.MethodPost, "/api/comments", strings.NewReader("{not valid json"))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// ---- GET /api/comments (list) ----

func TestHandleCommentsListDefaultsToOpenOnly(t *testing.T) {
	srv, feat := buildTestServer(t)
	handler := srv.routes()

	postJSON(t, handler, "/api/comments", map[string]any{"id": feat.ID, "file": "app.go", "line": 1, "body": "open one"})
	rec2 := postJSON(t, handler, "/api/comments", map[string]any{"id": feat.ID, "file": "app.go", "line": 2, "body": "to be resolved"})
	var resolved model.Comment
	if err := json.Unmarshal(rec2.Body.Bytes(), &resolved); err != nil {
		t.Fatal(err)
	}
	if rec := postJSON(t, handler, "/api/comments/resolve", map[string]any{"id": feat.ID, "commentId": resolved.ID}); rec.Code != http.StatusOK {
		t.Fatalf("resolve: status = %d, body=%s", rec.Code, rec.Body.String())
	}

	payload, rec := getComments(t, handler, "id="+feat.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(payload.Comments) != 1 || payload.Comments[0].Body != "open one" {
		t.Errorf("default (open) list = %+v, want just the one still-open comment", payload.Comments)
	}
}

func TestHandleCommentsListAllIncludesResolved(t *testing.T) {
	srv, feat := buildTestServer(t)
	handler := srv.routes()

	rec1 := postJSON(t, handler, "/api/comments", map[string]any{"id": feat.ID, "file": "app.go", "line": 1, "body": "one"})
	var c1 model.Comment
	if err := json.Unmarshal(rec1.Body.Bytes(), &c1); err != nil {
		t.Fatal(err)
	}
	postJSON(t, handler, "/api/comments/resolve", map[string]any{"id": feat.ID, "commentId": c1.ID})

	payload, rec := getComments(t, handler, "id="+feat.ID+"&state=all")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if len(payload.Comments) != 1 || payload.Comments[0].State != "resolved" {
		t.Errorf("state=all list = %+v, want the resolved comment included", payload.Comments)
	}
}

func TestHandleCommentsListFiltersByFile(t *testing.T) {
	srv, feat := buildTestServer(t)
	handler := srv.routes()
	postJSON(t, handler, "/api/comments", map[string]any{"id": feat.ID, "file": "app.go", "line": 1, "body": "on app.go"})

	payload, rec := getComments(t, handler, "id="+feat.ID+"&file=other.go")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if len(payload.Comments) != 0 {
		t.Errorf("file filter should exclude non-matching comments, got %+v", payload.Comments)
	}
}

func TestHandleCommentsListIncludesWorktreePathBranchBase(t *testing.T) {
	srv, feat := buildTestServer(t)
	handler := srv.routes()

	payload, rec := getComments(t, handler, "id="+feat.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if payload.WorktreeID != feat.ID || payload.Path != feat.Path || payload.Branch != feat.Branch || payload.Base != feat.Base {
		t.Errorf("payload = %+v, want worktreeId/path/branch/base to mirror the registry's worktree %+v", payload, feat)
	}
}

func TestHandleCommentsListReturns404ForUnknownWorktree(t *testing.T) {
	srv, _ := buildTestServer(t)
	handler := srv.routes()

	_, rec := getComments(t, handler, "id=no-such-id")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleCommentsListReturns400ForInvalidState(t *testing.T) {
	srv, feat := buildTestServer(t)
	handler := srv.routes()

	_, rec := getComments(t, handler, "id="+feat.ID+"&state=bogus")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// ---- POST /api/comments/resolve, /api/comments/delete ----

func TestHandleCommentsResolveReturns200AndFlipsState(t *testing.T) {
	srv, feat := buildTestServer(t)
	handler := srv.routes()

	rec1 := postJSON(t, handler, "/api/comments", map[string]any{"id": feat.ID, "file": "app.go", "line": 1, "body": "resolve me"})
	var c model.Comment
	if err := json.Unmarshal(rec1.Body.Bytes(), &c); err != nil {
		t.Fatal(err)
	}

	rec := postJSON(t, handler, "/api/comments/resolve", map[string]any{"id": feat.ID, "commentId": c.ID})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	payload, _ := getComments(t, handler, "id="+feat.ID+"&state=all")
	if len(payload.Comments) != 1 || payload.Comments[0].State != "resolved" {
		t.Errorf("after resolve, list = %+v, want state=resolved", payload.Comments)
	}
}

func TestHandleCommentsResolveReturns404ForUnknownCommentID(t *testing.T) {
	srv, feat := buildTestServer(t)
	handler := srv.routes()

	rec := postJSON(t, handler, "/api/comments/resolve", map[string]any{"id": feat.ID, "commentId": "c-doesnotexist"})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleCommentsResolveReturns404ForUnknownWorktree(t *testing.T) {
	srv, _ := buildTestServer(t)
	handler := srv.routes()

	rec := postJSON(t, handler, "/api/comments/resolve", map[string]any{"id": "no-such-id", "commentId": "c-anything"})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleCommentsDeleteReturns200AndRemovesComment(t *testing.T) {
	srv, feat := buildTestServer(t)
	handler := srv.routes()

	rec1 := postJSON(t, handler, "/api/comments", map[string]any{"id": feat.ID, "file": "app.go", "line": 1, "body": "delete me"})
	var c model.Comment
	if err := json.Unmarshal(rec1.Body.Bytes(), &c); err != nil {
		t.Fatal(err)
	}

	rec := postJSON(t, handler, "/api/comments/delete", map[string]any{"id": feat.ID, "commentId": c.ID})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	payload, _ := getComments(t, handler, "id="+feat.ID+"&state=all")
	if len(payload.Comments) != 0 {
		t.Errorf("comment should be gone after delete, got %+v", payload.Comments)
	}
}

func TestHandleCommentsDeleteReturns404ForUnknownCommentID(t *testing.T) {
	srv, feat := buildTestServer(t)
	handler := srv.routes()

	rec := postJSON(t, handler, "/api/comments/delete", map[string]any{"id": feat.ID, "commentId": "c-doesnotexist"})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// ---- SSE: comment.changed over the real handler surface ----

// TestHandleCommentsCreateAndResolvePublishCommentChangedOverTheRealHandler
// proves the comment.changed event fires from the actual HTTP handler path
// (internal/engine's own tests already pin the engine-level contract) —
// exactly one event per mutation, asserted directly on the bus (as WP1's
// brief allows as an alternative to reading the SSE wire format).
func TestHandleCommentsCreateAndResolvePublishCommentChangedOverTheRealHandler(t *testing.T) {
	srv, feat := buildTestServer(t)
	handler := srv.routes()

	ch, cancel := srv.eng.Registry().Subscribe(8)
	defer cancel()

	rec := postJSON(t, handler, "/api/comments", map[string]any{"id": feat.ID, "file": "app.go", "line": 1, "body": "note"})
	if rec.Code != http.StatusOK {
		t.Fatalf("create: status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var c model.Comment
	if err := json.Unmarshal(rec.Body.Bytes(), &c); err != nil {
		t.Fatal(err)
	}
	assertNextEventIsCommentChanged(t, ch)

	rec2 := postJSON(t, handler, "/api/comments/resolve", map[string]any{"id": feat.ID, "commentId": c.ID})
	if rec2.Code != http.StatusOK {
		t.Fatalf("resolve: status = %d, body=%s", rec2.Code, rec2.Body.String())
	}
	assertNextEventIsCommentChanged(t, ch)
}

func assertNextEventIsCommentChanged(t *testing.T, ch <-chan model.Event) {
	t.Helper()
	select {
	case ev := <-ch:
		if ev.Type != model.EventCommentChanged {
			t.Errorf("event = %+v, want type %q", ev, model.EventCommentChanged)
		}
	default:
		t.Error("expected a comment.changed event to already be buffered")
	}
}
