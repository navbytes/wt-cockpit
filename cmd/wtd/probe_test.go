package main

// This file is an independent verification pass over cmd/wtd, filling gaps
// left after the initial P2 handoff (see .claude/company/TASKS.md P2-T1):
// /api/version content-type, /api/status on a genuinely empty scan root, the
// SSE hello preamble under concurrent registry churn, and the AUDIT log line
// on both the approve/deny paths. It intentionally does not repeat coverage
// already in main_test.go (protocol/version equality, the hello-first single
// case, status field aggregation).

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/navbytes/wt-cockpit/internal/engine"
	"github.com/navbytes/wt-cockpit/internal/gitbackend"
	"github.com/navbytes/wt-cockpit/internal/guardrail"
	"github.com/navbytes/wt-cockpit/internal/model"
	"github.com/navbytes/wt-cockpit/internal/registry"
	"github.com/navbytes/wt-cockpit/internal/store"
)

// ---- /api/version ----

// TestHandleVersionSetsJSONContentType pins the response Content-Type header
// — every client decodes this body as JSON without sniffing, so the header
// must actually say so.
func TestHandleVersionSetsJSONContentType(t *testing.T) {
	srv, _ := buildTestServer(t)
	handler := srv.routes()

	req := httptest.NewRequest(http.MethodGet, "/api/version", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

// ---- /api/status on an empty root ----

// TestHandleStatusOnEmptyRootReportsZeroCounts: a freshly pointed-at root with
// no git repos at all (not merely no worktrees within a repo) must still
// answer with valid, all-zero counts and a non-negative uptime — not an
// error, a panic, or a nil-slice/omitted-field JSON quirk.
func TestHandleStatusOnEmptyRootReportsZeroCounts(t *testing.T) {
	root := t.TempDir() // deliberately no `git init` here: zero repos to discover

	reg := registry.New()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	be := gitbackend.NewCLIWithEnv(testGitEnv())
	gr := mustResolver(t, guardrail.DefaultRules())
	eng := engine.New(engine.Config{
		Roots:          []string{root},
		ActivityWindow: 30 * time.Second,
	}, be, reg, st, gr)
	if err := eng.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh on an empty root should not error, got: %v", err)
	}

	srv := &server{eng: eng, startedAt: time.Now()}
	handler := srv.routes()

	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got statusPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body not valid JSON: %v (body=%s)", err, rec.Body.String())
	}
	if got.RepoCount != 0 {
		t.Errorf("RepoCount = %d, want 0", got.RepoCount)
	}
	if got.WorktreeCount != 0 {
		t.Errorf("WorktreeCount = %d, want 0", got.WorktreeCount)
	}
	if got.ReviewedFiles != 0 {
		t.Errorf("ReviewedFiles = %d, want 0", got.ReviewedFiles)
	}
	if got.TotalFiles != 0 {
		t.Errorf("TotalFiles = %d, want 0", got.TotalFiles)
	}
	if got.UptimeSeconds < 0 {
		t.Errorf("UptimeSeconds = %v, want >= 0", got.UptimeSeconds)
	}
	if got.Protocol != model.ProtocolVersion {
		t.Errorf("Protocol = %d, want %d", got.Protocol, model.ProtocolVersion)
	}
}

// ---- SSE hello preamble under concurrency ----

// TestHandleEventsHelloAlwaysFirstUnderConcurrentRegistryChurn stress-tests
// the ordering main_test.go's TestHandleEventsEmitsHelloEventFirst already
// pins for a single request: handleEvents writes the "hello" frame (and
// flushes) before it ever calls Registry().Subscribe, so it must land first
// on the wire even while many requests subscribe concurrently and the
// registry is being hammered with upserts in the background — not just in
// the quiet, single-request case.
func TestHandleEventsHelloAlwaysFirstUnderConcurrentRegistryChurn(t *testing.T) {
	srv, _ := buildTestServer(t)
	handler := srv.routes()

	stop := make(chan struct{})
	var churnWG sync.WaitGroup
	churnWG.Add(1)
	go func() {
		defer churnWG.Done()
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			i++
			srv.eng.Registry().Upsert(model.Worktree{
				ID:    "churn",
				Repo:  "churn-repo",
				Name:  "churn",
				Stats: model.Stats{Files: i % 7},
			})
		}
	}()

	const concurrentSubscribers = 20
	var reqWG sync.WaitGroup
	for i := 0; i < concurrentSubscribers; i++ {
		reqWG.Add(1)
		go func() {
			defer reqWG.Done()
			// An already-cancelled context (same trick as the single-request
			// test): the handler writes hello+snapshot unconditionally, then
			// its for-select's ctx.Done() case fires, returning immediately —
			// deterministic, no goroutine leaks, no sleeps.
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			req := httptest.NewRequest(http.MethodGet, "/api/events", nil).WithContext(ctx)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			body := rec.Body.String()
			lines := strings.SplitN(body, "\n", 2)
			first := ""
			if len(lines) > 0 {
				first = lines[0]
			}
			if first != "event: hello" {
				t.Errorf("first SSE line = %q, want %q; body:\n%s", first, "event: hello", body)
			}
		}()
	}
	reqWG.Wait()
	close(stop)
	churnWG.Wait()
}

// ---- AUDIT log line on approve ----

// withJSONLogCapture installs a JSON slog handler as the package default for
// the duration of fn, restoring the previous default afterward (same
// save/restore convention as main_test.go's
// TestSlogSetDefaultBridgesStandardLogPackage), and returns everything
// logged.
func withJSONLogCapture(t *testing.T, fn func()) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	defer slog.SetDefault(prev)
	fn()
	return &buf
}

// findAuditLine scans buf (one JSON object per line, per slog's JSONHandler)
// for the "AUDIT approve" line and returns its fields, or nil if absent.
func findAuditLine(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	sc := bufio.NewScanner(buf)
	for sc.Scan() {
		var line map[string]any
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			continue
		}
		if line["msg"] == "AUDIT approve" {
			return line
		}
	}
	return nil
}

// TestHandleApproveLogsAuditLineOnDenial pins the audit trail on the refused
// path: handleApprove logs "AUDIT approve" with the worktree id and a
// "denied: ..." outcome even when Approve itself returns an error — the
// product's only mutation must leave a trail regardless of whether a gate
// stopped it. buildTestServer's fixture has an unreviewed pending change, so
// the review gate refuses it.
func TestHandleApproveLogsAuditLineOnDenial(t *testing.T) {
	srv, feat := buildTestServer(t)
	handler := srv.routes()

	buf := withJSONLogCapture(t, func() {
		rec := postJSON(t, handler, "/api/approve", map[string]any{"id": feat.ID})
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409 (Conflict); body=%s", rec.Code, rec.Body.String())
		}
	})

	line := findAuditLine(t, buf)
	if line == nil {
		t.Fatalf("no AUDIT approve line found in logs:\n%s", buf.String())
	}
	if line["worktree"] != feat.ID {
		t.Errorf("worktree = %v, want %q", line["worktree"], feat.ID)
	}
	outcome, _ := line["outcome"].(string)
	if !strings.HasPrefix(outcome, "denied:") {
		t.Errorf("outcome = %q, want it to start with %q", outcome, "denied:")
	}
}

// TestHandleApproveLogsAuditLineOnSuccess is the control case for the denial
// test above: the same AUDIT line, with outcome "ok", on the path where both
// approve gates (fully reviewed, clean tree) pass and the merge actually
// happens — proving the denial test is pinning a real branch, not the only
// branch.
func TestHandleApproveLogsAuditLineOnSuccess(t *testing.T) {
	srv, feat := buildTestServer(t)

	// Commit the fixture's pending change and mark every file reviewed against
	// its post-commit hash so both approve gates pass.
	wtPath, ok := srv.eng.WorktreePath(feat.ID)
	if !ok {
		t.Fatal("worktree path not found")
	}
	testGit(t, wtPath, "add", "-A")
	testGit(t, wtPath, "commit", "-q", "-m", "feature work")
	if err := srv.eng.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	d, ok := srv.eng.Diff(feat.ID)
	if !ok || len(d.Files) == 0 {
		t.Fatalf("precondition: diff must still have files after commit+refresh, got %+v", d)
	}
	for _, f := range d.Files {
		if err := srv.eng.SetReviewed(feat.ID, f.Path, true, f.Hash); err != nil {
			t.Fatalf("SetReviewed(%s): %v", f.Path, err)
		}
	}

	handler := srv.routes()
	buf := withJSONLogCapture(t, func() {
		rec := postJSON(t, handler, "/api/approve", map[string]any{"id": feat.ID})
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
	})

	line := findAuditLine(t, buf)
	if line == nil {
		t.Fatalf("no AUDIT approve line found in logs:\n%s", buf.String())
	}
	if line["worktree"] != feat.ID {
		t.Errorf("worktree = %v, want %q", line["worktree"], feat.ID)
	}
	if line["outcome"] != "ok" {
		t.Errorf("outcome = %v, want %q", line["outcome"], "ok")
	}
}

// TestHandleApproveOnMergeConflictKeepsRawDetailInAuditButFriendlyInBody is
// P7-ux.md P1-2's end-to-end pin over the real HTTP handler: a real merge
// conflict must produce a friendly, git-detail-free response body (what the
// CLI/web caller actually sees) while the AUDIT log line still retains git's
// raw conflict text for troubleshooting.
func TestHandleApproveOnMergeConflictKeepsRawDetailInAuditButFriendlyInBody(t *testing.T) {
	srv, feat := buildTestServer(t)

	wtPath, ok := srv.eng.WorktreePath(feat.ID)
	if !ok {
		t.Fatal("worktree path not found")
	}
	repo := filepath.Join(filepath.Dir(wtPath), "repo")

	// Conflict main and feature on the SAME (only) line of app.go.
	if err := os.WriteFile(filepath.Join(wtPath, "app.go"), []byte("package feat\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	testGit(t, wtPath, "add", "-A")
	testGit(t, wtPath, "commit", "-q", "-m", "feature edit")
	if err := os.WriteFile(filepath.Join(repo, "app.go"), []byte("package main2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	testGit(t, repo, "commit", "-qam", "main edit, conflicting")

	if err := srv.eng.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	d, ok := srv.eng.Diff(feat.ID)
	if !ok || len(d.Files) == 0 {
		t.Fatalf("precondition: diff must still have files, got %+v", d)
	}
	for _, f := range d.Files {
		if err := srv.eng.SetReviewed(feat.ID, f.Path, true, f.Hash); err != nil {
			t.Fatalf("SetReviewed(%s): %v", f.Path, err)
		}
	}

	handler := srv.routes()
	var rec *httptest.ResponseRecorder
	buf := withJSONLogCapture(t, func() {
		rec = postJSON(t, handler, "/api/approve", map[string]any{"id": feat.ID})
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "exit status") {
		t.Errorf("response body leaked git's raw exit-status text: %q", body)
	}
	if !strings.Contains(body, "conflicts with the base branch") {
		t.Errorf("response body = %q, want the friendly conflict message", body)
	}

	line := findAuditLine(t, buf)
	if line == nil {
		t.Fatalf("no AUDIT approve line found in logs:\n%s", buf.String())
	}
	outcome, _ := line["outcome"].(string)
	if !strings.Contains(outcome, "CONFLICT") {
		t.Errorf("AUDIT outcome = %q, want it to retain git's raw conflict detail", outcome)
	}
}
